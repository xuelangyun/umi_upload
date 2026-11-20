# -*- coding: utf-8 -*-
"""扫描：在根目录里找 umi_normal/umi_calibration → 识别数据集 → 统计会话（时长/大小/时间）；数据行提示；后台扫描任务。
提速点：① os.scandir 一次遍历（Windows 上目录项自带体积，不再逐文件 stat）② 分片 yaml 只读文件尾部 ③ 会话签名缓存：重复扫描只重新统计有变化的会话
        ④ 统计线程数按 CPU 放大（I/O 密集）。「完整扫描」可忽略缓存。
两种会话格式（由桶决定，见 upload_umi_sdk.FORMATS）：ISuit-V3（分片 yaml 里取时长）/ UMI_v3（没有分片；时长与起止时间取自 metadata/conf.yaml，见 umi_times）。"""
import concurrent.futures as cf
import datetime
import hashlib
import os
import re
import time
from pathlib import Path
from types import SimpleNamespace
import yaml
import upload_umi_sdk as sdk
from .state import DATASETS, LOCAL_DIRS, PL, PROBLEMS, SCAN, SKIP_DIRS, TZ
from .db import apply_cfg, save_datasets
from .util import fmt_ts, pad_room, read_text, rel_of, ROOM_OK

CONF_FIELDS = ('size', 'slices', 'duration', 'start', 'end', 'task', 'sub_task', 'device_sn', 'purpose', 'operator', 'location', 'misplaced')   # 缓存命中时直接复用的统计字段（device 取自路径，每次重算）
CAL_MAX_SEC = 180                                                # 时长小于 3 分钟 → 视为标定数据


def _times(fp):
    """分片 yaml 的 head_time/tail_time 在文件末尾：只读最后 600 字节；读不到再整文件读。"""
    try:
        with open(fp, 'rb') as f:
            f.seek(0, 2); n = f.tell(); f.seek(max(0, n - 600)); t = f.read().decode('utf-8', 'ignore')
        h, e = re.search(r'head_time:\s*([\d.]+)', t), re.search(r'tail_time:\s*([\d.]+)', t)
        if h and e: return float(h.group(1)), float(e.group(1))
        t = read_text(fp)
        h, e = re.search(r'^head_time:\s*([\d.]+)', t, re.M), re.search(r'^tail_time:\s*([\d.]+)', t, re.M)
        return (float(h.group(1)), float(e.group(1))) if h and e else None
    except OSError:
        return None


def session_sig(src):
    """会话目录的轻量签名：目录 mtime + 分片目录数 + 最新分片目录 mtime。新增分片/文件会改变它；文件内容原地改动不会（用「完整扫描」）。"""
    try:
        n = newest = 0
        with os.scandir(src) as it:
            for e in it:
                if e.is_dir(follow_symlinks=False): n += 1; newest = max(newest, e.stat(follow_symlinks=False).st_mtime_ns)
        return '%d:%d:%d' % (os.stat(src).st_mtime_ns, n, newest)
    except OSError:
        return ''


def stats(src, name, shards=True):
    """一次 scandir 遍历：体积（排除 manifest/_READY/_pipeline）、分片数、采集时长、起止时间。
    shards=False（UMI_v3）：没有「<会话>_N/<会话>_N.yaml」分片，只统计体积，分片数 0、时长/起止留空（由 umi_times 补）。"""
    size = slices = 0
    heads, tails, dur = [], [], 0.0
    stack, pre = [(str(src), 0)], (name + '_') if shards else None
    while stack:
        d, depth = stack.pop()
        try: it = os.scandir(d)
        except OSError: continue
        with it:
            for e in it:
                try:
                    if e.is_dir(follow_symlinks=False):
                        if e.name != '_pipeline': stack.append((e.path, depth + 1))
                        continue
                    if e.name in sdk.MARKERS: continue
                    size += e.stat(follow_symlinks=False).st_size
                    if pre and depth == 1 and e.name.endswith('.yaml') and e.name.startswith(pre):
                        slices += 1; t = _times(e.path)
                        if t: heads.append(t[0]); tails.append(t[1]); dur += max(t[1] - t[0], 0)
                except OSError:
                    continue
    return {'size': size, 'slices': slices, 'duration': round(dur, 2), 'start': fmt_ts(min(heads) if heads else 0), 'end': fmt_ts(max(tails) if tails else 0)}


