# -*- coding: utf-8 -*-
"""上传核心：本地清单、manifest.json/_READY 提交、SDK 逐文件上传、ossutil 整夹上传、按清单顺序逐文件夹上传。"""
import collections
import concurrent.futures as cf
import hashlib
import json
import os
import re
import shlex
import shutil
import stat
import subprocess
import tempfile
import threading
import time
import upload_umi_sdk as sdk
from .lifecycle import register_child, unregister_child
from .ossutil_client import looks_network, wait_network
from .state import CKPT_DIR, PL, STATE_DIR
from .progress import add_sent, hash_file, log


# ------------------------------------------------------------ 本地清单 / 提交
def inventory(src):
    out = []
    for root, _, files in os.walk(src):
        for f in files:
            fp = os.path.join(root, f); rel = os.path.relpath(fp, src).replace(os.sep, '/')
            if sdk.is_excluded(rel):
                continue
            st = os.lstat(fp)
            if not stat.S_ISREG(st.st_mode):
                raise sdk.UploadError('不支持的文件类型(符号链接等): ' + fp)
            out.append((rel, fp, st))
    if not out:
        raise sdk.UploadError('数据集为空: %s' % src)
    return out


def commit(oss, ds, files):
    """复核远端清单 → 写 manifest.json → 写 _READY（与 SDK 一致）。返回(状态, 版本号)。"""
    raw, manifest = ds['raw_uri'], sdk.make_manifest(ds, files)
    sdk.verify_inventory(oss, raw, files)
    cc = sdk.remote_commit(oss, raw)
    if cc:
        if not sdk.same_content(cc['manifest'], manifest):
            raise sdk.UploadError('其他上传进程已提交不同版本，请勿同时上传同一数据集')
        return '已存在', cc['manifest']['revision']
    mb = (json.dumps(manifest, ensure_ascii=False, indent=2) + '\n').encode('utf-8')
    oss.put_object(raw + 'manifest.json', mb)
    if oss.read_object(raw + 'manifest.json') != mb:
        raise sdk.UploadError('远端清单校验失败，未发送完成标记')
    marker = {'schema_version': 1, 'revision': manifest['revision'], 'manifest_sha256': hashlib.sha256(mb).hexdigest()}
    oss.put_object(raw + '_READY', (json.dumps(marker, indent=2) + '\n').encode('utf-8'))
    ok = sdk.remote_commit(oss, raw)
    if not ok or ok['ready']['manifest_sha256'] != marker['manifest_sha256']:
        raise sdk.UploadError('完成标记确认失败，请重试以确认提交状态')
    return '成功', manifest['revision']


def put_file(oss, local, key, size, job):
    last = [0]

    def cb(c, _t=None):
        if oss.is_s3: add_sent(job, c); last[0] += c
        elif c >= last[0]: add_sent(job, c - last[0]); last[0] = c

    def attempt():
        add_sent(job, -last[0]); last[0] = 0               # 重试时回退已计入的字节，避免进度超 100%
        if oss.is_s3:
            oss.boto3_client.upload_file(local, oss.bucket_name, key, Config=oss.transfer_config, Callback=cb)
        else:
            oss.oss2.resumable_upload(oss.bucket, key, local, store=oss.store, multipart_threshold=sdk.MULTIPART_THRESHOLD,
                                      part_size=sdk.PART_SIZE, progress_callback=cb, num_threads=oss.parallel)
    oss._retry(attempt)
    add_sent(job, size - last[0])


def ckpt_root(ds):
    """该数据集的断点目录（持久化，数据集提交成功后删除）。里面每个上传单元一个子目录：ossutil 的 checkpoint + started 标记。"""
    return os.path.join(CKPT_DIR, hashlib.sha256(ds['raw_uri'].encode()).hexdigest()[:16])


def net_cb(job):
    """断网等待的状态回调：进度条阶段显示「网络中断，等待恢复」，日志记录中断/恢复。"""
    p, st = job['prog'], {}
    def cb(ev, waited=0.0, err=''):
        if ev == 'down': st['phase'] = p['phase']; log(job, '   ⚠ 网络中断，等待恢复后自动续传: ' + ' '.join(err.split())[:160]); p['phase'] = '⚠ 网络中断，等待恢复后自动续传…'
        elif ev == 'tick': p['raw'] = '网络中断，已等待 %d 秒（恢复后自动续传）' % waited
        else:
            p['phase'], p['raw'] = st.get('phase') or p['phase'], ''
            log(job, '   ✔ 网络已恢复（等待 %d 秒），继续上传' % waited if ev == 'up' else '   ✘ 网络中断超过等待上限，放弃本次')
    return cb


