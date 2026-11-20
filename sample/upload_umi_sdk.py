#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
UMI 数据集批量上传 —— 阿里云 OSS Python SDK（oss2）版，不依赖 ossutil 和客户端。
AK / SK / Endpoint 直接填在下面的「OSS 连接配置」里；上传路径规则与 ossutil 版、客户端完全一致。

安装依赖：  pip install oss2

用法示例（Windows）：
    python upload_umi_sdk.py                                              # 不带参数：按脚本顶部 ROUTES 里维护的多条路线一次上传
    python upload_umi_sdk.py --normal D:\\a D:\\b --calibration D:\\c --city XZ --room 001   # 临时传一组文件夹（忽略 ROUTES）
    python upload_umi_sdk.py --dry-run                                    # 只看计划，不上传

normal 和 calibration 是两个独立的文件夹，类型由你在 ROUTES（或 --normal / --calibration）里指定，不靠目录名猜测。
每个数据集（session）的目标路径：
    oss://{桶}/raw_data/raw_data_{YYMMDD}/{umi_normal|umi_calibration}/PRT_{城市}_room{房间号}/{session名}/
  - 类型：写在 normal 里的文件夹按 normal 上传，写在 calibration 里的按 calibration 上传；
  - session 格式 → 桶：目录名决定格式，格式决定桶（两种格式可同时出现在同一批目录里，各传各的桶）：
      ISuit-V3_xxx_YYYY-MM-DD-hh-mm-ss（datacollect_conf.yaml + 采集切片）→ BUCKET（默认 hw-upload，--bucket 覆盖）
      UMI_v3_YYYYMMDD_HHMMSS（metadata/conf.yaml + head / left hand / right hand）→ BUCKET_UMI（默认 prt-huawei，--umi-bucket 覆盖）
  - 日期：取自 session 目录名；
  - 房间：路线里的 city / room；没写则取路径里最靠近 session 的 PRT_XX_roomNNN 目录。

