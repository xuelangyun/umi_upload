# -*- coding: utf-8 -*-
"""进度与速度：滑动窗口速度计、日志、已传字节/哈希字节计数、多数据集进度汇总（prog_view）。"""
import collections
import datetime
import hashlib
import os
import time
import upload_umi_sdk as sdk
from .state import CUR, PL, TZ


# ------------------------------------------------------------ 进度 / 速度
class Meter:
    """滑动窗口速度：取最近 WIN 秒的字节增量，比「总量/总时长」更接近实时速度。"""
    WIN = 5.0

    def __init__(s): s.q, s.total = collections.deque(), 0

    def add(s, n):
        with PL:
            s.total = max(s.total + n, 0); s.q.append((time.time(), s.total))

    def speed(s):
        now = time.time()
        with PL:
            while len(s.q) > 1 and now - s.q[0][0] > s.WIN: s.q.popleft()
            if not s.q or now - s.q[0][0] < 0.3: return 0.0
            return max(s.total - s.q[0][1], 0) / (now - s.q[0][0])


def log(job, text):
    tag = job.get('tag')                                    # 并发时日志行带数据集标签（会话名时分秒），便于区分
    job['log'].append('[%s] %s%s' % (datetime.datetime.now(TZ).strftime('%H:%M:%S'), '[%s] ' % tag if tag else '', text)); del job['log'][:-1500]


def add_sent(job, n):
    with PL:
        p = job['prog']; p['sent'] = max(0, min(p['sent'] + n, p['total'])) if p['total'] else max(0, p['sent'] + n)
    job['meter'].add(n)


def hash_file(path, job):
    d = hashlib.sha256()
    with open(path, 'rb') as h:
        for chunk in iter(lambda: h.read(4 * 1024 * 1024), b''):
            d.update(chunk)
            with PL: job['prog']['hashed'] += len(chunk)
    return d.hexdigest()


def _hash_file(path, before):           # 替换 SDK 的 _hash_file，让深度校验也能统计哈希进度
    h = hash_file(path, CUR['job']) if CUR['job'] else None
    after = os.lstat(path)
    if sdk._signature(before) != sdk._signature(after):
        raise sdk.UploadError('文件正在变化，请结束采集后重试: %s' % path)
    return {'size': after.st_size, 'sha256': h}


def prog_view(j):
    """汇总所有并发数据集：只有一个总进度 = (已完成数据集的全部工作量 + 进行中数据集的 哈希+已传 字节) / 总工作量。"""
    now = time.time()
    with PL:
        acts = [dict(a['prog'], last=a['last_speed']) for a in j['actives']]; base = j['base']
    cur, tot = base + sum(a['hashed'] + a['sent'] for a in acts), max(j['total_units'], 1)
    el = max((j['t_end'] or now) - j['t0'], 0.001)
    reps, p0 = [a['rep_speed'] for a in acts], (acts[0] if acts else {})
    ok, fail = sum(i['state'] == 'done' for i in j['items']), sum(i['state'] == 'fail' for i in j['items'])
    sm = lambda k: sum(a[k] for a in acts)
    return {'pct': 100.0 if j['done'] else min(100 * cur / tot, 99.9), 'eta': None if j['done'] or cur <= 0 else (tot - cur) / (cur / el), 'elapsed': el,
            'phase': p0.get('phase', '准备'), 'running': len(acts), 'done_n': ok + fail, 'ds_n': j['count'], 'name': p0.get('name', ''), 'url': p0.get('url', ''),
            'file': p0.get('file', ''), 'raw': p0.get('raw', ''), 'files': sm('files'), 'total_files': sm('total_files'), 'fdone': sm('fdone'), 'ftotal': sm('ftotal'),
            'sent': sm('sent'), 'hashed': sm('hashed'), 'total': sm('total'), 'speed': 0 if j['done'] else (j['meter'].speed() or sm('last')),
            'rep_speed': sum(reps) if acts and all(reps) else None, 'avg': j['meter'].total / el, 'ok': ok, 'fail': fail}


sdk._hash_file = _hash_file