def read_conf(src, fmt):
    """读会话的采集配置（ISuit-V3：datacollect_conf.yaml；UMI_v3：metadata/conf.yaml）。读不出/不是字典返回 {}。"""
    try:
        conf = yaml.safe_load(read_text(Path(src) / sdk.FORMATS[fmt]['conf']))
        return conf if isinstance(conf, dict) else {}
    except Exception:
        return {}


def txt(v):
    """配置值转文本入库：None→''，列表（如 UMI_v3 的 sub_task: []）用「、」连接，其余原样。"""
    if v is None: return ''
    if isinstance(v, (list, tuple)): return '、'.join(str(x) for x in v if x is not None)
    return v


_LOG_TS = re.compile(r'^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})(\.\d+)? ([+-]\d{4})\b')
_LOG_DUR = re.compile(r'操作录制时长秒=([0-9.]+)')


def _log_epoch(line):
    m = _LOG_TS.match(line)
    if not m: return None
    try: return datetime.datetime.strptime(m.group(1) + (m.group(2) or '.0') + ' ' + m.group(3), '%Y-%m-%d %H:%M:%S.%f %z').timestamp()
    except ValueError: return None


def umi_log_times(src):
    """metadata/recording.log（采集程序写的录制日志）→ (开始, 结束, 时长秒)，读不到的为 None。
    「INFO 录制开始」行的时间 = 开始；「INFO 停止录制」行 = 结束；「操作录制时长秒=651.50」= 时长（缺时用 结束-开始）。时间带时区（+0800）。"""
    start = end = dur = None
    try: text = read_text(Path(src) / 'metadata' / 'recording.log')
    except Exception: return None, None, None
    for line in text.splitlines():
        if start is None and ' INFO 录制开始' in line: start = _log_epoch(line)
        elif ' INFO 停止录制' in line: end = _log_epoch(line)
        m = _LOG_DUR.search(line)
        if m:
            try: dur = float(m.group(1))
            except ValueError: pass
    if not dur and start and end and end > start: dur = end - start
    return start, end, (dur if dur and dur > 0 else None)


def _first_last(path):
    """csv 的表头、第一条数据行、最后一行（只读文件头尾，不整个读）。"""
    with open(path, 'rb') as f:
        head, first = f.readline(), f.readline()
        f.seek(0, 2); size = f.tell(); f.seek(max(0, size - 4096)); tail = f.read().splitlines()
    last = next((l for l in reversed(tail) if l.strip()), b'')
    d = lambda b: b.decode('utf-8', 'replace').strip()
    return d(head), d(first), d(last)


def umi_frame_times(src):
    """备用：各相机 <采集目录>/*.frames.csv 里 exposure_ns（Unix 纳秒）的最早/最晚 → (开始, 结束) 秒；读不到返回 (None, None)。"""
    lo, hi = [], []
    try:
        for d in os.scandir(src):
            if not d.is_dir(follow_symlinks=False) or d.name == 'metadata': continue
            for f in os.scandir(d.path):
                if not (f.name.endswith('.frames.csv') and f.is_file()): continue
                try:
                    head, first, last = _first_last(f.path)
                    col = head.split(',').index('exposure_ns')
                    if first and last and last != head: lo.append(int(first.split(',')[col]) / 1e9); hi.append(int(last.split(',')[col]) / 1e9)
                except (OSError, ValueError, IndexError): continue
    except OSError: pass
    return (min(lo), max(hi)) if lo else (None, None)