# ossutil v1.7.19 进度行：Total num: N, size: X. Dealed num: N, Progress: 25.123%, Speed: 1234.56KB/s
PROG_RE = re.compile(r'Progress:\s*([\d.]+)\s*%')


SPD_RE = re.compile(r'Speed:\s*([\d.]+)\s*KB/s')


DEAL_RE = re.compile(r'Dealed num:\s*(\d+)')


PCT_RE = re.compile(r'(\d{1,3}(?:\.\d+)?)\s*%')                  # 其他版本的兜底


def has_excluded_local(src):
    """本地目录里是否有 _pipeline / manifest.json / _READY 等排除项：ossutil 整夹上传会把它们一起传上去，此时该数据集改用 SDK 逐文件上传。"""
    for root, dirs, files in os.walk(src):
        for n in dirs + files:
            if sdk.is_excluded(os.path.relpath(os.path.join(root, n), src).replace(os.sep, '/')):
                return True
    return False


def check_unit(oss, target, expect, exact):
    """上传一个文件夹后立刻用远端列表核对（文件名+大小）。exact=True 时远端不得有多余文件。"""
    remote = {f['name']: f['size'] for f in oss.list_objects(target) if not sdk.is_excluded(f['name'])}
    bad = [k for k, v in expect.items() if remote.get(k) != v]
    extra = [k for k in remote if k not in expect] if exact else []
    if bad or extra:
        raise sdk.UploadError('文件夹上传后远端与本地不一致 %s：缺失/大小不符 %d 个(如 %s)，多余 %d 个(如 %s)。可能 ossutil 目录语义与预期不同，请改用 SDK 方式或检查 ossutil 版本'
                              % (target, len(bad), (bad or ['-'])[0], len(extra), (extra or ['-'])[0]))