上传流程：生成清单 → 多文件并发上传（大文件自动分片、断点续传）→ 复核本地与 OSS 文件列表 →
上传 manifest.json → 上传 _READY（提交管线）。同一数据集重复运行安全：已提交且内容相同会自动跳过，失败后重跑会续传。
"""
import argparse
import concurrent.futures
import datetime
import hashlib
import json
import os
import re
import stat
import sys
import threading
import time
from pathlib import Path

# ============================ 路线维护（在这里增删，一次维护多个）============================
# normal 和 calibration 是两个独立的文件夹：一条路线 = 同一个房间的 normal 文件夹 + calibration 文件夹。
# 不带参数运行时按下面的表逐条上传；命令行用了 --normal / --calibration 则忽略此表。
# 路线里的字段（写错字段名会报错，不会被悄悄忽略）：
#   normal       normal 数据所在的文件夹；有多个就写成列表 [..., ...]。文件夹里所有 session 都按 normal 上传
#   calibration  calibration 数据所在的文件夹（写法同上）。normal / calibration 至少写一个
#   city / room  城市缩写 / 房间号（数字）。不写则取文件夹路径里的 PRT_城市_room房间号
#   date         日期（YYYY-MM-DD / YYYYMMDD / YYMMDD）。写了则与 session 目录名里的日期不符时报错；不写取目录名里的日期
#   enabled      False 表示暂时停用这条路线（默认 True）
#   note         备注，仅供自己看
ROUTES = [
     {'city': 'WX', 'room': '004', 'date': '2026-11-02',
      'normal': r'E:\data\umi_noraml\1B7C\20261102\PRT_WX_room004', 'calibration': r'E:\data\umi_calibration\1B7C\20261102\PRT_WX_room004'}
    # {'city': 'XZ', 'room': '002',
    #  'normal': [r'D:\data\0002_a', r'D:\data\0002_b'], 'calibration': r'D:\data\0002_cal'},      # 一个类型多个文件夹
    # {'normal': r'E:\data\umi_noraml', 'calibration': r'E:\data\umi_calibration', 'note': '房间号取自 PRT_ 目录名'},
    # {'city': 'XZ', 'room': '004', 'calibration': r'D:\data\0004_cal', 'note': '这天只有标定'},
    # {'city': 'XZ', 'room': '005', 'normal': r'D:\data\0005_normal', 'enabled': False},            # 暂时停用
]
# ==========================================================================================

# ============================ OSS 连接配置（直接填在这里）============================
# 留空时依次读取环境变量 OSS_ENDPOINT / OSS_ACCESS_KEY_ID / OSS_ACCESS_KEY_SECRET。
# 注意：脚本里含明文密钥，不要提交到代码仓库或发给别人；可使用只有目标桶（hw-upload / prt-huawei）写权限的子账号 AK。
ENDPOINT = 'http://10.20.100.10:9000'             # 例如 'https://oss-cn-hangzhou.aliyuncs.com'（与客户端「配置」里的 endpoint 相同）
ACCESS_KEY_ID = 'minioadmin'
ACCESS_KEY_SECRET = 'minioadmin@123'
MULTIPART_THRESHOLD = 10 * 1024 * 1024   # 大于等于此大小的文件走分片上传（可断点续传）
PART_SIZE = 10 * 1024 * 1024             # 分片大小
# ==========================================================================================

BUCKET = 'hw-upload'                     # ISuit-V3 格式数据上传到的桶；命令行可用 --bucket 覆盖
BUCKET_UMI = 'prt-huawei'                # UMI_v3 格式数据上传到的桶；命令行可用 --umi-bucket 覆盖
TYPE_SUBDIR = {'normal': 'umi_normal', 'calibration': 'umi_calibration'}
MARKERS = {'manifest.json', '_READY', '_SUCCESS.json'}
SESSION_RE = re.compile(r'^.+_(\d{4}-\d{2}-\d{2})-\d{2}-\d{2}-\d{2}$')
PRT_RE = re.compile(r'^PRT_([A-Za-z]+)_room(\d+)$', re.IGNORECASE)
LS_RE = re.compile(r'^(.+?)\s+(\d+)\s+(Standard|IA|Archive|ColdArchive|DeepColdArchive)\s+([0-9A-Fa-f]{32}(?:-\d+)?)\s+(.+)$')

# ============================ 会话格式（目录名决定）→ 桶 ============================
# 两种格式可以同时出现（例如一个盘放 ISuit-V3、另一个盘放 UMI_v3）：目录名决定格式，格式决定上传到哪个桶（与官方客户端一致，只是官方客户端按桶选格式）。
#   isuit   ISuit-V3_YYYY-MM-DD-HH-MM-SS   datacollect_conf.yaml + 数字编号的采集切片 <会话>_N/        → BUCKET（hw-upload）
#   umi_v3  UMI_v3_YYYYMMDD_HHMMSS         metadata/conf.yaml + head / left hand / right hand 三个采集目录 → BUCKET_UMI（prt-huawei）
# 两种格式的 OSS 路径、清单（manifest.json）与提交协议（数据文件 → manifest.json → _READY）完全相同，只有「怎么识别和校验 session」和「用哪个桶」不同。
UMI_V3_RE = re.compile(r'^UMI_v3_(\d{8})_(\d{6})$')
UMI_V3_DIRS = ('head', 'left hand', 'right hand')       # UMI_v3 session 根目录下必须有的采集目录
FORMATS = {
    'isuit': {'label': 'ISuit-V3', 'token': 'ISuit-V3_xxx', 'conf': 'datacollect_conf.yaml', 'name_re': SESSION_RE,
              'desc': 'datacollect_conf.yaml 和采集切片'},
    'umi_v3': {'label': 'UMI_v3', 'token': 'UMI_v3_xxx', 'conf': 'metadata/conf.yaml', 'name_re': UMI_V3_RE,
               'desc': 'metadata/conf.yaml 和 head / left hand / right hand 采集目录'},
}


class UploadError(Exception):
    pass


def session_date(name, fmt=None):
    """session 目录名里的采集日期（YYYY-MM-DD）；不符合格式返回 None。fmt 缺省 = 两种格式都试。"""
    for f in ([fmt] if fmt else FORMATS):
        m = FORMATS[f]['name_re'].match(name or '')
        if m:
            d = m.group(1)
            return d if '-' in d else '%s-%s-%s' % (d[:4], d[4:6], d[6:])
    return None


def fmt_of_name(name):
    """按目录名判断 session 属于哪种格式（isuit / umi_v3）；都不是返回 None。两种命名互不重叠。"""
    return next((f for f in FORMATS if session_date(name, f)), None)


def bucket_for(fmt):
    """格式 → 目标桶：umi_v3 → BUCKET_UMI，其余 → BUCKET。"""
    return BUCKET_UMI if fmt == 'umi_v3' else BUCKET


def validate_session(fmt, source, entries):
    """session 结构校验（与官方客户端 FORMAT[fmt].validate 一致）。entries = 该目录第一级的 os.scandir 条目；不合格抛 UploadError。"""
    name = source.name
    if fmt == 'umi_v3':
        if not os.path.isfile(os.path.join(str(source), 'metadata', 'conf.yaml')):
            raise UploadError('数据集缺少 metadata/conf.yaml')
        for sub in UMI_V3_DIRS:
            if not any(e.is_dir() and e.name == sub for e in entries):
                raise UploadError('数据集缺少采集目录 %s' % sub)
        return
    if not any(e.name == 'datacollect_conf.yaml' and e.is_file() for e in entries):
        raise UploadError('数据集缺少 datacollect_conf.yaml')
    if not any(e.is_dir() and e.name.startswith(name + '_') and e.name[len(name) + 1:].isdigit() for e in entries):
        raise UploadError('数据集缺少数字编号的采集切片')


# ------------------------------------------------------------------ 通用工具
def sha256_text(text):
    return hashlib.sha256(text.encode('utf-8')).hexdigest()


def utf16_key(text):
    # 与 JavaScript 字符串比较（UTF-16 码元顺序）一致，保证清单文件顺序和客户端相同
    return text.encode('utf-16-be', 'surrogatepass')


def to_yymmdd(value):
    text = str(value or '').strip()
    if re.fullmatch(r'\d{4}-\d{2}-\d{2}', text):
        full = text.replace('-', '')
    elif re.fullmatch(r'\d{8}', text):
        full = text
    elif re.fullmatch(r'\d{6}', text):
        full = '20' + text
    else:
        raise UploadError('日期无效: %r' % value)
    year, month, day = int(full[:4]), int(full[4:6]), int(full[6:])
    try:
        datetime.date(year, month, day)
    except ValueError:
        raise UploadError('采集日期无效: %s' % value)
    if not 2000 <= year <= 2099:
        raise UploadError('采集日期无效: %s' % value)
    return full[2:]


def is_excluded(rel):
    parts = rel.split('/')
    return '_pipeline' in parts or parts[-1] in MARKERS


def safe_part(name):
    if not name or name in ('.', '..') or re.search(r'[\\/\r\n?#\0]', name):
        raise UploadError('目录名不安全: %r' % name)
    return name


def format_size(n):
    for unit in ('B', 'KB', 'MB', 'GB'):
        if n < 1024 or unit == 'GB':
            return '%.1f%s' % (n, unit) if unit != 'B' else '%d%s' % (n, unit)
        n /= 1024.0


class Ui:
    """控制台输出：ossutil 的进度行（\\r 覆盖）原地刷新，其他行正常换行。"""

    def __init__(self):
        self.in_progress = False

    def line(self, text, replace=False):
        if replace:
            sys.stdout.write('\r' + text[:110].ljust(110))
            self.in_progress = True
        else:
            if self.in_progress:
                sys.stdout.write('\n')
                self.in_progress = False
            sys.stdout.write(text + '\n')
        sys.stdout.flush()


# ------------------------------------------------------------------ OSS SDK 封装
class Progress:
    """线程安全的上传进度：原地刷新一行（文件数 / 字节数 / 平均速度）。"""

    def __init__(self, ui, total_files, total_bytes):
        self.ui, self.total_files, self.total_bytes = ui, total_files, total_bytes
        self.files = self.bytes = 0
        self.lock, self.started, self.last_print = threading.Lock(), time.time(), 0.0

    def add(self, nbytes=0, files=0):
        with self.lock:
            self.bytes = min(self.bytes + nbytes, self.total_bytes)
            self.files += files
            now = time.time()
            if now - self.last_print >= 0.5 or self.files >= self.total_files:
                self.last_print = now
                speed = self.bytes / max(now - self.started, 0.001)
                self.ui.line('  %d/%d 个文件  %s/%s  %s/s' % (self.files, self.total_files, format_size(self.bytes),
                                                           format_size(self.total_bytes), format_size(speed)), True)


class Oss:
    def __init__(self, state_dir, jobs, parallel, ui, bucket=None):
        endpoint = ENDPOINT or os.environ.get('OSS_ENDPOINT', '')
        key_id = ACCESS_KEY_ID or os.environ.get('OSS_ACCESS_KEY_ID', '')
        key_secret = ACCESS_KEY_SECRET or os.environ.get('OSS_ACCESS_KEY_SECRET', '')
        if not (endpoint and key_id and key_secret):
            raise UploadError('请先在脚本顶部「OSS 连接配置」里填写 ENDPOINT / ACCESS_KEY_ID / ACCESS_KEY_SECRET（或设置对应环境变量）')
        self.jobs, self.parallel, self.ui = jobs, parallel, ui
        self.endpoint, self.key_id, self.key_secret = endpoint, key_id, key_secret
        self.state_dir = state_dir
        self.bucket_name = bucket or BUCKET                     # 这个客户端操作的桶（两种数据格式各用各的桶，每个桶一个客户端）

        # 判断是否为 MinIO / S3 兼容服务端（自建 IP / 端口 或 包含 minio/s3 等）
        is_s3_compatible = not ('.aliyuncs.com' in endpoint)
        self.is_s3 = is_s3_compatible

        if self.is_s3:
            try:
                import boto3
                import botocore
                from botocore.config import Config
                from boto3.s3.transfer import TransferConfig
            except ImportError:
                raise UploadError('S3/MinIO 服务端需要 boto3，请先执行: pip install boto3')
            self.botocore = botocore
            self.boto3_client = boto3.client(
                's3',
                endpoint_url=endpoint,
                aws_access_key_id=key_id,
                aws_secret_access_key=key_secret,
                config=Config(signature_version='s3v4', max_pool_connections=max(20, jobs * parallel))
            )
            self.transfer_config = TransferConfig(
                multipart_threshold=MULTIPART_THRESHOLD,
                multipart_chunksize=PART_SIZE,
                max_concurrency=parallel,
                use_threads=True
            )
        else:
            try:
                import oss2
            except ImportError:
                raise UploadError('缺少 oss2，请先执行: pip install oss2')
            self.oss2 = oss2
            session = oss2.Session(pool_size=max(10, jobs * parallel))
            self.bucket = oss2.Bucket(oss2.Auth(key_id, key_secret), endpoint, self.bucket_name, session=session, connect_timeout=30)
            checkpoint_root = os.path.join(state_dir, 'oss2-checkpoints')
            os.makedirs(checkpoint_root, exist_ok=True)
            self.store = oss2.ResumableStore(root=checkpoint_root)

    def _key(self, uri):
        prefix = 'oss://%s/' % self.bucket_name
        if not uri.startswith(prefix):
            raise UploadError('不是 %s 桶里的路径: %s' % (self.bucket_name, uri))
        return uri[len(prefix):]

    def _transient(self, error):
        if self.is_s3:
            from botocore.exceptions import EndpointConnectionError, ConnectionClosedError, ReadTimeoutError, ConnectTimeoutError, ClientError
            if isinstance(error, (EndpointConnectionError, ConnectionClosedError, ReadTimeoutError, ConnectTimeoutError, ConnectionError, TimeoutError)):
                return True
            if isinstance(error, ClientError):
                code = error.response.get('ResponseMetadata', {}).get('HTTPStatusCode', 0)
                return code >= 500 or code == 429
            return False
        else:
            exc = self.oss2.exceptions
            return (isinstance(error, (exc.RequestError, ConnectionError, TimeoutError))
                    or (isinstance(error, exc.ServerError) and (error.status >= 500 or error.status == 429)))

    def _retry(self, action, attempts=4):
        for attempt in range(1, attempts + 1):
            try:
                return action()
            except Exception as error:  # noqa: 只重试网络类 / 服务端临时错误
                if attempt == attempts or not self._transient(error):
                    raise
                time.sleep(2 ** (attempt - 1))

    def upload_files(self, source, raw_uri, files):
        """并发上传清单里的文件：--jobs 个文件同时传，大文件再按 --parallel 分片并发、断点续传。
        只上传清单内的文件，所以 _pipeline / manifest.json / _READY 等排除项天然不会被传上去。"""
        prefix = self._key(raw_uri)
        progress = Progress(self.ui, len(files), sum(f['size'] for f in files))
        stop, errors = threading.Event(), []

        def one(f):
            if stop.is_set():
                return
            local = os.path.join(str(source), *f['path'].split('/'))
            last = [0]

            def callback(consumed, _total=None):
                # boto3 的 Callback(bytes_transferred) 传入的是增量字节；oss2 传入的是已传输总字节
                if self.is_s3:
                    progress.add(consumed)
                    last[0] += consumed
                else:
                    if consumed >= last[0]:
                        progress.add(consumed - last[0])
                    last[0] = consumed

            try:
                if self.is_s3:
                    self._retry(lambda: self.boto3_client.upload_file(
                        local, self.bucket_name, prefix + f['path'],
                        Config=self.transfer_config,
                        Callback=callback
                    ))
                else:
                    self._retry(lambda: self.oss2.resumable_upload(
                        self.bucket, prefix + f['path'], local, store=self.store,
                        multipart_threshold=MULTIPART_THRESHOLD, part_size=PART_SIZE,
                        progress_callback=callback, num_threads=self.parallel))
            except Exception as error:  # noqa: 记录后让其他任务尽快停下
                stop.set()
                errors.append('%s: %s' % (f['path'], error))
                return
            progress.add(max(0, f['size'] - last[0]), 1)  # 补足回调未覆盖的字节，并记一个完成文件

        with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, self.jobs)) as pool:
            list(pool.map(one, sorted(files, key=lambda f: -f['size'])))  # 大文件先传，减少尾部等待
        if errors:
            raise UploadError('%d 个文件上传失败，尚未提交管线（只列前 5 个）:\n    %s' % (len(errors), '\n    '.join(errors[:5])))

    def put_object(self, uri, data):
        if self.is_s3:
            self._retry(lambda: self.boto3_client.put_object(Bucket=self.bucket_name, Key=self._key(uri), Body=data))
        else:
            self._retry(lambda: self.bucket.put_object(self._key(uri), data))

    def read_object(self, uri):
        if self.is_s3:
            try:
                resp = self._retry(lambda: self.boto3_client.get_object(Bucket=self.bucket_name, Key=self._key(uri)))
                return resp['Body'].read()
            except self.botocore.exceptions.ClientError as e:
                if e.response.get('Error', {}).get('Code') in ('NoSuchKey', '404'):
                    return None
                raise
        else:
            try:
                return self._retry(lambda: self.bucket.get_object(self._key(uri)).read())
            except self.oss2.exceptions.NoSuchKey:
                return None

    def list_objects(self, uri):
        prefix = self._key(uri)
        if self.is_s3:
            def scan_s3():
                paginator = self.boto3_client.get_paginator('list_objects_v2')
                items = []
                for page in paginator.paginate(Bucket=self.bucket_name, Prefix=prefix):
                    for obj in page.get('Contents', []):
                        if not obj['Key'].endswith('/'):
                            items.append({'name': obj['Key'][len(prefix):], 'size': obj['Size']})
                return items
            return self._retry(scan_s3)
        else:
            def scan():
                return [{'name': obj.key[len(prefix):], 'size': obj.size}
                        for obj in self.oss2.ObjectIterator(self.bucket, prefix=prefix)
                        if not obj.key.endswith('/')]
            return self._retry(scan)


# ------------------------------------------------------------------ 数据集发现与分类
def find_room(source, city_arg, room_arg):
    for part in reversed(source.parts[:-1]):
        m = PRT_RE.match(part)
        if m:
            return m.group(1).upper(), m.group(2)
    city = re.sub(r'[^A-Z]', '', (city_arg or '').upper())
    room = re.sub(r'\D', '', room_arg or '')
    if not city or not room:
        raise UploadError('路径里没有 PRT_城市_room房间号 目录，请用 --city 和 --room 指定')
    return city, room


def find_sessions(root):
    """在 root 下（含 root 自身）递归找 session 目录，两种格式（ISuit-V3 / UMI_v3）一起认，按目录名区分；找到 session 就不再往里走。
    返回 ([(目录, 第一级条目, 格式)], 问题)。"""
    sessions, problems = [], []

    def visit(directory):
        try:
            entries = sorted(os.scandir(directory), key=lambda e: e.name)
        except OSError as error:
            problems.append('%s: %s' % (directory, error))
            return
        fmt = fmt_of_name(directory.name)
        if fmt:
            sessions.append((directory, entries, fmt))
            return
        for entry in entries:
            if entry.name == '_pipeline' or entry.name.startswith('.'):
                continue
            if entry.is_symlink():
                problems.append('上传目录不支持符号链接: ' + entry.path)
            elif entry.is_dir(follow_symlinks=False):
                visit(Path(entry.path))

    visit(root)
    return sessions, problems


ROUTE_KEYS = {'normal', 'calibration', 'city', 'room', 'date', 'enabled', 'note'}


def expand_routes(routes):
    """把路线表展开成「文件夹 + 类型」任务：写在 normal 里的就是 normal，写在 calibration 里的就是 calibration。"""
    jobs, problems = [], []
    for route in routes:
        if route.get('enabled', True) is False:
            continue
        label = route.get('note') or '; '.join('%s=%s' % (k, route[k]) for k in ('normal', 'calibration') if route.get(k)) or repr(route)
        unknown = sorted(set(route) - ROUTE_KEYS)
        if unknown:
            problems.append('%s: 不认识的字段 %s（可用字段: %s）' % (label, unknown, ', '.join(sorted(ROUTE_KEYS))))
            continue
        found = False
        for kind in ('normal', 'calibration'):
            value = route.get(kind)
            if not value:
                continue
            for path in ([value] if isinstance(value, (str, os.PathLike)) else list(value)):
                jobs.append({'path': path, 'type': kind, 'city': route.get('city'), 'room': route.get('room'), 'date': route.get('date')})
                found = True
        if not found:
            problems.append('%s: 路线里没有 normal / calibration 文件夹' % label)
    return jobs, problems


def discover(routes, args):
    """扫描路线：ISuit-V3_xxx 与 UMI_v3_xxx 两种会话都认（按目录名区分）；每个数据集带 fmt 和它的目标桶（ISuit-V3 → BUCKET，UMI_v3 → BUCKET_UMI）。"""
    datasets, (jobs, problems), seen, claimed = [], expand_routes(routes), {}, {}
    for job in jobs:
        dtype, date = job['type'], job['date'] or args.date
        root = Path(os.path.abspath(str(job['path'])))
        if not root.is_dir():
            problems.append('%s 文件夹不存在: %s' % (dtype, root))
            continue
        sessions, issues = find_sessions(root)
        problems += issues
        if not sessions:
            problems.append('%s 文件夹里没有找到数据集（%s）: %s' % (dtype, ' 或 '.join('%s 格式：%s 目录，需带 %s' % (f['label'], f['token'], f['desc']) for f in FORMATS.values()), root))
        for source, entries, fmt in sessions:
            name, spec = source.name, FORMATS[fmt]
            try:
                validate_session(fmt, source, entries)
                capture_date = session_date(name, fmt)
                if date and to_yymmdd(date) != to_yymmdd(capture_date):
                    raise UploadError('采集日期不一致: 数据集是 %s，指定的是 %s' % (capture_date, date))
                if claimed.setdefault(str(source), dtype) != dtype:
                    raise UploadError('同一个数据集同时出现在 normal 和 calibration 文件夹里，请检查路线')
                if job['room']:  # 路线里显式写了房间号，优先于路径里的 PRT 目录
                    city = re.sub(r'[^A-Z]', '', str(job['city'] or args.city or '').upper())
                    room = re.sub(r'\D', '', str(job['room']))
                    if not city or not room:
                        raise UploadError('路线里的 city / room 无效（城市要字母缩写，房间号要数字）')
                else:
                    city, room = find_room(source, job['city'] or args.city, args.room)
                prt = 'PRT_%s_room%s' % (city, room)
                bucket = bucket_for(fmt)
                raw_uri = 'oss://%s/raw_data/raw_data_%s/%s/%s/%s/' % (bucket, to_yymmdd(capture_date), TYPE_SUBDIR[dtype], prt, name)
                if raw_uri in seen and seen[raw_uri] != str(source):
                    raise UploadError('不同本地目录指向同一 OSS 数据集: %s 与 %s' % (seen[raw_uri], source))
                if raw_uri in seen:
                    continue
                seen[raw_uri] = str(source)
                datasets.append({'source': source, 'name': name, 'type': dtype, 'capture_date': capture_date, 'raw_uri': raw_uri, 'fmt': fmt, 'bucket': bucket, 'conf': spec['conf']})
            except UploadError as error:
                problems.append('%s: %s' % (source, error))
    return datasets, problems


# ------------------------------------------------------------------ 清单与上传（与客户端 pipeline-upload.js 对应）
def _signature(st):
    return (st.st_size, st.st_mtime_ns, st.st_ctime_ns, st.st_ino)


def _hash_file(path, before):
    digest = hashlib.sha256()
    with open(path, 'rb') as handle:
        for chunk in iter(lambda: handle.read(4 * 1024 * 1024), b''):
            digest.update(chunk)
    after = os.lstat(path)
    if _signature(before) != _signature(after):
        raise UploadError('文件正在变化，请结束采集后重试: %s' % path)
    return {'size': after.st_size, 'sha256': digest.hexdigest()}


def snapshot(source, workers):
    pending = []

    def visit(directory, parts):
        for name in sorted(os.listdir(directory), key=utf16_key):
            safe_part(name)
            rel = '/'.join(parts + [name])
            if is_excluded(rel):
                continue
            path = os.path.join(directory, name)
            st = os.lstat(path)
            if stat.S_ISLNK(st.st_mode):
                raise UploadError('数据集不支持符号链接: ' + path)
            if stat.S_ISDIR(st.st_mode):
                visit(path, parts + [name])
            elif stat.S_ISREG(st.st_mode):
                pending.append((rel, path, st))
            else:
                raise UploadError('不支持的文件类型: ' + path)

    visit(str(source), [])
    with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, workers)) as pool:
        hashed = list(pool.map(lambda item: _hash_file(item[1], item[2]), pending))
    files = [{'path': item[0], 'size': h['size'], 'sha256': h['sha256']} for item, h in zip(pending, hashed)]
    files.sort(key=lambda f: utf16_key(f['path']))
    if not files:
        raise UploadError('数据集为空: %s' % source)
    return files


def make_manifest(ds, files):
    identity = {'dataset_id': ds['name'], 'capture_date': ds['capture_date'], 'dataset_type': ds['type'], 'files': files}
    revision = 'raw-' + sha256_text(json.dumps(identity, ensure_ascii=False, separators=(',', ':')))[:32]
    manifest = {'schema_version': 1}
    manifest.update(identity)
    manifest['revision'] = revision
    now = datetime.datetime.now(datetime.timezone.utc)
    manifest['created_at'] = now.strftime('%Y-%m-%dT%H:%M:%S.') + '%03dZ' % (now.microsecond // 1000)
    return manifest


def same_content(a, b):
    return (a.get('schema_version') == 1 and a.get('dataset_id') == b['dataset_id'] and a.get('capture_date') == b['capture_date']
            and a.get('dataset_type') == b['dataset_type'] and a.get('files') == b['files'])


def remote_commit(oss, raw_uri):
    ready_bytes = oss.read_object(raw_uri + '_READY')
    if ready_bytes is None:
        return None
    manifest_bytes = oss.read_object(raw_uri + 'manifest.json')
    if manifest_bytes is None:
        raise UploadError('远端已有 _READY 但缺少清单，拒绝覆盖: ' + raw_uri)
    ready, manifest = json.loads(ready_bytes.decode('utf-8')), json.loads(manifest_bytes.decode('utf-8'))
    if ready.get('schema_version') != 1 or ready.get('revision') != manifest.get('revision') or ready.get('manifest_sha256') != hashlib.sha256(manifest_bytes).hexdigest():
        raise UploadError('远端完成标记与清单不一致，拒绝覆盖: ' + raw_uri)
    return {'ready': ready, 'manifest': manifest}


def verify_inventory(oss, raw_uri, files):
    remote = [f for f in oss.list_objects(raw_uri) if not is_excluded(f['name'])]
    by_name = {f['name']: f['size'] for f in remote}
    if len(remote) != len(files) or any(by_name.get(f['path']) != f['size'] for f in files):
        raise UploadError('远端文件列表或大小与本地清单不一致；未发送完成标记，请检查残留文件后重试: ' + raw_uri)


def upload_dataset(ds, oss, ui, hash_workers):
    started = time.time()
    raw_uri, source = ds['raw_uri'], ds['source']
    ui.line('  生成数据清单（计算 sha256）…')
    files = snapshot(source, hash_workers)
    manifest = make_manifest(ds, files)
    total_bytes = sum(f['size'] for f in files)
    conf = (source / ds.get('conf', 'datacollect_conf.yaml')).read_text(encoding='utf-8', errors='replace')
    if re.search(r'^record_location:\s*["\']?Default["\']?\s*(?:#.*)?$', conf, re.MULTILINE):
        ui.line('  提示：采集位置为 Default，后续交付需要正确的房间元数据；上传程序不会自动猜测房间。')
    result = {'rawUri': raw_uri, 'fileCount': len(files), 'bytes': total_bytes}
    existing = remote_commit(oss, raw_uri)
    if existing:
        if not same_content(existing['manifest'], manifest):
            raise UploadError('同一路径已提交过不同内容，不能覆盖正在处理或已完成的数据集: ' + raw_uri)
        verify_inventory(oss, raw_uri, files)
        ui.line('  已提交过相同数据，保留原版本，不重复创建任务')
        result.update(status='already-ready', revision=existing['manifest']['revision'])
        return result
    ui.line('  并发上传 %d 个文件（%s），同时传输 %d 个…' % (len(files), format_size(total_bytes), oss.jobs))
    oss.upload_files(source, raw_uri, files)
    ui.line('  复核本地文件和 OSS 文件列表…')
    if snapshot(source, hash_workers) != files:
        raise UploadError('上传过程中数据集发生变化，未发送完成标记；请结束采集后重试')
    verify_inventory(oss, raw_uri, files)
    concurrent_commit = remote_commit(oss, raw_uri)
    if concurrent_commit:
        if not same_content(concurrent_commit['manifest'], manifest):
            raise UploadError('其他上传进程已提交不同版本，请勿同时上传同一数据集')
        result.update(status='already-ready', revision=concurrent_commit['manifest']['revision'])
        return result
    manifest_bytes = (json.dumps(manifest, ensure_ascii=False, indent=2) + '\n').encode('utf-8')
    oss.put_object(raw_uri + 'manifest.json', manifest_bytes)
    if oss.read_object(raw_uri + 'manifest.json') != manifest_bytes:
        raise UploadError('远端清单校验失败，未发送完成标记')
    marker = {'schema_version': 1, 'revision': manifest['revision'], 'manifest_sha256': hashlib.sha256(manifest_bytes).hexdigest()}
    ui.line('  所有文件上传成功，提交数据集到处理管线…')
    oss.put_object(raw_uri + '_READY', (json.dumps(marker, indent=2) + '\n').encode('utf-8'))
    confirmed = remote_commit(oss, raw_uri)
    if not confirmed or confirmed['ready']['manifest_sha256'] != marker['manifest_sha256']:
        raise UploadError('完成标记确认失败，请重试以确认提交状态')
    elapsed = max(time.time() - started, 0.001)
    ui.line('  完成: %d 个文件, %s, 用时 %.0fs, 平均 %s/s' % (len(files), format_size(total_bytes), elapsed, format_size(total_bytes / elapsed)))
    result.update(status='submitted', revision=manifest['revision'])
    return result


# ------------------------------------------------------------------ 入口
def client_city():
    """客户端「配置」里保存的城市缩写，仅作为 --city 的默认值。"""
    for base in (os.environ.get('APPDATA'), os.path.join(os.path.expanduser('~'), '.config')):
        if not base:
            continue
        try:
            with open(os.path.join(base, 'oss-upload-tool', 'config.json'), encoding='utf-8') as handle:
                return json.load(handle).get('city', '')
        except (OSError, ValueError):
            continue
    return ''


def parse_args():
    p = argparse.ArgumentParser(description='UMI 数据集批量上传：自动识别 normal / calibration，按日期和房间号上传到 OSS 并提交管线')
    p.add_argument('--normal', nargs='+', metavar='DIR', help='normal 数据所在的文件夹（可多个）；与 --calibration 一起用来临时指定，不用时读脚本顶部 ROUTES')
    p.add_argument('--calibration', nargs='+', metavar='DIR', help='calibration 数据所在的文件夹（可多个）')
    p.add_argument('--bucket', help='ISuit-V3 格式数据的目标桶（默认用脚本顶部的 BUCKET，hw-upload）')
    p.add_argument('--umi-bucket', help='UMI_v3 格式数据的目标桶（默认用脚本顶部的 BUCKET_UMI，prt-huawei）')
    p.add_argument('--date', help='可选：断言数据集日期（YYYY-MM-DD / YYYYMMDD / YYMMDD），不一致则报错；默认取各 session 目录名里的日期')
    p.add_argument('--city', help='城市缩写，路线里没写 city 时使用（默认取客户端配置）')
    p.add_argument('--room', help='房间号（数字），路径里没有 PRT_城市_room房间号 且路线没写 room 时使用')
    p.add_argument('--skip-invalid', action='store_true', help='有问题的数据集跳过，其余继续（默认：有任何问题就整批不上传）')
    p.add_argument('--dry-run', action='store_true', help='只显示上传计划，不上传')
    p.add_argument('-y', '--yes', action='store_true', help='跳过确认直接上传')
    p.add_argument('--jobs', type=int, default=16, help='同时上传的文件数（默认 16）')
    p.add_argument('--parallel', type=int, default=4, help='单个大文件的分片并发（默认 4）')
    p.add_argument('--hash-workers', type=int, default=8, help='计算 sha256 的线程数（默认 8）')
    p.add_argument('--state-dir', default=os.path.join(os.path.expanduser('~'), '.umi_upload'), help='断点续传记录和上传汇总的保存目录')
    return p.parse_args()


def main():
    global BUCKET, BUCKET_UMI
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, 'reconfigure'):
            stream.reconfigure(errors='replace')
    args = parse_args()
    if args.bucket:
        BUCKET = args.bucket.strip()
    if args.umi_bucket:
        BUCKET_UMI = args.umi_bucket.strip()
    ui = Ui()
    ui.line('目标桶: ISuit-V3 → %s；UMI_v3 → %s' % (BUCKET, BUCKET_UMI))
    if not args.city:
        args.city = client_city()
    if args.normal or args.calibration:
        routes = [{'normal': args.normal or [], 'calibration': args.calibration or []}]
    else:
        routes = [r for r in ROUTES if r.get('enabled', True) is not False]
        if not routes:
            ui.line('没有指定文件夹：请在脚本顶部 ROUTES 里维护路线，或用 --normal / --calibration 指定。用法见 --help。')
            return 2
        ui.line('使用脚本内维护的 %d 条路线：' % len(routes))
        for r in routes:
            parts = ['%s=%s' % (k, r[k]) for k in ('normal', 'calibration') if r.get(k)]
            ui.line('  - %s%s' % ('; '.join(parts), ('  # ' + r['note']) if r.get('note') else ''))
    datasets, problems = discover(routes, args)
    for text in problems:
        ui.line('[问题] ' + text)
    if problems and not args.skip_invalid:
        ui.line('存在以上问题，整批未上传。修正后重试，或加 --skip-invalid 跳过有问题的数据集。')
        return 2
    if not datasets:
        ui.line('没有可上传的数据集。')
        return 2
    ui.line('上传计划（共 %d 个数据集）：' % len(datasets))
    for ds in datasets:
        ui.line('  [%-11s] %s\n               -> %s' % (ds['type'], ds['source'], ds['raw_uri']))
    for t in TYPE_SUBDIR:
        ui.line('  %s: %d 个' % (t, sum(1 for d in datasets if d['type'] == t)))
    for f in FORMATS:
        ui.line('  %s: %d 个 -> 桶 %s' % (FORMATS[f]['label'], sum(1 for d in datasets if d['fmt'] == f), bucket_for(f)))
    if args.dry_run:
        return 0
    os.makedirs(args.state_dir, exist_ok=True)
    oss_by = {}                                              # 每个桶一个客户端：两种格式可同时出现，各传各的桶
    oss_of = lambda ds: oss_by.setdefault(ds['bucket'], Oss(args.state_dir, args.jobs, args.parallel, ui, ds['bucket']))
    oss = None
    if not args.yes and input('确认上传？[y/N] ').strip().lower() not in ('y', 'yes'):
        ui.line('已取消。')
        return 1
    records, failed = [], 0
    started = time.time()
    for i, ds in enumerate(datasets, 1):
        ui.line('== [%d/%d] %s  (%s) ==' % (i, len(datasets), ds['name'], ds['type']))
        try:
            result = upload_dataset(ds, oss_of(ds), ui, args.hash_workers)
            ui.line('[OK] %s: %s' % (result['status'], ds['raw_uri']))
            records.append(dict(result, source=str(ds['source']), type=ds['type']))
        except Exception as error:  # noqa: 单个数据集失败不影响后续，最后统一汇总
            failed += 1
            ui.line('[FAIL] %s: %s' % (ds['source'], error))
            records.append({'source': str(ds['source']), 'type': ds['type'], 'rawUri': ds['raw_uri'], 'status': 'failed', 'error': str(error)})
    summary = os.path.join(args.state_dir, 'summary_%s.json' % datetime.datetime.now().strftime('%Y%m%d_%H%M%S'))
    with open(summary, 'w', encoding='utf-8') as handle:
        json.dump({'generatedAt': datetime.datetime.now().isoformat(), 'items': records}, handle, ensure_ascii=False, indent=2)
    counts = {}
    for r in records:
        counts[r['status']] = counts.get(r['status'], 0) + 1
    ui.line('全部结束，用时 %.0fs: %s' % (time.time() - started, ', '.join('%s=%d' % kv for kv in sorted(counts.items()))))
    ui.line('上传记录: ' + summary)
    if failed:
        ui.line('有 %d 个数据集失败，修正后重新运行同一命令即可（已提交的会自动跳过，未完成的会续传）。' % failed)
    return 1 if failed else 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except UploadError as error:
        print('[错误] %s' % error)
        sys.exit(2)
    except KeyboardInterrupt:
        print('\n已中断；重新运行同一命令可续传。')
        sys.exit(130)