def umi_times(src, name):
    """UMI_v3 没有分片 yaml，采集起止与时长按优先级取：
    ① metadata/recording.log（开始/停止时间 + 「操作录制时长秒」）；② 各相机 frames.csv 的 exposure_ns 最早到最晚；
    ③ 都没有 → 开始取目录名 UMI_v3_YYYYMMDD_HHMMSS（北京时间），时长 0（未知，不会因「时长 < 3 分钟」被判成标定）。
    注意 conf.yaml 里的 payload.task.duration_seconds 是计划时长，不是实际时长，不用。返回 {'duration','start','end'}。"""
    start, end, dur = umi_log_times(src)
    if dur: end = end or (start + dur if start else None)
    else:
        fs, fe = umi_frame_times(src)
        start, end, dur = (fs, fe, fe - fs) if fs and fe and fe >= fs else (None, None, 0.0)
    if not start:
        m = sdk.UMI_V3_RE.match(name)
        start = datetime.datetime.strptime(m.group(1) + m.group(2), '%Y%m%d%H%M%S').replace(tzinfo=TZ).timestamp() if m else None
    return {'duration': round(dur, 2), 'start': fmt_ts(start or 0), 'end': fmt_ts(end or 0)}


def local_prt(src):
    """本地路径里的 PRT 目录：(目录名, 目录路径)。没有 PRT 目录时返回 ('', 会话的上一级目录)。点击「房间」打开的就是这个目录。"""
    parts = Path(src).parts
    for i in range(len(parts) - 2, -1, -1):
        if sdk.PRT_RE.match(parts[i]): return parts[i], str(Path(*parts[:i + 1]))
    return '', str(Path(src).parent)


def device_dir(src):
    """设备文件夹名：路径里 umi_normal / umi_calibration 的下一级（如 ...\\umi_normal\\1CF3\\20261111\\PRT_WX_room002\\ISuit… → 1CF3）。找不到返回 ''。"""
    parts = Path(src).parts; names = {n for ns in LOCAL_DIRS.values() for n in ns}
    for i in range(len(parts) - 2, -1, -1):
        if parts[i].lower() in names:
            d = parts[i + 1] if i + 1 < len(parts) - 1 else ''
            return '' if re.fullmatch(r'\d{8}', d) or sdk.PRT_RE.fullmatch(d) else d                # 8 位数字是日期文件夹、PRT_城市_room… 是房间文件夹：说明这份数据没有设备这一层
    return ''