def ossutil_unit(job, c, ds, top, files, only_cur=None, single=None):
    """一次只运行一个 ossutil 进程：cp -r <本地文件夹/> <OSS 文件夹/> --jobs --parallel。top='' 且 only_cur=True 为会话根目录文件，top='' 且 only_cur=False 为整个会话。
    进度：解析 ossutil v1.7.19 的 Progress / Speed / Dealed num，结束后按大小补足；退出码非 0 或 .report 非空即失败：
    · 像网络问题（超时/断连/DNS）→ 等网络恢复（最长 net_wait 分钟，探测间隔 5→60 秒）后续传，不消耗重试次数；· 其他错误 → 自动重试 2 次。
    续传 = 持久的 --checkpoint-dir（大文件按分片接着传）+ --update（已传到 OSS 的文件跳过）。"""
    p, size = job['prog'], sum(st.st_size for _, _, st in files)
    only_cur = (not top) if only_cur is None else only_cur
    local = os.path.join(str(ds['source']), top) if top else str(ds['source'])
    target = ds['raw_uri'] + (top + '/' if top else '')
    if single:                                               # 单个文件（本地含 _pipeline 等排除项时逐文件上传）
        local, target = os.path.join(str(ds['source']), *single.split('/')), ds['raw_uri'] + single
    extra = re.sub(r'--(?:jobs|parallel)(?:=|\s+)\S+', '', c['ossutil_args'] or '')
    sig = hashlib.sha256('\n'.join('%s:%d:%d' % (r, st.st_size, st.st_mtime_ns) for r, _, st in files).encode()).hexdigest()
    ck = os.path.join(ckpt_root(ds), hashlib.sha256((target + '\0' + sig).encode()).hexdigest()[:24]); os.makedirs(ck, exist_ok=True)
    mark = os.path.join(ck, 'started'); redo = os.path.exists(mark)        # 有 started = 上次这个单元（同样的本地内容）没传完：断网/重启后的续传
    open(mark, 'w').close()
    budget, waited, tries, nets = float(c.get('net_wait') or 0) * 60, 0.0, 0, 0
    sent, tail, f0, cb = [0], collections.deque(maxlen=4), p['files'], net_cb(job)
    if redo: log(job, '   断点续传：上次未传完，已传到 OSS 的文件会跳过（--update），大文件按 checkpoint 接着传')
    while True:
        rep = tempfile.mkdtemp(prefix='rep-', dir=STATE_DIR)
        cmd = [c['ossutil_path'], 'cp', local if single else local + os.sep, target] + ([] if single else ['-r']) + ['-f', '--config-file', c['_cfg'], '--disable-ignore-error', '--output-dir=' + rep, '--checkpoint-dir=' + ck,
               '--jobs=%s' % (c['jobs'] or 16), '--parallel=%s' % (c['parallel'] or 4), '--bigfile-threshold=%d' % sdk.MULTIPART_THRESHOLD, '--part-size=%d' % sdk.PART_SIZE] \
              + (['--only-current-dir'] if only_cur else []) + (['--update'] if redo else []) + shlex.split(extra)
        if tries == 0 and nets == 0: log(job, '   ossutil: %s → %s' % (local, target))
        add_sent(job, -sent[0]); sent[0] = 0; p['files'] = f0; tail.clear()
        try:
            proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, bufsize=0, creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
        except OSError as e:
            raise sdk.UploadError('无法启动 ossutil(%s): %s' % (c['ossutil_path'], e))
        register_child(proc)
        buf = b''
        while True:
            chunk = os.read(proc.stdout.fileno(), 4096)
            if not chunk: break
            buf += chunk; *segs, buf = re.split(rb'[\r\n]', buf)
            for seg in segs:
                t = seg.decode('utf-8', 'replace').strip()
                if not t: continue
                m, sp, dn = PROG_RE.search(t) or PCT_RE.search(t), SPD_RE.search(t), DEAL_RE.search(t)
                if m or sp:
                    p['raw'] = t[:160]
                    if m:
                        d = int(size * min(float(m.group(1)), 100) / 100) - sent[0]
                        if d > 0: add_sent(job, d); sent[0] += d
                    if sp: p['rep_speed'] = float(sp.group(1)) * 1024
                    if dn: p['files'] = max(p['files'], min(f0 + int(dn.group(1)), f0 + len(files)))
                else: tail.append(t[:200])
        rc = proc.wait(); unregister_child(proc)
        rtxt = ''.join(open(os.path.join(rep, f), encoding='utf-8', errors='replace').read(4000) for f in os.listdir(rep) if f.endswith('.report'))
        ok = rc == 0 and not rtxt.strip()
        shutil.rmtree(rep, True)
        if ok:
            add_sent(job, size - sent[0]); p['rep_speed'] = None; p['files'] = f0 + len(files); return
        err = ' | '.join(tail) + ' ' + rtxt
        if budget and looks_network(err) and nets < 50:           # 网络中断：不消耗普通重试次数，等网络恢复后续传（已传完的文件 --update 跳过，大文件接着 checkpoint）
            nets += 1; p['rep_speed'] = None
            w = wait_network(c['ossutil_path'], c['_cfg'], c['bucket'], budget - waited, cb, err)
            if w is None: raise sdk.UploadError('网络中断超过 %g 分钟仍未恢复，已保留断点，网络恢复后重新上传即可续传: %s' % (budget / 60, ' '.join(err.split())[:200]))
            waited += w; redo = True; continue
        tries += 1; redo = True
        if tries >= 3: break
        log(job, '   ossutil 失败，重试(%d/2): %s' % (tries, ' | '.join(tail))); time.sleep(2 ** (tries - 1))
    raise sdk.UploadError('ossutil 上传失败 %s: %s' % (target, ' | '.join(tail) or '见失败报告'))


class AheadHasher(threading.Thread):
    """按清单顺序单线程算 sha256（供 manifest），但最多领先已上传字节 AHEAD：既不占用上传进程，也避免把整批文件提前读一遍
    （磁盘被读两遍、系统缓存放不下）——哈希总是趁文件还在缓存里进行。"""
    AHEAD = 256 * 1024 * 1024

    def __init__(self, job, inv):
        super().__init__(daemon=True); self.job, self.inv, self.res, self.err, self.stop = job, inv, {}, None, False

    def run(self):
        p = self.job['prog']
        try:
            for r, fp, st in self.inv:
                while not self.stop and p['hashed'] - p['sent'] > self.AHEAD: time.sleep(0.05)
                if self.stop: return
                self.res[r] = hash_file(fp, self.job)
        except Exception as e:
            self.err = e

    def result(self):
        self.join()
        if self.err: raise self.err
        return self.res


