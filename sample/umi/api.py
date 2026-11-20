# -*- coding: utf-8 -*-
"""HTTP 接口（Flask）。前端静态文件在 web/，由这里提供。"""
import collections
import datetime
import json
import urllib.parse
import os
import re
import string
import subprocess
import threading
import time
import upload_umi_sdk as sdk
from flask import Flask
from flask import jsonify
from flask import request
from flask import Response
from flask import send_file
from flask import send_from_directory
from . import state as S
from .state import TZ, DATASETS, HERE, JOBS, LOCAL_DIRS, MOVING, OK_STATES, PL, PROBLEMS, SCAN, SDK_DEFAULTS, STATE_DIR
from .db import apply_cfg, dbx, get_cfg, interrupted_notice, set_setting, start_run
from .lifecycle import shutdown_later
from .xlsx import build_xlsx
from .ossutil_client import OssutilOss, write_ossutil_cfg
from .util import fmt_ts, full_uri, mlabel, open_folder, resolve_ossutil
from .progress import Meter, log, prog_view
from .scan import local_prt, row_info, run_scan, scan_view
from .mover import move_ids, plans
from .videos import list_videos, resolve_video
from .nettune import estimate, recommend
from .jobs import run_job


START = threading.Lock()                                       # 启动类操作（扫描/上传校验/移动）的「检查+登记」互斥，持有时间很短
WEB_DIR = HERE / 'web'
app = Flask(__name__, static_folder=str(WEB_DIR), static_url_path='/static')
app.config['SEND_FILE_MAX_AGE_DEFAULT'] = 0                 # 改了前端文件刷新即生效


# ------------------------------------------------------------ API
def buckets():
    """两种数据格式各自的目标桶（目录名决定格式，格式决定桶）：{isuit: hw-upload, umi_v3: prt-huawei}。"""
    return {f: sdk.bucket_for(f) for f in sdk.FORMATS}


@app.get('/api/config')
def cfg_get():
    c = get_cfg(); c['has_sk'] = bool(c.pop('sk'))
    return jsonify(dict(c, buckets=buckets(), formats={f: v['label'] for f, v in sdk.FORMATS.items()}))


CFG_NUM = {'jobs': (1, 256), 'parallel': (1, 256), 'ds_jobs': (1, 32), 'wan': (1, 16), 'net_wait': (0, 10080)}      # 数值型配置的范围（net_wait 单位：分钟）
CFG_ENUM = {'method': ('sdk', 'ossutil'), 'order': ('folder', 'pipeline'), 'ossutil_unit': ('slice', 'session')}


def cfg_check(k, v):
    """校验一项配置，返回错误说明（合法返回 ''）。空值 = 恢复默认。"""
    if v == '': return ''
    if k in ('bucket', 'bucket_umi') and not re.fullmatch(r'[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]', v): return 'Bucket 只能含小写字母、数字、- 和 .（3~63 位），例如 hw-upload / prt-huawei'
    if k in CFG_ENUM and v not in CFG_ENUM[k]: return '%s 只能是 %s' % (k, ' / '.join(CFG_ENUM[k]))
    if k in CFG_NUM:
        lo, hi = CFG_NUM[k]
        try: n = float(v)
        except ValueError: return '%s 必须是数字' % k
        if not lo <= n <= hi or (k != 'net_wait' and n != int(n)): return '%s 必须是 %d ~ %d 的%s' % (k, lo, hi, '数' if k == 'net_wait' else '整数')
    return ''


