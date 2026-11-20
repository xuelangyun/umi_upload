# -*- coding: utf-8 -*-
"""ossutil 客户端：与 sdk.Oss 接口兼容（list_objects / read_object / put_object）。
方式选 ossutil 时，所有 OSS 操作（上传文件、列表、读/写 manifest.json 与 _READY、校验、连接测试）都走 ossutil，不需要 oss2 / boto3。
命令与官方客户端 ossutil.js 一致：ls 长格式解析、cp 读（NoSuchKey → None）、cp 写（临时文件 + 失败报告检查）。"""
import os
import re
import shutil
import subprocess
import tempfile
import time
import upload_umi_sdk as sdk
from .state import STATE_DIR
from .lifecycle import register_child, unregister_child

LS_RE = re.compile(r'^(.+?)\s+(\d+)\s+([A-Za-z_]+)\s+([0-9A-Fa-f]{32}(?:-\d+)?)\s+(.+)$')      # LastModified  Size  StorageClass  ETag  oss://桶/对象
_strip = lambda u: re.sub(r'^oss://[^/]+/', '', u)


NET_RE = re.compile(r'dial tcp|i/o timeout|timed out|timeout|no such host|connection (?:reset|refused|closed|aborted)|forcibly closed|unexpected EOF|\bEOF\b|broken pipe|'
                    r'network is unreachable|connectex|wsarecv|wsasend|TLS handshake|deadline exceeded|temporary failure|name resolution|RequestTimeout|ServiceUnavailable|SlowDown', re.I)
PROBE_MIN, PROBE_MAX = 5.0, 60.0                         # 断网后每隔多久探测一次：5 秒起步，逐次加倍，最长 60 秒


def looks_network(text):
    """ossutil 的错误输出是否像网络问题（超时/断连/DNS/连接被重置…）。认证错误(AccessDenied)、本地文件错误等不算，仍走普通的 3 次重试。"""
    return bool(NET_RE.search(text or ''))