def ossutil_ordered(job, oss, ds, inv, groups, c, per_file=False):
    """ossutil 方式，一次只运行一个进程。
       unit=slice（默认）：按清单顺序逐个文件夹（各分片 _N/，根目录文件单独一组）；每个文件夹传完立即按远端列表核对。严格有序，但每个文件夹有进程启动/收尾开销。
       unit=session：先用会话根目录文件（2 个小文件）做一次目录语义预检，再用单个进程上传整个会话目录——没有文件夹之间的空档，带宽利用最高；
                     ossutil 自行按目录字典序启动文件（与清单顺序一致，但完成顺序交错），整体由 commit() 复核。
       per_file：本地含 _pipeline/manifest.json/_READY 等排除项时，整夹上传会把它们一起传上去，改为逐文件上传（仍是单进程，按清单顺序）。
    全部传完后由 commit() 复核远端列表，并通过 ossutil 写入 manifest.json → _READY。"""
    p, raw = job['prog'], ds['raw_uri']
    root = [inv[i] for t, ix in groups for i in ix if t == '']
    E = lambda **k: dict(dict(top='', files=[], only_cur=False, exact=None, title='', single=None, whole=False), **k)
    if per_file:
        plan = [E(files=[it], title=it[0], single=it[0]) for it in inv]
    elif c['ossutil_unit'] == 'session':
        plan = ([E(files=root, only_cur=True, exact=False, title='会话根目录文件（目录语义预检）')] if root else []) + [E(files=inv, title='整个会话（单进程）', whole=True)]
    else:
        plan = [E(top=t, files=[inv[i] for i in ix], only_cur=not t, exact=bool(t), title=(raw + t + '/') if t else '会话根目录文件') for t, ix in groups]
    p['ftotal'], p['fdone'] = len(plan), 0
    left = collections.Counter(r.split('/', 1)[0] if '/' in r else '' for r, _, _ in inv)       # 逐文件模式：按第一级目录汇报进度
    if per_file: log(job, '   ▶ 逐文件上传（本地含 _pipeline/manifest.json 等排除项，不能整夹上传）：%d 个文件' % len(inv))
    hasher = AheadHasher(job, inv); hasher.start()
    try:
        for k, e in enumerate(plan):
            files, t0 = e['files'], time.time()
            size = sum(st.st_size for _, _, st in files)
            if not per_file: log(job, '   ▶ 文件夹 [%d/%d] %s  %d 个文件 %s' % (k + 1, len(plan), e['title'], len(files), sdk.format_size(size)))
            with PL:
                p['file'] = e['title'] or '（根目录文件）'
                if e['whole']: p['files'] = 0                      # 整会话：预检用的根目录文件不重复计数
            ossutil_unit(job, c, ds, e['top'], files, e['only_cur'], e['single'])
            if e['exact'] is not None:
                tgt = raw + (e['top'] + '/' if e['top'] else '')
                check_unit(oss, tgt, {(r[len(e['top']) + 1:] if e['top'] else r): st.st_size for r, _, st in files}, exact=e['exact'])
            dt = max(time.time() - t0, 0.001); job['last_speed'] = size / dt
            with PL: p['fdone'] += 1
            if per_file:
                top = e['single'].split('/', 1)[0] if '/' in e['single'] else ''; left[top] -= 1
                if not left[top]: log(job, '   ✔ 文件夹 %s%s 完成' % (raw, (top + '/') if top else '（根目录文件）'))
            else: log(job, '   ✔ 文件夹 [%d/%d] 完成  用时 %.1fs  平均 %s/s' % (k + 1, len(plan), dt, sdk.format_size(size / dt)))
        for r, fp, st in inv:
            if sdk._signature(st) != sdk._signature(os.lstat(fp)): raise sdk.UploadError('文件在上传过程中变化: ' + fp)
        res = hasher.result()
        return [{'path': r, 'size': st.st_size, 'sha256': res[r]} for r, _, st in inv]
    finally:
        hasher.stop = True


def upload_one(job, oss, ds, item):
    rel, path, st = item
    with PL: job['prog']['file'] = rel
    sha = hash_file(path, job)                                  # 哈希与上传单趟完成
    put_file(oss, path, oss._key(ds['raw_uri']) + rel, st.st_size, job)
    if sdk._signature(st) != sdk._signature(os.lstat(path)):
        raise sdk.UploadError('文件在上传过程中变化: ' + path)
    with PL: job['prog']['files'] += 1
    return {'path': rel, 'size': st.st_size, 'sha256': sha}