@app.post('/api/config')
def cfg_set():
    b = {k: str(v).strip() for k, v in (request.json or {}).items() if k in ('endpoint', 'bucket', 'bucket_umi', 'ak', 'sk', 'jobs', 'parallel', 'method', 'order', 'ds_jobs', 'wan', 'ossutil_unit', 'ossutil_path', 'ossutil_args', 'net_wait')}
    for k, v in b.items():
        e = cfg_check(k, v)
        if e: return jsonify(error=e), 400
    cur = get_cfg()
    if any(k in b and (b[k] or SDK_DEFAULTS[k]) != cur[k] for k in ('bucket', 'bucket_umi')):     # 任务/扫描进行中换桶会让正在传的数据集路径错乱
        with PL:
            if SCAN['running'] or any(not j['done'] for j in JOBS.values()): return jsonify(error='扫描/上传/校验进行中，暂时不能更换 Bucket'), 409
    for k, v in b.items():
        if k == 'sk' and not v: continue                         # 密钥留空 = 不修改
        if v == '' and (k in CFG_NUM or k in CFG_ENUM or k in ('bucket', 'bucket_umi')): dbx('DELETE FROM settings WHERE k=?', (k,))      # 数值/枚举/桶留空 = 恢复默认
        else: dbx('INSERT OR REPLACE INTO settings VALUES(?,?)', (k, v))
    apply_cfg()
    return jsonify(ok=True, buckets=buckets())


@app.get('/api/net/plan')
def net_plan():
    """多出口并发建议：?wan=出口数 → 建议的 ds_jobs/jobs/parallel 与估算连接数；同时按 ?ds_jobs&parallel（缺省取当前配置）估算「当前设置」能有多少条并发连接。"""
    c, a = get_cfg(), request.args
    try:
        wan = min(max(int(a.get('wan') or c['wan']), 1), 16)
        ds, par = int(a.get('ds_jobs') or c['ds_jobs']), int(a.get('parallel') or c['parallel'])
    except ValueError: return jsonify(error='参数必须是整数'), 400
    return jsonify(wan=wan, recommend=recommend(wan), current=dict(estimate(ds, par, wan), ds_jobs=ds, parallel=par))


@app.post('/api/config/test')
def cfg_test():
    """连接测试：两个桶（ISuit-V3 桶、UMI_v3 桶）都测。方式=ossutil 时只用 ossutil（不需要 oss2/boto3）；方式=SDK 时用 sdk.Oss。"""
    apply_cfg(); c = get_cfg(); out, ok = [], True
    try:
        if c['method'] == 'ossutil':
            c = dict(c, ossutil_path=resolve_ossutil(c['ossutil_path'])); c['_cfg'] = write_ossutil_cfg(c)
            for f, b in buckets().items():
                try: OssutilOss(dict(c, bucket=b)).test(); out.append('%s → %s：成功' % (sdk.FORMATS[f]['label'], b))
                except Exception as e: ok = False; out.append('%s → %s：失败 %s' % (sdk.FORMATS[f]['label'], b, str(e)[:160]))
            r = subprocess.run([c['ossutil_path'], '--version'], capture_output=True, text=True, timeout=15)
            return jsonify(ok=ok, msg=('连接成功' if ok else '有桶连接失败') + '（ossutil，%s）：%s；%s' % (sdk.ENDPOINT, '；'.join(out), ((r.stdout or r.stderr).strip().splitlines() or [''])[0][:80]))
        class Q:
            def line(self, *a, **k): pass
        for f, b in buckets().items():
            try:
                o = sdk.Oss(STATE_DIR, 1, 1, Q(), b)
                o.boto3_client.head_bucket(Bucket=b) if o.is_s3 else o.bucket.get_bucket_info()
                out.append('%s → %s：成功' % (sdk.FORMATS[f]['label'], b))
            except Exception as e: ok = False; out.append('%s → %s：失败 %s' % (sdk.FORMATS[f]['label'], b, str(e)[:160]))
        return jsonify(ok=ok, msg=('连接成功' if ok else '有桶连接失败') + '（SDK，%s）：%s' % (sdk.ENDPOINT, '；'.join(out)))
    except Exception as e:
        return jsonify(ok=False, msg='失败：%s' % e)