def net_up(exe, cfg, bucket):
    """探测网络：ossutil ls -d 能返回就算通；返回了认证错误等「非网络」错误也说明网络是通的。超时/断连 → False。"""
    try:
        p = subprocess.Popen([exe, 'ls', 'oss://%s/' % bucket, '-d', '--limited-num=1', '--retry-times=1', '--config-file', cfg], stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                             creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
    except OSError:
        return False
    register_child(p)
    try:
        out = p.communicate(timeout=45)[0].decode('utf-8', 'replace')
    except subprocess.TimeoutExpired:
        p.kill(); p.communicate(); return False
    finally:
        unregister_child(p)
    return p.returncode == 0 or not looks_network(out)


def wait_network(exe, cfg, bucket, budget, cb=None, err=''):
    """断网时阻塞，直到网络恢复。返回已等待秒数；等待超过 budget 秒仍未恢复返回 None。cb(事件, 已等秒数, 错误)：down 开始等待 / tick 每次探测后 / up 恢复 / giveup 放弃。"""
    cb = cb or (lambda *a: None)
    cb('down', 0.0, err); t0, gap = time.time(), PROBE_MIN
    while True:
        left = budget - (time.time() - t0)
        if left <= 0: cb('giveup', time.time() - t0, err); return None
        time.sleep(min(gap, max(left, 0.05))); gap = min(gap * 2, PROBE_MAX)
        w = time.time() - t0
        if net_up(exe, cfg, bucket): cb('up', w, err); return w
        cb('tick', w, err)


def write_ossutil_cfg(c):
    """与官方客户端一致：用 ossutil 原生配置文件传 endpoint/AK/SK（不上命令行）；参数取自页面配置，文件放临时目录、退出即删。"""
    p = os.path.join(STATE_DIR, 'ossutil.cfg')
    with open(p, 'w', encoding='utf-8') as f:
        f.write('[Credentials]\nlanguage=EN\nendpoint=%s\naccessKeyID=%s\naccessKeySecret=%s\n' % (c['endpoint'], c['ak'], c['sk']))
    os.chmod(p, 0o600)
    return p


class OssutilOss:
    is_s3 = False

    def __init__(self, c):
        self.exe, self.cfg, self.bucket = c['ossutil_path'], c['_cfg'], c['bucket']
        self.net_wait, self.on_net = float(c.get('net_wait') or 0) * 60, None      # 断网最长等待秒数（0=不等待）、状态回调（jobs 设置，用于进度/日志）

    def _run(self, args, retries=3):
        """运行 ossutil，返回输出文本；失败自动重试 retries 次（确定性错误 NoSuchKey 不重试）。
        网络中断（输出像超时/断连）时不消耗重试次数：等待网络恢复（最长 net_wait 分钟）后重试同一条命令，写 manifest.json/_READY 等收尾操作也因此能熬过断网。"""
        last, rc, k, waited, nets = '', 0, 0, 0.0, 0
        while True:
            try:
                p = subprocess.Popen([self.exe] + args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
            except OSError as e:
                raise sdk.UploadError('无法启动 ossutil(%s): %s' % (self.exe, e))
            register_child(p)
            try: out = p.communicate()[0].decode('utf-8', 'replace')
            finally: unregister_child(p)
            rc = p.returncode
            if rc == 0: return out
            last = out.strip()
            if 'NoSuchKey' in last: break
            if self.net_wait and looks_network(last) and nets < 50:
                nets += 1; w = wait_network(self.exe, self.cfg, self.bucket, self.net_wait - waited, self.on_net, last[-200:])
                if w is None: raise sdk.UploadError('网络中断超过 %g 分钟仍未恢复: %s' % (self.net_wait / 60, last[-300:]))
                waited += w; continue
            k += 1
            if k >= retries: break
            time.sleep(2 ** (k - 1))
        raise sdk.UploadError('ossutil 退出码 %d: %s' % (rc, last[-400:]))

    def test(self):
        self.net_wait = 0                                         # 连接测试要立刻给出结果，不等待网络恢复
        self._run(['ls', 'oss://%s/' % self.bucket, '-d', '--limited-num=1', '--config-file', self.cfg], retries=1)

    def list_objects(self, uri):
        out = self._run(['ls', uri, '--config-file', self.cfg])
        prefix, files = _strip(uri), []
        for line in out.splitlines():
            m = LS_RE.match(line.strip())
            if not m: continue
            rel = _strip(m.group(5).strip()); name = rel[len(prefix):] if prefix and rel.startswith(prefix) else rel
            if name and not name.endswith('/'): files.append({'name': name, 'size': int(m.group(2))})
        known = {uri + f['name'] for f in files}                 # 输出格式变化时不能把解析失败当成空目录（与官方客户端一致）
        for line in out.splitlines():
            t = line.strip()
            if uri in t and not t.endswith('/') and t[t.index(uri):].strip() not in known:
                raise sdk.UploadError('无法完整解析 OSS 文件列表，停止提交: ' + t[:200])
        return files

    def read_object(self, uri):
        tmp = tempfile.mkdtemp(prefix='oss-read-', dir=STATE_DIR)
        try:
            local = os.path.join(tmp, 'object')
            try: self._run(['cp', uri, local, '--config-file', self.cfg, '-f'])
            except sdk.UploadError as e:
                if re.search(r'\bNoSuchKey\b', str(e)): return None
                raise
            return open(local, 'rb').read()
        finally:
            shutil.rmtree(tmp, True)

    def put_object(self, uri, data):
        tmp = tempfile.mkdtemp(prefix='oss-put-', dir=STATE_DIR)
        try:
            local, rep = os.path.join(tmp, 'object'), os.path.join(tmp, 'rep'); os.makedirs(rep)
            open(local, 'wb').write(data)
            self._run(['cp', local, uri, '--config-file', self.cfg, '-f', '--disable-ignore-error', '--output-dir=' + rep])
            if any(f.endswith('.report') and os.path.getsize(os.path.join(rep, f)) > 0 for f in os.listdir(rep)):
                raise sdk.UploadError('OSS 复制存在失败记录: ' + uri)
        finally:
            shutil.rmtree(tmp, True)