def upload_ordered(job, oss, ds, inv, c):
    """按 manifest.json 里 files 的顺序（路径 UTF-16 排序）逐文件夹上传。「文件夹」= 会话下的第一级目录（各分片 _N/），会话根目录文件单独一组，
    按各自在清单中首次出现的先后排列；manifest.json / _READY 在全部传完并复核后由 commit() 最后写入。
      ossutil：每个文件夹一个 ossutil 进程（一次只运行一个，并发由 ossutil 的 --jobs 负责），后台同步哈希；
      SDK：文件夹内按 jobs 并发；order=folder 上个文件夹传完再传下一个，order=pipeline 按清单顺序流水线（不等收尾）。"""
    inv, raw, p = sorted(inv, key=lambda i: sdk.utf16_key(i[0])), ds['raw_uri'], job['prog']
    units = collections.OrderedDict()
    for idx, (rel, _, _) in enumerate(inv):
        units.setdefault(rel.split('/', 1)[0] if '/' in rel else '', []).append(idx)
    groups = list(units.items())
    p['ftotal'], p['fdone'] = len(groups), 0
    if c['method'] == 'ossutil':
        excl = has_excluded_local(ds['source'])
        if excl: log(job, '   ⚠ 本地含 _pipeline/manifest.json 等排除项，整夹上传会把它们一起传上去，本数据集改为 ossutil 逐文件上传')
        return ossutil_ordered(job, oss, ds, inv, groups, c, per_file=excl)
    res, stop, started, nxt = [None] * len(inv), threading.Event(), set(), [0]
    label = lambda g: raw + (g[0] + '/' if g[0] else '（根目录文件）')

    def work(gi, i):
        if stop.is_set(): return
        with PL:
            first = gi not in started; started.add(gi)
        if first: log(job, '   ▶ 文件夹 [%d/%d] %s  %d 个文件 %s' % (gi + 1, len(groups), label(groups[gi]), len(groups[gi][1]), sdk.format_size(sum(inv[k][2].st_size for k in groups[gi][1]))))
        try: res[i] = upload_one(job, oss, ds, inv[i])
        except Exception: stop.set(); raise
        with PL:
            while nxt[0] < len(groups) and all(res[k] for k in groups[nxt[0]][1]):
                p['fdone'] += 1; nxt[0] += 1
                log(job, '   ✔ 文件夹 [%d/%d] %s  完成' % (nxt[0], len(groups), label(groups[nxt[0] - 1])))
    with cf.ThreadPoolExecutor(max(1, int(c['jobs'] or 16))) as pool:
        if c['order'] == 'pipeline':
            list(pool.map(lambda t: work(*t), [(gi, i) for gi, g in enumerate(groups) for i in g[1]]))
        else:
            for gi, g in enumerate(groups):
                for f in [pool.submit(work, gi, i) for i in g[1]]: f.result()
    return res


def do_upload(job, oss, rec, ds, c):
    p, raw = job['prog'], ds['raw_uri']
    inv = inventory(ds['source'])
    p.update(total=sum(st.st_size for *_, st in inv), total_files=len(inv), phase='检查远端')
    ex = sdk.remote_commit(oss, raw)
    if ex:                                                       # 已提交：只比对大小，不再哈希/上传
        if {f['path']: f['size'] for f in ex['manifest']['files']} != {r: st.st_size for r, _, st in inv}:
            raise sdk.UploadError('同一路径已提交过不同内容，拒绝覆盖: ' + raw)
        sdk.verify_inventory(oss, raw, [{'path': r, 'size': st.st_size} for r, _, st in inv])
        log(job, '   远端已提交且文件大小一致，跳过上传'); shutil.rmtree(ckpt_root(ds), True)
        return '已存在', ex['manifest']['revision'], len(inv)
    p['phase'] = '上传中（ossutil · %s单进程）' % ('整会话' if c['ossutil_unit'] == 'session' else '逐分片') if c['method'] == 'ossutil' else '上传中（SDK · %s）' % ('逐文件夹' if c['order'] != 'pipeline' else '顺序流水线')
    files = upload_ordered(job, oss, ds, inv, c)
    p['phase'] = '复核并提交（manifest.json → _READY）'
    log(job, '   复核完成后写入 manifest.json 与 _READY')
    st, rev = commit(oss, ds, files)
    shutil.rmtree(ckpt_root(ds), True)                           # 已提交：断点不再需要
    return st, rev, len(files)