@app.get('/api/browse')
def browse():
    p = request.args.get('path', '')
    if not p and os.name == 'nt':
        return jsonify(path='', parent=None, hint=[], dirs=[{'name': d + ':\\', 'full': d + ':\\'} for d in string.ascii_uppercase if os.path.isdir(d + ':\\')])
    p = os.path.abspath(p or '/')
    try:
        dirs = [{'name': e.name, 'full': e.path} for e in sorted(os.scandir(p), key=lambda e: e.name.lower()) if e.is_dir(follow_symlinks=False) and not e.name.startswith('.')]
    except OSError as e:
        return jsonify(error=str(e)), 400
    par = os.path.dirname(p)
    return jsonify(path=p, parent=(par if par != p else ('' if os.name == 'nt' else None)), dirs=dirs,
                   hint=[n for d in LOCAL_DIRS.values() for n in d if os.path.isdir(os.path.join(p, n))])


@app.post('/api/roots')
def roots():
    b = request.json or {}
    for r in b.get('add', []): dbx('INSERT OR IGNORE INTO roots VALUES(?)', (r,))
    for r in b.get('remove', []): dbx('DELETE FROM roots WHERE path=?', (r,))
    return jsonify(ok=True)


@app.post('/api/scan')
def scan():
    b = request.json or {}
    rs = [r['path'] for r in dbx('SELECT path FROM roots ORDER BY path')]
    if not rs:
        return jsonify(error='请先添加数据根目录'), 400
    with START, PL:                                             # 防重复点击：同一时间只允许一个扫描，且不与上传/校验/移动并行
        if SCAN['running']: return jsonify(error='正在扫描，请勿重复点击'), 409
        if MOVING.locked(): return jsonify(error='正在移动数据，请稍候'), 409
        if any(not j['done'] for j in JOBS.values()): return jsonify(error='有上传/校验任务在运行，暂时不能扫描'), 409
        SCAN.update(id=SCAN['id'] + 1, running=True, stage='准备', done=0, total=0, t0=time.time(), t1=0.0, result=None)
    threading.Thread(target=run_scan, args=(b, rs), daemon=True).start()
    return jsonify(ok=True)


@app.post('/api/run')
def run():
    b = request.json or {}
    with PL: action, recs = b.get('action'), [DATASETS[i] for i in b.get('ids', []) if i in DATASETS]
    if action not in ('upload', 'verify', 'verify_deep') or not recs:
        return jsonify(error='参数无效或未选择数据'), 400
    with START:                                                 # 「检查有无进行中的操作 → 登记任务」在一把锁里，两次点击/扫描/移动不会同时通过检查
        return _start_job(action, recs, b)


def _start_job(action, recs, b):
    with PL:
        if SCAN['running']: return jsonify(error='正在扫描，请稍候'), 409
        if MOVING.locked(): return jsonify(error='正在移动数据，请稍候'), 409
        if any(not j['done'] for j in JOBS.values()): return jsonify(error='已有任务在运行'), 409
    skipped = 0
    if action == 'upload' and not b.get('force'):
        done = {r['rel'] for r in dbx("SELECT rel FROM uploads WHERE status IN ('成功','已存在')")} | {r['rel'] for r in dbx('SELECT rel FROM checks WHERE ready=1')}
        kept = [r for r in recs if r['rel'] not in done]; skipped, recs = len(recs) - len(kept), kept
        if not recs: return jsonify(error='所选数据均已上传（勾选「含已上传」可重新核对）'), 400
    recs.sort(key=lambda r: (r['start'], r['rel']))                     # 按采集时间先后依次开始
    deep, c = action == 'verify_deep', get_cfg()
    units = [max(2 * r['size'], 1) if action == 'upload' else (r['size'] + 1 if deep else 1) for r in recs]
    job = {'id': str(time.time()), 'action': action, 'count': len(recs), 'log': [], 'done': False, 't0': time.time(), 't_end': None, 'base': 0, 'units': units,
           'total_units': sum(units), 'meter': Meter(), 'actives': [], 'items': [{'rel': r['rel'], 'state': 'queued', 'msg': ''} for r in recs]}
    if skipped: log(job, '已跳过 %d 个已上传的数据集' % skipped)
    for r in recs:
        i = row_info(r)
        for m in (i['date_warn'], i['room_bad'], i['room_diff'], i['dev_diff']):
            if m: log(job, '⚠ %s：%s' % (r['name'], m))
    log(job, '开始 %s，共 %d 个数据集，方式: %s，数据集并发: %s' % (action, len(recs), mlabel(dict(c, order=c['order'])) if action == 'upload' else 'SDK', 1 if deep else c['ds_jobs']))
    job['run_id'] = start_run(action, mlabel(c) if action == 'upload' else 'SDK', 1 if deep else int(c['ds_jobs'] or 1), len(recs))
    with PL: JOBS.clear(); JOBS[job['id']] = job
    threading.Thread(target=run_job, args=(job, action, recs, c), daemon=True).start()
    return jsonify(ok=True)