def _mmss(sec):
    sec = int(round(sec or 0)); return '%02d:%02d' % (sec // 60, sec % 60)


def calib_reasons(duration, slices, task, declared=''):
    """判定「这是标定数据」的依据（任一成立）：① 时长 < 3 分钟（分片 yaml 读不出时间、时长为 0 即未知时，不按此条判定）② 任务名含「标定」③ 采集配置自己声明（record_purpose=Start/End/Calibration 或 location_type=标定）。"""
    r = []
    if 0 < duration < CAL_MAX_SEC: r.append('时长 %s < 3 分钟' % _mmss(duration))
    if '标定' in (task or ''): r.append('任务名含「标定」')
    if declared: r.append(declared)
    return r


def inspect(ds, prev=None, pad=False):
    """统计一个会话。prev = 上次扫描的同一路径记录；签名一致则复用统计，只重算与路径相关的字段。返回的记录带 cached 标记（不入库）。"""
    src = ds['source']; sig = session_sig(src)
    fmt = ds.get('fmt') or sdk.fmt_of_name(ds['name']) or 'isuit'
    if prev and sig and prev.get('sig') == sig and prev.get('size') is not None:
        st, cached = {k: prev[k] for k in CONF_FIELDS}, True
    else:
        conf = read_conf(src, fmt); g = lambda k, d='': conf.get(k, d)
        st = stats(src, src.name, fmt == 'isuit'); cached = False
        if fmt == 'umi_v3': st.update(umi_times(src, src.name))
        purpose, ltype = str(g('record_purpose')), str(g('location_type'))
        decl = ('record_purpose=%s' % purpose) if purpose in ('Start', 'End', 'Calibration') else '位置类型=标定' if ltype == '标定' else ''
        task = g('task_name', None) or g('sub_task', None) or ''
        if fmt == 'umi_v3': task = ' / '.join(str(x) for x in (g('task_name', None), g('task_title', None)) if x) or txt(g('sub_task', None))    # task_name=项目名，task_title=具体任务名；判标定两个都看
        why = calib_reasons(st['duration'], st['slices'], task, decl)
        # 错放判定只用于 ISuit-V3。UMI_v3 的标定会话无法靠 conf/时长自动区分（要看视频里有没有二维码，时长通常 < 3 分钟只是参考），
        # 所以 UMI_v3 不自动判错放（misplaced 恒为空），由用户在页面里看视频后手动「移到标定 / 移回常规」。
        st.update(task=task, sub_task=txt(g('sub_task', None)), device_sn=str(g('device_SN')), purpose=purpose,
                  operator=g('record_operator', None) or '', location=g('record_location', None) or '',
                  misplaced='' if fmt == 'umi_v3' else ('标定数据放在 umi_normal：' + '；'.join(why)) if ds['type'] == 'normal' and why else '常规数据放在 umi_calibration' if ds['type'] == 'calibration' and not why else '')
    rel = rel_of(ds['raw_uri'])
    if pad: rel = pad_room(rel)                                     # 可选：OSS 路径里的房间号补零成 3 位（不改本地目录）
    prt_local, prt_dir = local_prt(src)
    dev = device_dir(src) or ('' if fmt == 'umi_v3' else st['device_sn'][-4:])      # 设备 = 数据所在的设备文件夹名（两种格式本地层级相同：umi_normal\\{设备}\\{日期}\\PRT_…\\会话）。ISuit-V3 没有该层时取 SN 后 4 位；UMI_v3 的设备文件夹与设备SN 没有关系、仅作参考，没有该层就留空
    return dict(st, device=dev, id=hashlib.sha1(rel.encode()).hexdigest()[:12], rel=rel, type=ds['type'], name=ds['name'], source=str(src), cap_date=ds['capture_date'],
                dir_date=next((p for p in src.parts if re.fullmatch(r'\d{8}', p)), ''), room=rel.split('/')[1], prt_local=prt_local, prt_dir=prt_dir, sig=sig, cached=cached)


def find_dirs(root, depth=3):
    """在 root 下（含 root 自身、最多 depth 层）找 umi_normal / umi_calibration 目录，支持直接选整个盘符。"""
    names = {n: t for t, ns in LOCAL_DIRS.items() for n in ns}
    if Path(root).name in names:
        return [(names[Path(root).name], root)]
    out = []

    def walk(p, d):
        try:
            ents = list(os.scandir(p))
        except OSError:
            return
        for e in ents:
            if not e.is_dir(follow_symlinks=False):
                continue
            if e.name in names:
                out.append((names[e.name], e.path))
            elif d < depth and not e.name.startswith(('.', '$')) and e.name.lower() not in SKIP_DIRS:
                walk(e.path, d + 1)
    walk(root, 0)
    return out


def row_info(r):
    """数据行提示（只提示，不拦截）：
    ① date_warn  session 目录名日期（OSS 路径 raw_data_YYMMDD 由它得出）≠ 所在日期文件夹（归档日期）
    ② room_bad   OSS 路径里的房间不是 PRT_城市_room三位数 格式（如 PRT_WX_room2，应为 PRT_WX_room002）
    ③ room_diff  OSS 路径里的房间 ≠ 本地目录里的 PRT（被界面城市/房间覆盖，或勾选了补零）
    ④ dev_diff   设备文件夹名 ≠ 设备SN 后 4 位（设备列取文件夹名，写错时在「设备」格橙色提示）"""
    cap = r['cap_date']
    dw = ('目录日期 %s 与 session 目录名日期 %s 不一致（上传路径按 session 日期 raw_data_%s）' % (r['dir_date'], cap, sdk.to_yymmdd(cap))
          if r.get('dir_date') and r['dir_date'] != cap.replace('-', '') else '')
    room, local = r['room'], r.get('prt_local') or ''
    bad = '' if ROOM_OK.fullmatch(room) else '房间 %s 不是 3 位房间号格式，应为 %s（可点击打开文件夹改名后重新扫描，或扫描时勾选「补零」）' % (room, pad_room(room))
    diff = 'OSS 路径使用 %s，本地目录是 %s' % (room, local) if local and local != room else ''
    sn4, dev = (r.get('device_sn') or '')[-4:], r.get('device') or ''
    dd = '设备文件夹 %s 与设备SN %s 的后 4 位 %s 不一致（文件夹名可能写错，设备列取的是文件夹名）' % (dev, r['device_sn'], sn4) if sn4 and dev.upper() != sn4.upper() and sdk.fmt_of_name(r['name']) != 'umi_v3' else ''     # UMI_v3：设备文件夹与设备SN 没有关系，不比对
    return {'date_warn': dw, 'room_bad': bad, 'room_diff': diff, 'prt_warn': diff, 'dev_diff': dd}


def scan_view():
    with PL: v = dict(SCAN)
    v['elapsed'] = round(((time.time() if v['running'] else v['t1']) - v['t0']) if v['t0'] else 0, 1)
    return v


def run_scan(b, rs):
    """后台扫描：查找目录 → 识别数据集 → 逐个统计会话（有缓存的跳过）；进度写入 SCAN，供页面轮询显示。
    城市+房间都填时覆盖本地 PRT 目录（显式覆盖：页面不再保存/恢复这两个值）；都空则取本地路径里的 PRT_… 目录。"""
    res = None
    try:
        apply_cfg(); routes, problems = [], []
        city, room, full, pad = (b.get('city') or '').strip(), (b.get('room') or '').strip(), bool(b.get('full')), bool(b.get('pad'))
        ov = {'city': city, 'room': room} if city and room else {}
        if (city or room) and not ov: problems.append('城市和房间号需同时填写才会覆盖本地路径里的 PRT 目录，已忽略')
        for i, r in enumerate(rs):
            with PL: SCAN.update(stage='查找目录 %d/%d' % (i + 1, len(rs)))
            found = find_dirs(r) if os.path.isdir(r) else []
            if not found: problems.append('%s 下（3 层内）没有 umi_normal / umi_calibration' % r)
            routes += [dict({t: p}, **ov) for t, p in found]
        with PL: SCAN.update(stage='识别数据集')
        datasets, pr = sdk.discover(routes, SimpleNamespace(date=None, city=city, room=room)) if routes else ([], [])
        with PL:
            SCAN.update(stage='统计会话', total=len(datasets), done=0)
            prev = {} if full else {r['source']: r for r in DATASETS.values()}

        def one(ds):
            r = inspect(ds, prev.get(str(ds['source'])), pad)
            with PL: SCAN['done'] += 1
            return r
        with cf.ThreadPoolExecutor(min(32, (os.cpu_count() or 4) * 4)) as pool:
            recs = list(pool.map(one, datasets))
        seen = {}
        for r in recs:
            if r['id'] in seen: problems.append('重复数据集(同一 OSS 路径): %s 与 %s' % (seen[r['id']]['source'], r['source']))
            seen[r['id']] = r
        cached = sum(1 for r in recs if r.pop('cached'))
        over = sum(1 for r in recs if r['prt_local'] and r['prt_local'] != r['room'])
        if over and ov: problems.append('界面城市/房间覆盖了 %d 个数据集的 PRT（OSS 路径与本地目录名不同，列表里已标出）。要让路径跟随文件夹名，请清空城市/房间后重新扫描' % over)
        with PL:                                                    # 两种格式（ISuit-V3 / UMI_v3）一次扫描一起识别，整体替换
            DATASETS.clear(); DATASETS.update(seen); PROBLEMS[:] = problems + pr
        save_datasets(list(seen.values()))
        res = {'count': len(seen), 'problems': len(problems) + len(pr), 'cached': cached, 'scanned': len(recs) - cached, 'overridden': over if ov else 0}
    except Exception as e:
        res = {'error': str(e)}
    finally:
        with PL: SCAN.update(running=False, stage='完成', t1=time.time(), result=res)
