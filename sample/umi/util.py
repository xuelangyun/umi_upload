# -*- coding: utf-8 -*-
"""小工具：文本读取（编码容错）、时间格式、OSS 路径拼接（rel ↔ 全路径）、ossutil 路径解析。"""
import datetime
import os
import re
import subprocess
import sys
from pathlib import Path
import upload_umi_sdk as sdk
from .state import HERE, TZ


ROOM_OK = re.compile(r'^PRT_[A-Za-z]+_room\d{3}$')              # 规范的房间目录：PRT_WX_room002（3 位房间号）


def pad_room(rel):
    """把 PRT_WX_room2 补零成 PRT_WX_room002（对 rel 或单个房间名都适用；已是 3 位及以上则不变）。"""
    return re.sub(r'(_room)(\d{1,2})(?=/|$)', lambda m: m.group(1) + m.group(2).zfill(3), rel)


def read_text(path):
    raw = Path(path).read_bytes()
    for enc in ('utf-8', 'gb18030', 'latin1'):
        try:
            return raw.decode(enc)
        except UnicodeDecodeError:
            pass
    return raw.decode('utf-8', 'replace')


def fmt_ts(t):
    return datetime.datetime.fromtimestamp(t, TZ).strftime('%Y-%m-%d %H:%M:%S') if t else ''


def rel_of(raw_uri):
    """OSS 路径去掉桶与 raw_data/raw_data_日期 前缀，从 umi_normal / umi_calibration 开始。"""
    return re.match(r'^oss://[^/]+/raw_data/raw_data_\d{6}/(.+?)/$', raw_uri).group(1)


def cap_of(name):
    """会话目录名里的采集日期（YYYY-MM-DD）。ISuit-V3_… 与 UMI_v3_… 两种命名都认；都不是则抛出带说明的错误。"""
    cap = sdk.session_date(name)
    if not cap:
        raise sdk.UploadError('会话目录名 %s 不是 ISuit-V3_xxx_日期-时间 或 UMI_v3_日期_时间 格式，请重新扫描' % name)
    return cap


def full_uri(rel):
    """数据集的 OSS 路径：格式由会话目录名决定，格式决定桶（ISuit-V3 → 配置里的「ISuit-V3 桶」，UMI_v3 → 「UMI_v3 桶」）。"""
    name = rel.split('/')[-1]
    return 'oss://%s/raw_data/raw_data_%s/%s/' % (sdk.bucket_for(sdk.fmt_of_name(name)), sdk.to_yymmdd(cap_of(name)), rel)


def make_ds(rec):
    fmt = sdk.fmt_of_name(rec['name']) or 'isuit'; cap = cap_of(rec['name'])
    return {'source': Path(rec['source']), 'name': rec['name'], 'type': rec['type'], 'capture_date': cap, 'raw_uri': full_uri(rec['rel']),
            'fmt': fmt, 'bucket': sdk.bucket_for(fmt), 'conf': sdk.FORMATS[fmt]['conf']}


def tag_of(name):
    """并发上传时日志行的会话标签：取目录名末尾的时间部分（ISuit-V3：时-分-秒 8 位；UMI_v3：HHMMSS 6 位）。"""
    return name[-6:] if sdk.UMI_V3_RE.match(name or '') else name[-8:]


def resolve_ossutil(p):
    """ossutil 默认放在 umi_web.py 同级目录：路径为空 / 'ossutil' / 相对路径时，先找同级目录里的 ossutil.exe、ossutil64.exe、ossutil。"""
    p = (p or '').strip()
    if p and os.path.isabs(p): return p
    for n in ([p] if p and p != 'ossutil' else []) + ['ossutil.exe', 'ossutil64.exe', 'ossutil']:
        if (HERE / n).is_file(): return str(HERE / n)
    return p or 'ossutil'


def mlabel(c):
    """上传记录里的「方式」列：sdk·逐文件夹 / sdk·流水线 / ossutil·逐分片 / ossutil·整会话。"""
    if c['method'] == 'sdk': return 'sdk·' + ('流水线' if c['order'] == 'pipeline' else '逐文件夹')
    return 'ossutil·' + ('整会话' if c['ossutil_unit'] == 'session' else '逐分片')


def open_folder(path):
    """在本机文件管理器里打开目录（Windows 资源管理器 / macOS Finder / Linux xdg-open）。"""
    if os.name == 'nt': os.startfile(path)                      # noqa
    elif sys.platform == 'darwin': subprocess.Popen(['open', path])
    else: subprocess.Popen(['xdg-open', path])