@app.get('/api/export')
def export():
    """导出上传记录为官方客户端 upload-history 的 JSON 格式（浏览器下载，服务端不落盘）：按 类型+采集日期+PRT 目录 分组，每组一份。"""
    latest = {r['rel']: dict(r) for r in dbx('SELECT * FROM uploads ORDER BY id')}
    groups = collections.OrderedDict()
    for r in latest.values():
        name = r['rel'].split('/')[-1]
        groups.setdefault((r['type'], r['cap_date'] or sdk.session_date(name, sdk.fmt_of_name(name) or 'isuit') or '', r['oss_url'].rsplit('/' + name + '/', 1)[0]), []).append(r)
    out = []
    for (typ, cdate, prefix), rs in groups.items():
        items = [({'kind': 'dataset', 'source': r['source'], 'uri': None, 'status': 'failed', 'error': r['error']} if r['status'] == '失败' else
                  {'kind': 'dataset', 'source': r['source'], 'uri': r['oss_url'], 'status': 'submitted' if r['status'] == '成功' else 'already-ready',
                   'revision': r['revision'], 'fileCount': r['files']}) for r in rs]
        sub, ar, fl = (sum(i['status'] == k for i in items) for k in ('submitted', 'already-ready', 'failed'))
        out.append({'generatedAt': datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%S.%f')[:-3] + 'Z', 'type': typ, 'date': cdate, 'prefix': prefix,
                    'managed': True, 'counts': {'uploaded': sub + ar, 'submitted': sub, 'alreadyReady': ar, 'failed': fl}, 'items': items})
    return Response(json.dumps(out[0] if len(out) == 1 else out, ensure_ascii=False, indent=2), mimetype='application/json',
                    headers={'Content-Disposition': 'attachment; filename=umi-upload-history.json'})


def like(t):
    """LIKE 模式：用户输入里的 % _ 当普通字符（配合 ESCAPE '!'）。"""
    return '%' + t.replace('!', '!!').replace('%', '!%').replace('_', '!_') + '%'


def up_where(a):
    """上传记录的筛选条件（SQL）：类型/设备/采集日期/操作员/任务名/地点/状态/关键字。采集日期 = session 目录名里的日期。"""
    w, p = [], []
    for k in ('type', 'device', 'cap_date', 'operator'):
        if a.get(k): w.append('%s=?' % k); p.append(a[k])
    if a.get('fmt') == 'umi_v3': w.append("rel LIKE '%/UMI!_v3!_%' ESCAPE '!'")                      # 数据格式：看会话目录名（UMI_v3_… / ISuit-V3_…）
    elif a.get('fmt') == 'isuit': w.append("rel NOT LIKE '%/UMI!_v3!_%' ESCAPE '!'")
    for k in ('task', 'location'):
        if a.get(k): w.append("%s LIKE ? ESCAPE '!'" % k); p.append(like(a[k]))
    if a.get('q'):
        cols = ("REPLACE(source,'\\','/')", 'rel', 'task', 'sub_task', 'operator', 'location', 'device', 'device_sn')    # 会话目录名在 source/rel 里；\ 与 / 通用
        for t in a['q'].replace('\\', '/').split():                      # 空格分隔多个词：每个词都要匹配
            w.append('(' + ' OR '.join("%s LIKE ? ESCAPE '!'" % c for c in cols) + ')'); p += [like(t)] * len(cols)
    st = a.get('status')
    if st == 'ok': w.append("status IN ('成功','已存在')")
    elif st: w.append('status=?'); p.append(st)
    return (' WHERE ' + ' AND '.join(w)) if w else '', p


def uploads_groups(w, p, col):
    """累计采集时长明细：按 col(device|operator) 分组，按数据集(rel)去重后求和。"""
    return [dict(r) for r in dbx(
        "SELECT %s AS k, MAX(device_sn) AS sn, COUNT(*) AS n, SUM(duration) AS duration, SUM(size) AS size FROM "
        "(SELECT rel, MAX(device) device, MAX(device_sn) device_sn, MAX(operator) operator, MAX(duration) duration, MAX(size) size FROM uploads%s GROUP BY rel) "
        "GROUP BY %s ORDER BY duration DESC" % (col, w, col), p)]


def rec_fmt(r):
    """上传记录的数据格式与桶：格式看会话目录名，桶取自当时上传的 oss_url（记录里是当时真正用的桶）。"""
    name = (r['rel'] or '').split('/')[-1]; m = re.match(r'^oss://([^/]+)/', r['oss_url'] or '')
    return {'fmt': sdk.fmt_of_name(name) or 'isuit', 'bucket': m.group(1) if m else ''}


@app.get('/api/uploads')
def uploads_q():
    """上传记录查询：分页明细（按采集时间倒序）+ 汇总 + 按设备/按人员明细。累计采集时长按数据集(rel)去重（同一数据集多次上传只算一次）。"""
    a = request.args; w, p = up_where(a)
    ps, pg = min(max(a.get('ps', 100, type=int), 1), 1000), max(a.get('page', 0, type=int), 0)
    rows = [dict(r, **rec_fmt(r)) for r in dbx('SELECT * FROM uploads' + w + ' ORDER BY start_time DESC, id DESC LIMIT ? OFFSET ?', p + [ps, pg * ps])]
    total = dbx('SELECT COUNT(*) FROM uploads' + w, p)[0][0]
    agg = dbx('SELECT COUNT(*), SUM(duration), SUM(size) FROM (SELECT rel, MAX(duration) duration, MAX(size) size FROM uploads' + w + ' GROUP BY rel)', p)[0]
    okw = (w + ' AND' if w else ' WHERE') + " status IN ('成功','已存在')"
    ok = dbx('SELECT COUNT(*), SUM(duration), SUM(size) FROM (SELECT rel, MAX(duration) duration, MAX(size) size FROM uploads' + okw + ' GROUP BY rel)', p)[0]
    facets = {k: [r[0] for r in dbx("SELECT DISTINCT %s FROM uploads WHERE %s<>'' ORDER BY 1" % (k, k))] for k in ('cap_date', 'operator', 'device')}
    return jsonify(rows=rows, total=total, datasets=agg[0], duration=agg[1] or 0, size=agg[2] or 0, ok_datasets=ok[0], ok_duration=ok[1] or 0, ok_size=ok[2] or 0,
                   facets=facets, by_device=uploads_groups(w, p, 'device'), by_operator=uploads_groups(w, p, 'operator'))


FILTER_LABELS = (('fmt', '格式'), ('type', '类型'), ('status', '状态'), ('device', '设备'), ('cap_date', '采集日期'), ('operator', '人员'), ('task', '任务名'), ('location', '地点'), ('q', '关键字'))


def _hms(sec):
    sec = int(round(sec or 0)); return '%02d:%02d:%02d' % (sec // 3600, sec // 60 % 60, sec % 60)


@app.get('/api/uploads/export')
def uploads_export():
    """按查询条件把上传记录导出为 Excel（.xlsx，只用标准库生成，不需要 openpyxl）。条件与 /api/uploads 相同，导出全部匹配记录（不分页）。
    工作表：上传记录（明细）/ 汇总（含查询条件）/ 按设备 / 按人员（累计采集时长，按数据集去重）。"""
    a = request.args; w, p = up_where(a)
    recs = [dict(r) for r in dbx('SELECT * FROM uploads' + w + ' ORDER BY start_time DESC, id DESC LIMIT 200000', p)]
    cols = [('采集时间', 19, lambda r: r['start_time']), ('状态', 8, lambda r: r['status']), ('类型', 6, lambda r: '常规' if r['type'] == 'normal' else '标定'),
            ('格式', 9, lambda r: sdk.FORMATS[rec_fmt(r)['fmt']]['label']), ('桶', 12, lambda r: rec_fmt(r)['bucket']),
            ('设备', 7, lambda r: r['device']), ('设备SN', 11, lambda r: r['device_sn']), ('房间', 16, lambda r: r['room']), ('目录日期', 10, lambda r: r['dir_date']),
            ('采集日期', 11, lambda r: r['cap_date']), ('任务名', 26, lambda r: r['task']), ('子任务', 20, lambda r: r['sub_task']), ('操作员', 14, lambda r: r['operator']),
            ('地点', 18, lambda r: r['location']), ('采集结束', 19, lambda r: r['end_time']), ('分片', 6, lambda r: r['slices']),
            ('采集时长', 10, lambda r: _hms(r['duration'])), ('采集时长(秒)', 12, lambda r: round(r['duration'] or 0, 2)), ('大小(MB)', 10, lambda r: round((r['size'] or 0) / 1048576, 2)),
            ('文件数', 7, lambda r: r['files']), ('方式', 14, lambda r: r['method']), ('耗时(s)', 8, lambda r: r['elapsed']),
            ('平均速度(MB/s)', 14, lambda r: round((r['avg_speed'] or 0) / 1048576, 2)), ('登记时间', 19, lambda r: r['created_at']),
            ('OSS 相对路径', 60, lambda r: r['rel']), ('OSS 全路径', 90, lambda r: r['oss_url'])]
    detail = [[c[0] for c in cols]] + [[c[2](r) for c in cols] for r in recs]
    agg = dbx('SELECT COUNT(*), SUM(duration), SUM(size) FROM (SELECT rel, MAX(duration) duration, MAX(size) size FROM uploads' + w + ' GROUP BY rel)', p)[0]
    okw = (w + ' AND' if w else ' WHERE') + " status IN ('成功','已存在')"
    ok = dbx('SELECT COUNT(*), SUM(duration) FROM (SELECT rel, MAX(duration) duration FROM uploads' + okw + ' GROUP BY rel)', p)[0]
    cond = '；'.join('%s=%s' % (lab, {'normal': '常规', 'calibration': '标定'}.get(a.get(k), a.get(k))) for k, lab in FILTER_LABELS if a.get(k)) or '（无，导出全部）'
    dur = agg[1] or 0
    summary = [['项目', '值'], ['导出时间', fmt_ts(time.time())], ['查询条件', cond], ['记录数', len(recs)], ['数据集数（去重）', agg[0]], ['累计采集时长', _hms(dur)],
               ['累计采集时长(小时)', round(dur / 3600, 2)], ['体积(GB)', round((agg[2] or 0) / 2 ** 30, 2)], ['其中已上传数据集数', ok[0]], ['其中已上传累计采集时长', _hms(ok[1] or 0)],
               ['说明', '累计采集时长按数据集去重（同一数据集多次上传只算一次）；记录按采集时间倒序']]

    def grp(col, with_sn):
        g = uploads_groups(w, p, col)
        head = [col == 'device' and '设备' or '人员'] + (['设备SN'] if with_sn else []) + ['数据集数', '累计采集时长', '累计采集时长(小时)', '占比', '体积(GB)']
        return [head] + [[x['k']] + ([x['sn']] if with_sn else []) + [x['n'], _hms(x['duration']), round((x['duration'] or 0) / 3600, 2),
                round(100 * (x['duration'] or 0) / dur, 1) if dur else 0.0, round((x['size'] or 0) / 2 ** 30, 2)] for x in g]
    data = build_xlsx([('上传记录', detail, [c[1] for c in cols]), ('汇总', summary, [24, 70], {'filter': False, 'freeze': False}),
                       ('按设备', grp('device', True), [10, 12, 10, 14, 16, 8, 10]), ('按人员', grp('operator', False), [16, 10, 14, 16, 8, 10])])
    name = '上传记录_%s.xlsx' % datetime.datetime.now(TZ).strftime('%Y%m%d-%H%M%S')
    return Response(data, mimetype='application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
                    headers={'Content-Disposition': "attachment; filename=upload_records.xlsx; filename*=UTF-8''%s" % urllib.parse.quote(name)})


@app.post('/api/open')
def open_dir():
    """点击「房间」打开本地 PRT 目录；点击「会话目录」（target=session）打开会话目录本身。只允许本机访问；路径只取自已扫描的数据集 / 上传记录，不接受任意路径。"""
    bad = _local_only()
    if bad: return bad
    b = request.json or {}
    rec = DATASETS.get(b.get('id', ''))
    src = rec['source'] if rec else next((r[0] for r in dbx("SELECT source FROM uploads WHERE rel=? AND source<>'' ORDER BY id DESC LIMIT 1", (b.get('rel', ''),))), None)
    if not src: return jsonify(error='找不到对应的本地目录'), 404
    path = src if b.get('target') == 'session' else local_prt(src)[1]       # target=session：会话目录本身（「会话目录」列）；默认：PRT 房间目录（「房间」格）
    if not os.path.isdir(path): return jsonify(error='目录不存在（可能已改名，请重新扫描）：%s' % path), 404
    try: open_folder(path)
    except Exception as e: return jsonify(error='无法打开：%s' % e), 500
    return jsonify(ok=True, path=path)


def _local_only():
    return None if request.remote_addr in ('127.0.0.1', '::1') else (jsonify(error='只能在本机使用'), 403)


@app.get('/api/move/plan')
def move_plan():
    """错放数据的移动预览（ISuit-V3 自动判定）：umi_normal 下被判定为标定的会话 → 目标路径（umi_calibration 同层级）。只读，不动文件。"""
    return jsonify(plans=plans())


def _move_to(b):
    """请求里的 to：缺省 None（自动模式）；normal / calibration = 手动移动（UMI_v3：用户看过视频后自己决定）。"""
    to = b.get('to') or None
    return to if to in (None, 'normal', 'calibration') else False


@app.post('/api/move/preview')
def move_preview():
    """手动移动的预览：{ids, to} → 每个会话的目标路径与是否可移动。只读。"""
    b = request.json or {}; to = _move_to(b)
    if not to: return jsonify(error='to 必须是 normal 或 calibration'), 400
    return jsonify(plans=plans(b.get('ids') or [], to))


@app.get('/api/videos')
def videos():
    """会话里的视频列表（给「点会话看视频」用）。仅本机。"""
    bad = _local_only()
    if bad: return bad
    with PL: rec = DATASETS.get(request.args.get('id', ''))
    if not rec: return jsonify(error='数据集不存在，请重新扫描'), 404
    if not os.path.isdir(rec['source']): return jsonify(error='本地目录不存在（可能已被移动/改名，请重新扫描）'), 404
    return jsonify(name=rec['name'], type=rec['type'], files=list_videos(rec['source']))


@app.get('/api/video')
def video():
    """流式返回会话里的一个视频（支持 Range，浏览器可拖动进度）。只允许会话目录内的视频文件。仅本机。"""
    bad = _local_only()
    if bad: return bad
    with PL: rec = DATASETS.get(request.args.get('id', ''))
    path = resolve_video(rec['source'], request.args.get('f', '')) if rec else None
    if not path: return jsonify(error='视频不存在'), 404
    return send_file(path, mimetype='video/webm' if path.lower().endswith('.webm') else 'video/mp4', conditional=True, max_age=0)


@app.post('/api/move')
def move():
    """把选中的错放标定数据移动到 umi_calibration（os.rename，不复制不覆盖）。仅本机；扫描/上传/校验进行中时拒绝。"""
    bad = _local_only()
    if bad: return bad
    b = request.json or {}; ids, to = b.get('ids') or [], _move_to(b)
    if not ids: return jsonify(error='未选择数据'), 400
    if to is False: return jsonify(error='to 必须是 normal 或 calibration'), 400
    with START, PL:                                             # 检查与抢占 MOVING 在同一把锁里，扫描/上传无法在两者之间插入
        if SCAN['running']: return jsonify(error='正在扫描，暂时不能移动'), 409
        if any(not j['done'] for j in JOBS.values()): return jsonify(error='有上传/校验任务在运行，暂时不能移动'), 409
        if not MOVING.acquire(blocking=False): return jsonify(error='正在移动数据，请稍候'), 409
    try: moved, failed = move_ids(ids, to)
    finally: MOVING.release()
    return jsonify(ok=not failed, moved=moved, failed=failed)


_INFO = {}                                                       # 数据行提示的缓存：id → (记录对象, 提示)。扫描/移动会换新的记录对象，旧缓存自然失效


def info_of(r):
    hit = _INFO.get(r['id'])
    if hit and hit[0] is r: return hit[1]
    if len(_INFO) > 2 * len(DATASETS) + 100: _INFO.clear()
    info = row_info(r); _INFO[r['id']] = (r, info)
    return info


def dataset_rows():
    up = {r['rel']: dict(r) for r in dbx('SELECT rel,status,created_at FROM uploads WHERE id IN (SELECT MAX(id) FROM uploads GROUP BY rel)')}
    ck = {r['rel']: r for r in dbx('SELECT * FROM checks')}
    rows = []
    with PL: allr = list(DATASETS.values())
    for r in allr:
        u, k = up.get(r['rel']), ck.get(r['rel']); rm = json.loads(k['detail']) if k else None
        if u and u['status'] in OK_STATES: st, at = '已上传', u['created_at']
        elif k and k['ready']: st, at = '已上传', k['checked_at']
        elif u: st, at = '上传失败', u['created_at']
        else: st, at = '未上传', ''
        if st == '已上传' and rm and rm['status'] == '未上传': st = '远端缺失'
        try: url = full_uri(r['rel'])
        except sdk.UploadError: url = ''                           # 目录名不是两种已知格式（兜底，不应出现）
        fm = sdk.fmt_of_name(r['name']) or 'isuit'
        rows.append(dict(r, up_state=st, up_at=at, remote=rm, oss_url=url, fmt=fm, bucket=sdk.bucket_for(fm), **info_of(r)))
    return rows


@app.get('/api/state')
def state():
    """页面状态。?light=1 只返回进度类信息（不含 datasets）：任务/扫描进行中页面每 0.6 秒轮询一次，数据集列表较大时（2 万条≈23MB）不必每次都带。"""
    j, c = next(iter(JOBS.values()), None), get_cfg()
    out = dict(problems=PROBLEMS, db=S.DB_PATH, notice=interrupted_notice(), roots=[r['path'] for r in dbx('SELECT path FROM roots ORDER BY path')],
               method=c['method'], order=c['order'], scan=scan_view(), unit=c['ossutil_unit'], ds_jobs=c['ds_jobs'], buckets=buckets(), formats={f: v['label'] for f, v in sdk.FORMATS.items()},
               job=None if not j else {'id': j['id'], 'action': j['action'], 'done': j['done'], 'log': j['log'][-250:], 'prog': prog_view(j)})
    if request.args.get('light') != '1': out['datasets'] = dataset_rows()
    return jsonify(out)


@app.post('/api/notice/ack')
def notice_ack():
    """确认「上次任务被中断」提示（不再显示）。"""
    n = interrupted_notice()
    if n: set_setting('ack_run', n['id'])
    return jsonify(ok=True)


@app.post('/api/shutdown')
def shutdown():
    """退出服务（等同于关闭 cmd 窗口）。有任务在跑时需 force=true 确认。"""
    if any(not j['done'] for j in JOBS.values()) and not (request.json or {}).get('force'):
        return jsonify(error='有任务正在运行，退出会中断它（已传完的数据不受影响，下次可继续）', running=True), 409
    shutdown_later()
    return jsonify(ok=True)


@app.get('/')
def index():
    return send_from_directory(str(WEB_DIR), 'index.html')
