# -*- coding: utf-8 -*-
"""冒烟测试：假 OSS + 假 ossutil 跑完整业务流程，不需要网络/真实数据。
运行：python tests/smoke.py        （任意目录均可；失败时退出码非 0）
覆盖：建库/迁移 → 扫描(城市房间覆盖PRT、日期提示) → SDK/ossutil(逐分片/整会话)上传与顺序 → 跳过已上传 → 校验 → 记录筛选/汇总去重 → 导出 → 两种格式并存(§8：ISuit-V3→hw-upload、UMI_v3→prt-huawei，各自标定/上传/校验；视频预览；多出口并发建议) → 中断检测 → 子进程清理 → 静态文件。
给 AI：改了功能后先跑这个；新增功能请在末尾加对应用例（用 check(名称, 条件)）。"""
import json, os, shutil, sqlite3, stat, subprocess, sys, tempfile, time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)
TMP = tempfile.mkdtemp(prefix='umi_test_')
FAILS = []


def check(name, cond, extra=''):
    print(('  ✔ ' if cond else '  ✘ ') + name + ('' if cond else '  ' + str(extra)))
    if not cond: FAILS.append(name)


from umi import state
state.DB_PATH = os.path.join(TMP, 'test.db')
import upload_umi_sdk as sdk
import umi.uploader as up
import umi.api as api_mod
from umi.api import app
from umi.db import apply_cfg, dbx, init_db
from umi.lifecycle import register_child, terminate_children
from umi.progress import add_sent

R = os.path.join(TMP, 'remote') + os.sep
RB = lambda bucket: R if bucket == 'hw-upload' else R + '@' + bucket + os.sep       # 各桶的落盘目录：hw-upload 仍是 R（第 1~7 节不变），其他桶放在 R/@桶名/ 下
PROC_LOG = os.path.join(TMP, 'proc.log')
NETDOWN = os.path.join(TMP, 'netdown')           # 存在 = 假 ossutil 模拟断网


# ---------------------------------------------------------------- 假 OSS（落盘）与假 ossutil
class FakeOss:
    is_s3 = True
    seen = set()                                           # 本次创建过哪些桶的客户端（验证每个桶一个客户端）
    def __init__(self, state_dir=None, jobs=0, parallel=0, ui=None, bucket=None): self.bucket_name = bucket or sdk.BUCKET; FakeOss.seen.add(self.bucket_name)        # 每个桶一个客户端（与 sdk.Oss 一致）
    def _key(self, u):
        pre = 'oss://%s/' % self.bucket_name
        if not u.startswith(pre): raise sdk.UploadError('不是 %s 桶里的路径: %s' % (self.bucket_name, u))
        return u[len(pre):]
    def put_object(self, u, d):
        p = RB(self.bucket_name) + self._key(u); os.makedirs(os.path.dirname(p), exist_ok=True); open(p, 'wb').write(d)
        open(PROC_LOG, 'a').write('W %s\n' % self._key(u))
    def read_object(self, u):
        p = RB(self.bucket_name) + self._key(u); return open(p, 'rb').read() if os.path.exists(p) else None
    def list_objects(self, u):
        base = RB(self.bucket_name) + self._key(u); out = []
        for r, _, fs in os.walk(base):
            for f in fs: fp = os.path.join(r, f); out.append({'name': os.path.relpath(fp, base).replace(os.sep, '/'), 'size': os.path.getsize(fp)})
        return out


class BoomOss:
    """ossutil 方式下替换 sdk.Oss：任何 SDK 调用立刻失败 → 证明该方式不依赖 oss2 / boto3。"""
    def __init__(self, *a, **k): raise RuntimeError('ossutil 方式不应创建 SDK 客户端（需要 oss2/boto3）')


def fake_put(oss, local, key, size, job):
    add_sent(job, size); p = RB(oss.bucket_name) + key; os.makedirs(os.path.dirname(p), exist_ok=True); shutil.copy(local, p)


FAKE_OSSUTIL = r'''
import sys, os, shutil, time
a = sys.argv; cmd = a[1] if len(a) > 1 else ''
R = @@R@@; LOG = @@LOG@@; NET = @@NET@@
DOWN = os.path.exists(NET)                                         # 断网开关：文件存在 = 网络不通；内容为 cp 时只有 cp 写入和连通性探测(ls -d)失败，列表/读取正常（模拟传到一半断网）
MODE = open(NET).read().strip() if DOWN else ''
ERR = 'Error: Get "https://bucket.oss.example.com/": dial tcp 1.2.3.4:443: i/o timeout'
if '--version' in a: print('ossutil version: fake-1.7.19'); sys.exit(0)
assert '--config-file' in a and not any(x in a for x in ('-i', '-k', '-e')), a
key = lambda u: u.split('/', 3)[3]
RB = lambda b: R if b == 'hw-upload' else R + '@' + b + os.sep                 # 各桶落盘目录（与测试里的 RB 一致）
root_of = lambda u: RB(u.split('/')[2])
def log(tag, k): open(LOG, 'a').write('%s %.3f %s\n' % (tag, time.time(), k))
if cmd == 'ls':                                                    # ls oss://桶/前缀  （-d：连接测试）
    uri = a[2]
    if DOWN and ('-d' in a or MODE != 'cp'): print(ERR); sys.exit(1)
    if '-d' in a: print('oss://bucket/'); sys.exit(0)
    n = 0
    print('LastModifiedTime                   Size(B)  StorageClass   ETAG                  ObjectName')
    for r, ds, fs in os.walk(root_of(uri) + key(uri)):
        for f in sorted(fs):
            p = os.path.join(r, f); k = os.path.relpath(p, root_of(uri)).replace(os.sep, '/'); n += 1
            print('2026-11-05 11:20:00 +0800 CST %12d   Standard   %s   oss://%s/%s' % (os.path.getsize(p), 'a' * 32, uri.split('/')[2], k))
    print('Object Number is: %d' % n); sys.exit(0)
assert cmd == 'cp', a
src, dst = a[2], a[3]
if src.startswith('oss://'):                                       # 读：cp oss://… 本地
    if DOWN and MODE != 'cp': print(ERR); sys.exit(1)
    p = root_of(src) + key(src)
    if not os.path.isfile(p): print('Error: ServiceError: NoSuchKey The specified key does not exist.'); sys.exit(1)
    shutil.copy(p, dst); sys.exit(0)
assert dst.startswith('oss://') and '--disable-ignore-error' in a, a
k = key(dst); out = root_of(dst) + k; rec = '-r' in a
if rec: assert src.endswith(os.sep) and dst.endswith('/'), a
log('S', k); log('A', ' '.join(x for x in a if x.startswith('--jobs') or x.startswith('--parallel') or x.startswith('--part-size')))     # 记录并发相关参数
for n, pc in ((1, 20.1), (2, 55.5), (3, 100.0)):
    sys.stdout.write('\rTotal num: 3, size: 1,000. Dealed num: %d, Progress: %.3f%%, Speed: %.2fKB/s' % (n, pc, pc * 100)); sys.stdout.flush(); time.sleep(.05)
if rec:
    for r, ds, fs in os.walk(src):
        if '--only-current-dir' in a and r.rstrip(os.sep) != src.rstrip(os.sep): continue
        for f in fs:
            d = out + os.path.relpath(os.path.join(r, f), src).replace(os.sep, '/'); os.makedirs(os.path.dirname(d), exist_ok=True)
            if '--update' in a and os.path.exists(d): log('U', d); continue          # 续传：远端已有的文件跳过
            shutil.copy(os.path.join(r, f), d)
            if DOWN: print('\n' + ERR); sys.exit(1)                                   # 断网：已传一个文件后中断（模拟传到一半）
else:
    if DOWN: print(ERR); sys.exit(1)
    os.makedirs(os.path.dirname(out), exist_ok=True); shutil.copy(src, out)
log('E', k); print('\nSucceed: Total num ok')
'''


def make_ossutil():
    script = os.path.join(TMP, 'fake_ossutil.py'); open(script, 'w').write(FAKE_OSSUTIL.replace('@@R@@', repr(R)).replace('@@LOG@@', repr(PROC_LOG)).replace('@@NET@@', repr(NETDOWN)))
    if os.name == 'nt':
        w = os.path.join(TMP, 'fake_ossutil.cmd'); open(w, 'w').write('@"%s" "%s" %%*\r\n' % (sys.executable, script))
    else:
        w = os.path.join(TMP, 'fake_ossutil.sh'); open(w, 'w').write('#!/bin/sh\nexec "%s" "%s" "$@"\n' % (sys.executable, script)); os.chmod(w, 0o755)
    return w


def make_session(root, typ, folder, prt, name, operator, slices=2, t0=1794000000, sn='01351CA8', dev='1CA8'):
    s = os.path.join(*([root, typ] + ([dev] if dev else []) + [folder, prt, name])); os.makedirs(s, exist_ok=True)
    open(os.path.join(s, 'datacollect_conf.yaml'), 'w', encoding='utf-8').write('device_SN: %s\nrecord_purpose: Normal\nrecord_operator: %s\nrecord_location: 测试地点\ntask_name: 测试任务\nsub_task: 子任务A\n' % (sn, operator))
    for i in range(slices):
        d = os.path.join(s, '%s_%d' % (name, i)); os.makedirs(os.path.join(d, 'camera')); os.makedirs(os.path.join(d, 'imu'))
        open(os.path.join(d, '%s_%d.yaml' % (name, i)), 'w').write('head_time: %d.0\ntail_time: %d.0\n' % (t0 + 30 * i, t0 + 30 * i + 30))
        for f in 'abc': open(os.path.join(d, 'camera', f + '.record'), 'wb').write(os.urandom(50000))
        open(os.path.join(d, 'imu', 'l.record'), 'wb').write(os.urandom(2000))
    return s


def make_umi_session(root, typ, folder, prt, name, operator='zhangsan', conf=None, skip=(), root_file=False, log=None, frames=None, videos=(), dev='1CA8'):
    """造一个 UMI_v3 会话：metadata/conf.yaml + head / left hand / right hand（目录名带空格）。
    conf=None 用默认配置（task_name=项目名、task_title=任务名、sub_task 是列表，payload.task.duration_seconds=360 是「计划时长」，都与真实数据一致）；
    log=录制日志文本 → metadata/recording.log；frames=(开始秒, 结束秒) → head/camera-hl-head.frames.csv（exposure_ns）；skip 里的子目录/文件不创建（造残缺数据）。"""
    s = os.path.join(*([root, typ] + ([dev] if dev else []) + [folder, prt, name])); os.makedirs(s, exist_ok=True)       # 本地层级与 ISuit-V3 一样：umi_normal\{设备}\{日期}\PRT_…\会话
    for sub, files in (('head', ('a.bin', 'b.bin')), ('left hand', ('l.bin',)), ('right hand', ('r.bin',)), ('metadata', ())):
        if sub in skip: continue
        os.makedirs(os.path.join(s, sub), exist_ok=True)
        for f in files: open(os.path.join(s, sub, f), 'wb').write(os.urandom(30000))
    if 'metadata/conf.yaml' not in skip:
        os.makedirs(os.path.join(s, 'metadata'), exist_ok=True)
        open(os.path.join(s, 'metadata', 'conf.yaml'), 'w', encoding='utf-8').write(conf if conf is not None else
             'device_ID: UMI_v3\ndevice_SN: 01351CA8\nrecord_operator: %s\nrecord_location: 测试地点\ntask_name: 测试项目\ntask_title: 测试任务\nsub_task:\n  - 子任务A\n  - 子任务B\npayload: {task: {duration_seconds: 360}}\n' % operator)
        if log is not None: open(os.path.join(s, 'metadata', 'recording.log'), 'w', encoding='utf-8').write(log)
    if frames:
        rows = ['frame_index,sync_id,exposure_ns'] + ['%d,%d,%d' % (k, k + 1, int((frames[0] + (frames[1] - frames[0]) * k / 9) * 1e9)) for k in range(10)]
        open(os.path.join(s, 'head', 'camera-hl-head.frames.csv'), 'w').write('\n'.join(rows) + '\n')
    for v in videos: open(os.path.join(s, *v.split('/')), 'wb').write(bytes(range(256)) * 80)      # 20480 字节的假视频（仅用来测接口）
    if root_file: open(os.path.join(s, 'session.json'), 'w').write('{}')
    return s


def wait_job(c, timeout=60):
    t0 = time.time()
    while time.time() - t0 < timeout:
        j = c.get('/api/state').get_json()['job']
        if j and j['done']: return j
        time.sleep(.05)
    raise TimeoutError('任务超时')


def wait_scan(c):
    while c.get('/api/state').get_json()['scan']['running']: time.sleep(.03)


def reset_remote():
    shutil.rmtree(R, True); os.makedirs(R); open(PROC_LOG, 'w').close()


def upload_all(c, ids, **cfg):
    reset_remote()
    c.post('/api/config', json=dict({'jobs': '4', 'parallel': '2'}, **cfg))
    r = c.post('/api/run', json={'action': 'upload', 'ids': ids, 'force': True}); assert r.status_code == 200, r.get_json()
    return wait_job(c)


def main():
    sdk.Oss = FakeOss; up.put_file = fake_put
    data = os.path.join(TMP, 'data')
    make_session(data, 'umi_normal', '20261108', 'PRT_XZ_room001', 'ISuit-V3_2026-11-08-10-30-00', 'zhangsan', t0=1794000000, sn='01351CA8')      # 与用户示例一致
    make_session(data, 'umi_normal', '20261109', 'PRT_XZ_room001', 'ISuit-V3_2026-11-08-23-40-00', 'lisi', t0=1794040000, sn='01351B7C')          # 目录日期 ≠ session 日期
    make_session(data, 'umi_calibration', '20261108', 'PRT_WX_room003', 'ISuit-V3_2026-11-08-09-50-00', 'zhangsan', t0=1793990000)  # 本地 PRT ≠ 界面
    ossutil = make_ossutil()

    print('1 建库 / 迁移')
    init_db(); apply_cfg()
    names = {r[0] for r in dbx("SELECT name FROM sqlite_master WHERE type IN ('table','view')")}
    check('表齐全', {'settings', 'roots', 'datasets', 'runs', 'uploads', 'checks', 'v_latest_upload'} <= names, names)
    check('WAL + user_version=6', dbx('PRAGMA journal_mode')[0][0] == 'wal' and dbx('PRAGMA user_version')[0][0] == 6)
    old = os.path.join(TMP, 'v4.db'); cn = sqlite3.connect(old)                       # 造一个 v4 库：uploads 有数据，datasets 是 JSON 形式
    cn.executescript("CREATE TABLE settings(k TEXT PRIMARY KEY, v TEXT); CREATE TABLE roots(path TEXT PRIMARY KEY); CREATE TABLE datasets(rel TEXT PRIMARY KEY, data TEXT, scanned_at TEXT);"
                     "CREATE TABLE uploads(id INTEGER PRIMARY KEY AUTOINCREMENT, rel TEXT, oss_url TEXT, source TEXT, device TEXT, dir_date TEXT, task TEXT, operator TEXT, location TEXT, purpose TEXT, type TEXT, start_time TEXT, end_time TEXT, slices INTEGER, size INTEGER, duration REAL, files INTEGER, status TEXT, method TEXT, revision TEXT, elapsed INTEGER, avg_speed REAL, error TEXT, created_at TEXT); CREATE TABLE checks(rel TEXT PRIMARY KEY, status TEXT, ready INTEGER, deep INTEGER, detail TEXT, checked_at TEXT);"
                     "INSERT INTO uploads(rel,status) VALUES('umi_normal/PRT_WX_room002/ISuit-V3_2026-11-08-10-30-00','成功'); INSERT INTO settings VALUES('bucket','keepme'); PRAGMA user_version=4;"); cn.commit(); cn.close()
    keep = state.DB_PATH; state.DB_PATH = old; init_db()
    check('v4→v6 保留上传记录与配置', dbx('SELECT COUNT(*) FROM uploads')[0][0] == 1 and dbx("SELECT v FROM settings WHERE k='bucket'")[0][0] == 'keepme')
    check('v4→v6 新表/新列并回填', 'room' in [r[1] for r in dbx('PRAGMA table_info(datasets)')] and all(k in [r[1] for r in dbx('PRAGMA table_info(uploads)')] for k in ('run_id', 'cap_date', 'room', 'device_sn', 'sub_task')))
    u0 = dbx('SELECT cap_date, room FROM uploads')[0]; check('旧记录回填采集日期与房间', (u0[0], u0[1]) == ('2026-11-08', 'PRT_WX_room002'), tuple(u0))
    state.DB_PATH = keep; state.DATASETS.clear(); init_db(); apply_cfg()

    print('2 扫描')
    c = app.test_client()
    check('静态页面', c.get('/').status_code == 200 and 'UMI' in c.get('/').get_data(as_text=True) and c.get('/static/js/main.js').status_code == 200)
    c.post('/api/roots', json={'add': [data]})
    r = c.post('/api/scan', json={'city': 'xz', 'room': '001'}); check('扫描启动', r.status_code == 200)
    r2 = c.post('/api/scan', json={'city': 'xz', 'room': '001'}); check('重复点击被拒绝(409)', r2.status_code == 409)
    wait_scan(c); st = c.get('/api/state').get_json(); ds = {d['name'][-19:]: d for d in st['datasets']}
    check('3 个数据集', len(ds) == 3)
    ex = ds['2026-11-08-10-30-00']['oss_url']
    check('用户示例路径', ex == 'oss://hw-upload/raw_data/raw_data_261108/umi_normal/PRT_XZ_room001/ISuit-V3_2026-11-08-10-30-00/', ex)
    check('城市房间覆盖本地 PRT', ds['2026-11-08-09-50-00']['rel'].startswith('umi_calibration/PRT_XZ_room001/'))
    check('房间不一致提示（OSS≠本地）', bool(ds['2026-11-08-09-50-00']['room_diff']) and not ds['2026-11-08-10-30-00']['room_diff'])
    check('新字段 device_SN / sub_task / room', ds['2026-11-08-23-40-00']['device_sn'] == '01351B7C' and ds['2026-11-08-23-40-00']['sub_task'] == '子任务A' and ds['2026-11-08-23-40-00']['room'] == 'PRT_XZ_room001')
    check('日期不一致提示（目录日期≠session日期）', bool(ds['2026-11-08-23-40-00']['date_warn']) and not ds['2026-11-08-10-30-00']['date_warn'])
    check('采集日期取自 session 名', ds['2026-11-08-23-40-00']['cap_date'] == '2026-11-08')
    ids = [d['id'] for d in st['datasets']]
    check('扫描结果入库(datasets 逐列)', dbx('SELECT COUNT(*) FROM datasets')[0][0] == 3 and dbx('SELECT cap_date FROM datasets LIMIT 1')[0][0].startswith('2026'))

    print('3 SDK 上传（数据集并发 2）')
    j = upload_all(c, ids, method='sdk', order='folder', ds_jobs='2', ossutil_path=ossutil)
    check('全部成功', j['prog']['ok'] == 3 and j['prog']['fail'] == 0, j['prog'])
    W = [l.split()[1] for l in open(PROC_LOG) if l.startswith('W')]
    check('manifest 在 _READY 之前，且都在最后', all(W.index(k.replace('_READY', 'manifest.json')) < W.index(k) for k in W if k.endswith('_READY')))
    n_rec = dbx('SELECT COUNT(*) FROM uploads')[0][0]
    rec = dbx('SELECT * FROM uploads ORDER BY id DESC LIMIT 1')[0]
    check('上传记录含数据列表字段 + 全路径 + run_id', all(rec[k] for k in ('oss_url', 'task', 'operator', 'location', 'dir_date', 'start_time', 'run_id')) and rec['oss_url'].startswith('oss://'))
    check('runs 记录 done', dbx('SELECT status FROM runs ORDER BY id DESC LIMIT 1')[0][0] == 'done')
    r = c.post('/api/run', json={'action': 'upload', 'ids': ids}); check('已上传的默认跳过(400)', r.status_code == 400)

    print('4 ossutil（所有 OSS 操作都走 ossutil，不需要 oss2/boto3；逐分片 / 整会话 / 逐文件；单进程）')
    sdk.Oss = BoomOss                                                          # 任何 SDK 调用立刻失败
    def evs():
        ev = [l.split() for l in open(PROC_LOG)]; S_, E_ = [e for e in ev if e[0] == 'S'], [e for e in ev if e[0] == 'E']
        return S_, any(float(S_[i + 1][1]) < float(E_[i][1]) for i in range(len(S_) - 1))
    for unit in ('slice', 'session'):
        j = upload_all(c, ids[:1], method='ossutil', ossutil_unit=unit, ds_jobs='1', ossutil_path=ossutil)
        S_, overlap = evs(); keys = [e[2] for e in S_]
        check('ossutil %s 成功（未碰 SDK）、进程不重叠' % unit, j['prog']['ok'] == 1 and not overlap, (j['prog'], overlap, [l for l in j['log'] if '✘' in l]))
        check('%s：最后由 ossutil 写入 manifest.json 再写 _READY' % unit, keys[-2].endswith('/manifest.json') and keys[-1].endswith('/_READY'), keys[-3:])
        if unit == 'session': check('整会话 = 根目录预检 + 整会话 + manifest + _READY（4 个进程）', len(S_) == 4, len(S_))
    check('上传方式写入记录', 'ossutil·整会话' in [r['method'] for r in dbx('SELECT method FROM uploads')])
    src0 = state.DATASETS[ids[0]]['source']; os.makedirs(os.path.join(src0, '_pipeline')); open(os.path.join(src0, '_pipeline', 'x.json'), 'w').write('{}')
    j = upload_all(c, ids[:1], method='ossutil', ossutil_unit='session', ds_jobs='1', ossutil_path=ossutil)
    S_, overlap = evs(); n_remote = sum(len(fs) for _, _, fs in os.walk(R)) - 2
    check('本地含 _pipeline：改为 ossutil 逐文件上传（不退回 SDK），_pipeline 不上传', j['prog']['ok'] == 1 and any('逐文件上传' in l for l in j['log']) and not any('_pipeline' in r_ for r_, _, _ in os.walk(R)) and len(S_) == n_remote + 2 and not overlap, (len(S_), n_remote))
    shutil.rmtree(os.path.join(src0, '_pipeline'))
    j = upload_all(c, ids, method='ossutil', ossutil_unit='slice', ds_jobs='1', ossutil_path=ossutil)
    check('ossutil 全部上传成功', j['prog']['ok'] == 3 and j['prog']['fail'] == 0, j['prog'])
    c.post('/api/run', json={'action': 'verify', 'ids': ids}); wait_job(c)
    check('ossutil 方式快速校验一致（不碰 SDK）', {d['remote']['status'] for d in c.get('/api/state').get_json()['datasets']} == {'一致'})
    c.post('/api/run', json={'action': 'verify_deep', 'ids': ids}); wait_job(c)
    check('ossutil 方式深度校验一致（不碰 SDK）', all(d['remote']['deep'] and d['remote']['status'] == '一致' for d in c.get('/api/state').get_json()['datasets']))
    r_ = c.post('/api/config/test').get_json(); check('连接测试走 ossutil', r_['ok'] and 'ossutil' in r_['msg'], r_)
    print('4b 断网续传（ossutil：断网等待恢复 / 超时放弃 / 持久断点 / --update 跳过已传）')
    import umi.ossutil_client as oc
    oc.PROBE_MIN, oc.PROBE_MAX = 0.2, 0.4; up.CKPT_DIR = os.path.join(TMP, 'ckpt'); shutil.rmtree(up.CKPT_DIR, True)
    base = dict(method='ossutil', ossutil_unit='session', ds_jobs='1', ossutil_path=ossutil, jobs='4', parallel='2')
    def net_run(down_sec, mode='cp', **cfg):
        reset_remote(); c.post('/api/config', json=dict(base, **cfg)); open(NETDOWN, 'w').write(mode)
        r = c.post('/api/run', json={'action': 'upload', 'ids': ids[:1], 'force': True}); assert r.status_code == 200, r.get_json()
        if down_sec: time.sleep(down_sec); os.path.exists(NETDOWN) and os.remove(NETDOWN)
        return wait_job(c)
    j = net_run(1.5, mode='all', net_wait='1'); lg = '\n'.join(j['log'])
    check('一开始就断网（远端检查阶段）：等待恢复后继续，上传成功', j['prog']['ok'] == 1 and j['prog']['fail'] == 0 and '网络中断，等待恢复' in lg and '网络已恢复' in lg, [l for l in j['log'] if '✘' in l or '网络' in l])
    j = net_run(1.5, net_wait='1'); lg = '\n'.join(j['log'])
    check('传到一半断网：等待恢复后续传，数据集上传成功', j['prog']['ok'] == 1 and j['prog']['fail'] == 0 and '网络中断，等待恢复' in lg and '网络已恢复' in lg, [l for l in j['log'] if '✘' in l or '网络' in l])
    check('续传时 --update 跳过已传到 OSS 的文件', any(l.startswith('U ') for l in open(PROC_LOG)), open(PROC_LOG).read()[:1500])
    check('续传后远端完整（快速校验一致）', c.post('/api/run', json={'action': 'verify', 'ids': ids[:1], 'force': True}).status_code == 200 and wait_job(c)['prog']['ok'] == 1)
    check('提交成功后删除断点目录', not [f for _, _, fs in os.walk(up.CKPT_DIR) for f in fs], list(os.walk(up.CKPT_DIR)))
    j = net_run(0, net_wait='0.01'); lg = '\n'.join(j['log'])
    check('断网超过等待上限 → 失败并提示（保留断点）', j['prog']['fail'] == 1 and '网络中断超过' in lg and any(f == 'started' for _, _, fs in os.walk(up.CKPT_DIR) for f in fs), [l for l in j['log'] if '✘' in l])
    os.remove(NETDOWN); j = (c.post('/api/run', json={'action': 'upload', 'ids': ids[:1], 'force': True}), wait_job(c))[1]; lg = '\n'.join(j['log'])
    check('重新上传：读取上次断点，提示「断点续传」并成功（模拟重启后续传）', j['prog']['ok'] == 1 and '断点续传' in lg, [l for l in j['log'] if '✘' in l or '续传' in l])
    j = net_run(0, net_wait='0'); check('net_wait=0：不等待，3 次快速重试后失败', j['prog']['fail'] == 1 and '网络中断，等待恢复' not in '\n'.join(j['log']))
    os.path.exists(NETDOWN) and os.remove(NETDOWN); shutil.rmtree(up.CKPT_DIR, True); c.post('/api/config', json={'net_wait': '30'})
    check('配置项 net_wait 可保存', c.get('/api/config').get_json().get('net_wait') == '30', c.get('/api/config').get_json())
    bad = [c.post('/api/config', json={k: v}).status_code for k, v in (('jobs', 'abc'), ('jobs', '0'), ('ds_jobs', '3.5'), ('net_wait', '-1'), ('method', 'ftp'), ('ossutil_unit', 'x'))]
    check('配置校验：非法值(非数字/越界/非整数/未知枚举) → 400，不落库', bad == [400] * 6 and c.get('/api/config').get_json()['jobs'] != 'abc', bad)
    check('配置：合法值保存；留空 = 恢复默认', c.post('/api/config', json={'jobs': '8', 'net_wait': '5'}).status_code == 200 and c.get('/api/config').get_json()['jobs'] == '8'
          and c.post('/api/config', json={'jobs': '', 'net_wait': ''}).status_code == 200 and c.get('/api/config').get_json()['net_wait'] == '30')
    c.post('/api/config', json=dict(base, net_wait='30')); open(NETDOWN, 'w').write('all'); t0_ = time.time(); r_ = c.post('/api/config/test', json={}).get_json(); os.remove(NETDOWN)
    check('连接测试：断网时立即失败，不按 net_wait 等待', r_['ok'] is False and time.time() - t0_ < 10, (r_, time.time() - t0_))
    st_ = c.get('/api/state?light=1').get_json()
    check('/api/state?light=1：只含进度类信息，不含 datasets', 'datasets' not in st_ and 'scan' in st_ and 'job' in st_ and 'datasets' in c.get('/api/state').get_json())
    nm = c.get('/api/state').get_json()['datasets'][0]['name']; src0 = c.get('/api/state').get_json()['datasets'][0]['source']
    uq = lambda q: c.get('/api/uploads', query_string={'q': q}).get_json()['total']
    check('关键字：会话目录名 / 本地路径(\\ 与 / 通用) / 多个词(都要匹配)', uq(nm) >= 1 and uq(nm[-8:]) >= 1 and uq(os.path.dirname(src0).replace('/', '\\')) >= 1 and uq(nm + ' 测试任务') >= 1 and uq(nm + ' 不存在的词') == 0, [uq(nm), uq(nm[-8:])])
    check('关键字里的 % _ 当普通字符（不当 LIKE 通配符）', uq('%') == 0 and uq(nm[:-2] + '_' + nm[-1]) == 0 and uq(nm[:-2] + nm[-2:]) >= 1, (uq('%'), uq(nm[:-2] + '_' + nm[-1])))
    sdk.Oss = FakeOss

    print('5 校验')
    reset_remote(); j = upload_all(c, ids, method='sdk', ds_jobs='2')
    c.post('/api/run', json={'action': 'verify', 'ids': ids}); wait_job(c)
    check('快速校验一致', {d['remote']['status'] for d in c.get('/api/state').get_json()['datasets']} == {'一致'})
    c.post('/api/run', json={'action': 'verify_deep', 'ids': ids}); wait_job(c)
    check('深度校验一致', all(d['remote']['deep'] for d in c.get('/api/state').get_json()['datasets']))
    os.remove(next(os.path.join(r_, f) for r_, _, fs in os.walk(R) for f in fs if f.endswith('.record')))
    c.post('/api/run', json={'action': 'verify', 'ids': ids}); wait_job(c)
    check('远端缺文件 → 不一致', '不一致' in {d['remote']['status'] for d in c.get('/api/state').get_json()['datasets']})

    print('6 上传记录筛选 / 汇总 / 导出')
    u = c.get('/api/uploads').get_json()
    check('数据集去重汇总（同一数据集多次上传只算一次）', u['datasets'] == 3 and u['total'] > 3 and abs(u['duration'] - 3 * 60) < 1, (u['datasets'], u['total'], u['duration']))
    check('按操作员筛选', c.get('/api/uploads', query_string={'operator': 'lisi'}).get_json()['datasets'] == 1)
    check('按采集日期(session 名)筛选', c.get('/api/uploads', query_string={'cap_date': '2026-11-09'}).get_json()['total'] == 0 and c.get('/api/uploads', query_string={'cap_date': '2026-11-08'}).get_json()['datasets'] == 3)
    rows_ = c.get('/api/uploads').get_json()['rows']; st_ = [r_['start_time'] for r_ in rows_]
    check('上传记录按采集时间倒序', st_ == sorted(st_, reverse=True), st_)
    check('上传记录含 device_sn/sub_task/room/cap_date', all(rows_[0][k] for k in ('device_sn', 'sub_task', 'room', 'cap_date')))
    g = c.get('/api/uploads').get_json(); check('明细：按设备(文件夹名)/按人员（去重汇总=总计）', abs(sum(x['duration'] for x in g['by_device']) - g['duration']) < 1 and abs(sum(x['duration'] for x in g['by_operator']) - g['duration']) < 1 and {x['k'] for x in g['by_operator']} == {'zhangsan', 'lisi'} and {x['k'] for x in g['by_device']} == {'1CA8'}, (g['by_device'], g['by_operator']))
    check('按任务名/地点筛选', c.get('/api/uploads', query_string={'task': '测试', 'location': '测试地点'}).get_json()['datasets'] == 3)
    ex = c.get('/api/export').get_json(); items = [i for g in (ex if isinstance(ex, list) else [ex]) for i in g['items']]
    check('导出为官方客户端格式', all({'kind', 'source', 'uri', 'status'} <= set(i) for i in items) and items[0]['status'] in ('submitted', 'already-ready'))

    print('6a Excel 导出（按查询条件）')
    import io, zipfile
    xl = lambda q: c.get('/api/uploads/export', query_string=q)
    r_ = xl({'operator': 'lisi'}); z_ = zipfile.ZipFile(io.BytesIO(r_.data)); sh1 = z_.read('xl/worksheets/sheet1.xml').decode('utf-8'); wbx = z_.read('xl/workbook.xml').decode('utf-8')
    exp_n = c.get('/api/uploads', query_string={'operator': 'lisi'}).get_json()['total']
    check('Excel：按查询条件导出、4 个工作表、下载头', r_.status_code == 200 and 'spreadsheetml' in r_.mimetype and sh1.count('<row ') == exp_n + 1 and all(n in wbx for n in ('上传记录', '汇总', '按设备', '按人员')) and 'filename*=UTF-8' in r_.headers['Content-Disposition'], (sh1.count('<row '), exp_n))
    check('Excel：只含 lisi 的记录', 'lisi' in sh1 and 'zhangsan' not in sh1)
    all_n = c.get('/api/uploads').get_json()['total']; check('Excel：无条件 = 全部记录', zipfile.ZipFile(io.BytesIO(xl({}).data)).read('xl/worksheets/sheet1.xml').decode('utf-8').count('<row ') == all_n + 1)
    try:
        import openpyxl
        wb_ = openpyxl.load_workbook(io.BytesIO(xl({'cap_date': '2026-11-08'}).data)); ws_ = wb_['上传记录']; sm_ = {r[0].value: r[1].value for r in wb_['汇总'].iter_rows(min_row=2)}
        check('Excel（openpyxl 读取）：表头/汇总/按设备', ws_['A1'].value == '采集时间' and ws_.max_row > 1 and '2026-11-08' in sm_['查询条件'] and wb_['按设备']['A1'].value == '设备' and isinstance(ws_.cell(2, 16).value, (int, float)), sm_)
    except ImportError:
        print('  - 未安装 openpyxl，跳过读取校验')

    print('6b 校验补录（禁止重复）')
    dbx('DELETE FROM uploads'); dbx('DELETE FROM checks')                         # 远端数据还在，但「上传记录」被清空
    c.post('/api/run', json={'action': 'verify', 'ids': ids}); wait_job(c)
    bf = dbx("SELECT * FROM uploads WHERE method='校验补录'")
    check('快速校验给没有记录的已提交数据集补记录（不一致的那个不补）', len(bf) == 2 and {r_['status'] for r_ in bf} == {'已存在'} and all(r_['revision'] for r_ in bf), [dict(r_) for r_ in bf][:1])
    c.post('/api/run', json={'action': 'verify', 'ids': ids}); wait_job(c); c.post('/api/run', json={'action': 'verify_deep', 'ids': ids}); wait_job(c)
    check('重复校验不重复补录', dbx('SELECT COUNT(*) FROM uploads')[0][0] == 2)
    check('补录记录带数据列表字段', all(bf[0][k] for k in ('task', 'operator', 'location', 'device_sn', 'cap_date', 'room', 'start_time')))

    print('6c 重复扫描 / 改文件夹名 / 缓存 / 补零 / 打开文件夹')
    c.post('/api/roots', json={'remove': [data]})
    d2 = os.path.join(TMP, 'data2'); make_session(d2, 'umi_normal', '20261108', 'PRT_WX_room2', 'ISuit-V3_2026-11-08-12-00-00', 'zhangsan'); c.post('/api/roots', json={'add': [d2]})
    def rescan(**b):
        c.post('/api/scan', json=b); wait_scan(c); s_ = c.get('/api/state').get_json(); return s_, s_['scan']['result']
    s_, res = rescan(); d_ = s_['datasets'][0]
    check('房间 PRT_WX_room2 被标出（格式不规范）', d_['room'] == 'PRT_WX_room2' and bool(d_['room_bad']) and res['scanned'] == 1)
    s_, res = rescan(); check('重复扫描命中缓存', res['cached'] == 1 and res['scanned'] == 0, res)
    s_, res = rescan(full=True); check('完整扫描忽略缓存', res['cached'] == 0 and res['scanned'] == 1, res)
    os.rename(os.path.join(d2, 'umi_normal/1CA8/20261108/PRT_WX_room2'), os.path.join(d2, 'umi_normal/1CA8/20261108/PRT_WX_room002'))
    s_, res = rescan(); d_ = s_['datasets'][0]
    check('文件夹改名后重新扫描：OSS 路径随之更新', d_['room'] == 'PRT_WX_room002' and 'PRT_WX_room002' in d_['oss_url'] and not d_['room_bad'] and len(s_['datasets']) == 1, (d_['room'], d_['oss_url']))
    s_, res = rescan(city='wx', room='9'); d_ = s_['datasets'][0]
    check('界面填了城市/房间 = 显式覆盖，且有提示', d_['room'] == 'PRT_WX_room9' and bool(d_['room_diff']) and res['overridden'] == 1 and any('覆盖' in p_ for p_ in s_['problems']), (d_['room'], s_['problems']))
    s_, res = rescan(); check('清空后恢复跟随文件夹名', s_['datasets'][0]['room'] == 'PRT_WX_room002')
    os.rename(os.path.join(d2, 'umi_normal/1CA8/20261108/PRT_WX_room002'), os.path.join(d2, 'umi_normal/1CA8/20261108/PRT_WX_room2'))
    s_, res = rescan(pad=True); d_ = s_['datasets'][0]
    check('补零：OSS 用 room002，本地不动，并标出差异', d_['room'] == 'PRT_WX_room002' and d_['prt_local'] == 'PRT_WX_room2' and bool(d_['room_diff']) and not d_['room_bad'], d_)
    check('补零不改本地目录', os.path.isdir(os.path.join(d2, 'umi_normal/1CA8/20261108/PRT_WX_room2')))
    opened = []; api_mod.open_folder = lambda p_: opened.append(p_)
    r_ = c.post('/api/open', json={'id': d_['id']}); check('点击房间打开本地 PRT 文件夹', r_.status_code == 200 and opened == [os.path.join(d2, 'umi_normal/1CA8/20261108/PRT_WX_room2')], (r_.get_json(), opened))
    check('打开文件夹：未知 id → 404', c.post('/api/open', json={'id': 'nope'}).status_code == 404)
    check('打开文件夹：非本机访问 → 403', c.post('/api/open', json={'id': d_['id']}, environ_overrides={'REMOTE_ADDR': '10.0.0.8'}).status_code == 403)
    c.post('/api/roots', json={'remove': [d2]}); c.post('/api/roots', json={'add': [data]})

    print('6d 设备取文件夹名 / 会话目录列 / 错放标定数据移动')
    c.post('/api/roots', json={'remove': [data]})
    d3 = os.path.join(TMP, 'data3'); T0 = 1794100000
    A = make_session(d3, 'umi_normal', '20261111', 'PRT_WX_room002', 'ISuit-V3_2026-11-11-10-00-00', 'wang', slices=8, t0=T0)                 # 正常常规：240 秒
    B = make_session(d3, 'umi_noraml', '20261111', 'PRT_WX_room002', 'ISuit-V3_2026-11-11-11-00-00', 'wang', slices=2, t0=T0 + 1000, sn='01351CF3')   # 旧拼写目录 + 时长 60 秒 + SN 后 4 位=1CF3 ≠ 文件夹 1CA8
    C = make_session(d3, 'umi_normal', '20261111', 'PRT_WX_room002', 'ISuit-V3_2026-11-11-12-00-00', 'wang', slices=8, t0=T0 + 2000)         # 时长够，但任务名含「标定」
    open(os.path.join(C, 'datacollect_conf.yaml'), 'w', encoding='utf-8').write('device_SN: 01351CA8\nrecord_purpose: Normal\nrecord_operator: wang\ntask_name: 开工标定\n')
    Dd = make_session(d3, 'umi_normal', '20261111', 'PRT_WX_room002', 'ISuit-V3_2026-11-11-13-00-00', 'wang', slices=2, t0=T0 + 3000)          # 短，但目标已存在 → 不能移动
    os.makedirs(os.path.join(d3, 'umi_calibration/1CA8/20261111/PRT_WX_room002/ISuit-V3_2026-11-11-13-00-00'))
    E = make_session(d3, 'umi_normal', '20261111', 'PRT_WX_room002', 'ISuit-V3_2026-11-11-14-00-00', 'wang', slices=1, t0=T0)                       # 分片 yaml 读不出时间：时长未知，不按时长判定
    open(os.path.join(E, 'ISuit-V3_2026-11-11-14-00-00_0', 'ISuit-V3_2026-11-11-14-00-00_0.yaml'), 'w').write('foo: 1\n')
    c.post('/api/roots', json={'add': [d3]}); c.post('/api/scan', json={}); wait_scan(c)
    st = c.get('/api/state').get_json(); dm = {d['name'][-8:]: d for d in st['datasets']}
    check('设备 = 设备文件夹名（不是 SN 后 4 位）', dm['10-00-00']['device'] == '1CA8' and dm['11-00-00']['device'] == '1CA8' and dm['11-00-00']['device_sn'] == '01351CF3', [(d['name'], d['device']) for d in st['datasets']])
    check('文件夹名≠SN 后 4 位 → 提示（SN 一致的不提示）', bool(dm['11-00-00']['dev_diff']) and not dm['10-00-00']['dev_diff'] and not dm['12-00-00']['dev_diff'], dm['11-00-00']['dev_diff'])
    check('设备提示计入「有提示」', any('1CF3' in p_ for p_ in [dm['11-00-00']['dev_diff']]))
    check('正常常规数据不判错放', not dm['10-00-00']['misplaced'])
    check('时长 < 3 分钟 → 判定为标定', '时长' in dm['11-00-00']['misplaced'] and '3 分钟' in dm['11-00-00']['misplaced'], dm['11-00-00']['misplaced'])
    check('任务名含「标定」 → 判定为标定', '任务名含「标定」' in dm['12-00-00']['misplaced'] and '时长' not in dm['12-00-00']['misplaced'], dm['12-00-00']['misplaced'])
    check('分片读不出时间(时长未知)不按时长判定', not dm['14-00-00']['misplaced'], dm['14-00-00']['misplaced'])
    from pathlib import Path as P_
    from umi.scan import device_dir
    check('device_dir：取类型目录的下一级；没有设备层(下一级是日期)/会话直接在类型目录下 → 空；大小写原样保留',
          device_dir(P_('F:/data/umi_normal/1CF3/20261111/PRT_WX_room002/ISuit-V3_x')) == '1CF3' and device_dir(P_('F:/data/umi_normal/20261111/PRT_WX_room002/ISuit-V3_x')) == ''
          and device_dir(P_('/d/umi_normal/ISuit-V3_x')) == '' and device_dir(P_('/d/umi_noraml/1cf3/20261111/PRT/ISuit-V3_x')) == '1cf3')
    k_ = dm['10-00-00']['id']
    state.MOVING.acquire()
    try: codes = [c.post('/api/scan', json={}).status_code, c.post('/api/run', json={'action': 'upload', 'ids': [k_]}).status_code, c.post('/api/move', json={'ids': [k_]}).status_code]
    finally: state.MOVING.release()
    check('移动进行中：扫描/上传/再次移动都被拒绝(409)', codes == [409, 409, 409], codes)
    opened.clear(); r_ = c.post('/api/open', json={'id': dm['10-00-00']['id'], 'target': 'session'})
    check('点击「会话目录」打开会话目录本身', r_.status_code == 200 and opened == [A], (r_.get_json(), opened))
    pl = {p_['name'][-8:]: p_ for p_ in c.get('/api/move/plan').get_json()['plans']}
    check('移动预览：3 个候选（B、C、D），A/E 不在内', set(pl) == {'11-00-00', '12-00-00', '13-00-00'}, list(pl))
    check('目标路径 = 把 umi_normal/umi_noraml 换成 umi_calibration', pl['11-00-00']['target'] == os.path.join(d3, 'umi_calibration/1CA8/20261111/PRT_WX_room002/ISuit-V3_2026-11-11-11-00-00'), pl['11-00-00']['target'])
    check('目标已存在 → 标记不能移动', 'blocked' in pl['13-00-00'] and '目标已存在' in pl['13-00-00']['blocked'] and not pl['11-00-00']['blocked'])
    check('预览不动文件', all(os.path.isdir(x) for x in (A, B, C, Dd, E)))
    check('移动：非本机访问 → 403', c.post('/api/move', json={'ids': [dm['11-00-00']['id']]}, environ_overrides={'REMOTE_ADDR': '10.0.0.8'}).status_code == 403 and os.path.isdir(B))
    check('移动：未选择 → 400', c.post('/api/move', json={'ids': []}).status_code == 400)
    r_ = c.post('/api/move', json={'ids': [dm['11-00-00']['id'], dm['12-00-00']['id'], dm['13-00-00']['id'], dm['10-00-00']['id'], 'nope']}).get_json()
    check('移动 B、C 成功；D(目标已存在)/A(不是错放)/未知 id 失败', len(r_['moved']) == 2 and len(r_['failed']) == 3 and not r_['ok'], r_)
    TB, TC = os.path.join(d3, 'umi_calibration/1CA8/20261111/PRT_WX_room002/ISuit-V3_2026-11-11-11-00-00'), os.path.join(d3, 'umi_calibration/1CA8/20261111/PRT_WX_room002/ISuit-V3_2026-11-11-12-00-00')
    check('文件夹已整体移到 umi_calibration（原位置没有了，文件完整）', os.path.isdir(TB) and os.path.isdir(TC) and not os.path.exists(B) and not os.path.exists(C)
          and os.path.isfile(os.path.join(TB, 'datacollect_conf.yaml')) and len(os.listdir(TB)) == 3)
    check('没移动的原样保留', os.path.isdir(A) and os.path.isdir(Dd) and os.path.isdir(E))
    st = c.get('/api/state').get_json(); dm = {d['name'][-8:]: d for d in st['datasets']}
    check('移动后数据集更新：类型=标定、rel/OSS 路径换成 umi_calibration、不再错放', dm['11-00-00']['type'] == 'calibration' and dm['11-00-00']['rel'].startswith('umi_calibration/PRT_WX_room002/')
          and '/umi_calibration/' in dm['11-00-00']['oss_url'] and not dm['11-00-00']['misplaced'] and dm['11-00-00']['source'] == TB and dm['11-00-00']['device'] == '1CA8', dm['11-00-00'])
    check('移动结果已入库(datasets 表)', dbx("SELECT COUNT(*) FROM datasets WHERE rel LIKE 'umi_calibration/%' AND source=?", (TB,))[0][0] == 1 and len(st['datasets']) == 5)
    r_ = c.post('/api/move', json={'ids': [dm['10-00-00']['id']]}).get_json(); check('正常常规数据不可移动', len(r_['moved']) == 0 and not os.path.exists(TB + '_x'))
    c.post('/api/scan', json={'full': True}); wait_scan(c); st = c.get('/api/state').get_json()
    check('重新扫描后与移动结果一致（标定 2 个；常规里只剩被挡住的 D 仍是错放）', sum(d['type'] == 'calibration' for d in st['datasets']) == 2 and [d['name'][-8:] for d in st['datasets'] if d['type'] == 'normal' and d['misplaced']] == ['13-00-00'], [(d['name'][-8:], d['type'], d['misplaced']) for d in st['datasets']])
    Fs = make_session(d3, 'umi_normal', '20261111', 'PRT_WX_room002', 'ISuit-V3_2026-11-11-15-00-00', 'wang', slices=2, t0=T0 + 5000)         # 短会话；扫描后又有新内容（还在采集）
    c.post('/api/scan', json={}); wait_scan(c); fid = [d['id'] for d in c.get('/api/state').get_json()['datasets'] if d['name'].endswith('15-00-00')][0]
    open(os.path.join(Fs, 'late.txt'), 'w').write('x'); os.utime(Fs, (time.time() + 5, time.time() + 5))
    r_ = c.post('/api/move', json={'ids': [fid]}).get_json()
    check('扫描后内容有变化 → 拒绝按旧判定移动（原位置保留）', len(r_['moved']) == 0 and '有变化' in r_['failed'][0]['error'] and os.path.isdir(Fs), r_)
    c.post('/api/roots', json={'remove': [d3]}); c.post('/api/roots', json={'add': [data]})

    print('8 两种格式并存：ISuit-V3 → hw-upload、UMI_v3 → prt-huawei（同时加载，各自标定、各自上传、各自校验）')
    from umi.scan import txt, umi_frame_times, umi_log_times, umi_times
    from umi.util import fmt_ts, make_ds, tag_of
    from umi.nettune import estimate, recommend
    from umi.mover import plan_one
    from datetime import datetime as DT, timedelta
    check('格式只看目录名：两种命名各认各的，互不串', sdk.session_date('UMI_v3_20261108_103000') == '2026-11-08' and sdk.session_date('ISuit-V3_2026-11-08-10-30-00') == '2026-11-08' and sdk.session_date('foo') is None
          and sdk.session_date('UMI_v3_20261108_103000', 'isuit') is None and sdk.session_date('ISuit-V3_2026-11-08-10-30-00', 'umi_v3') is None and sdk.session_date('UMI_v3_20261108_1030') is None)
    check('fmt_of_name / bucket_for / tag_of', sdk.fmt_of_name('UMI_v3_20261108_103000') == 'umi_v3' and sdk.fmt_of_name('ISuit-V3_2026-11-08-10-30-00') == 'isuit' and sdk.fmt_of_name('x') is None
          and sdk.bucket_for('umi_v3') == 'prt-huawei' and sdk.bucket_for('isuit') == 'hw-upload' and tag_of('UMI_v3_20261108_103000') == '103000' and tag_of('ISuit-V3_2026-11-08-10-30-00') == '10-30-00')
    try: make_ds({'name': 'foo_bar', 'source': 'x', 'type': 'normal', 'rel': 'umi_normal/PRT_XZ_room001/foo_bar'}); bad_ = False
    except sdk.UploadError as e_: bad_ = '不是 ISuit-V3' in str(e_)
    check('目录名两种格式都不是 → make_ds 抛出带说明的错误', bad_)
    bj = lambda *a: DT(*a, tzinfo=state.TZ).timestamp()
    def rec_log(start, secs):                                        # 录制日志：格式与采集程序写的 metadata/recording.log 一致（时间带 +0800）
        t0 = DT.strptime(start, '%Y-%m-%d %H:%M:%S'); f_ = lambda d: d.strftime('%Y-%m-%d %H:%M:%S.') + '%03d +0800' % (d.microsecond // 1000)
        return '\n'.join(['%s INFO 录制日志创建：dataset=x' % f_(t0), '%s INFO 录制开始（操作计时起点，不含按键倒计时及启动准备）' % f_(t0 + timedelta(milliseconds=205)),
                          '%s INFO 停止录制：原因=用户停止请求' % f_(t0 + timedelta(seconds=secs, milliseconds=205)), '%s INFO 保存结束：结果=成功 操作录制时长秒=%s 停止后保存耗时秒=0.139016' % (f_(t0 + timedelta(seconds=secs + 1)), secs)]) + '\n'
    check('txt：列表用「、」连接，None→空串，字符串原样', txt([]) == '' and txt(['a', 'b']) == 'a、b' and txt(None) == '' and txt('x') == 'x' and txt(['a', None]) == 'a')
    check('多出口并发建议：1 个出口 = 默认值；出口越多数据集并发越高（≤8，与顶栏下拉一致）；估算连接数够用', recommend(1)['ds_jobs'] == 2 and recommend(1)['jobs'] == 16 and recommend(1)['parallel'] == 4
          and [recommend(w)['ds_jobs'] for w in (1, 2, 3, 4, 8, 16)] == [2, 4, 6, 8, 8, 8] and all(recommend(w)['enough'] for w in range(1, 9)) and recommend(0)['ds_jobs'] == 2 and recommend(99)['ds_jobs'] == 8)
    check('连接数估算 = 数据集并发 × 3 × 分片并发；每个出口 < 24 条判「偏少」', estimate(2, 4, 1) == {'conns': 24, 'per_wan': 24.0, 'enough': True} and estimate(2, 4, 3)['enough'] is False and estimate(6, 6, 3)['conns'] == 108)

    # ---- 两个「磁盘」同时加载：一个放 ISuit-V3，一个放 UMI_v3（本地层级相同：umi_normal\{设备}\{日期}\PRT_城市_room房间\会话）
    dataI, data4 = os.path.join(TMP, 'data_isuit'), os.path.join(TMP, 'data4')
    I1 = make_session(dataI, 'umi_normal', '20261108', 'PRT_XZ_room001', 'ISuit-V3_2026-11-08-10-30-00', 'zhangsan', slices=8)     # 8×30 秒 = 4 分钟：常规
    I2 = make_session(dataI, 'umi_normal', '20261108', 'PRT_XZ_room001', 'ISuit-V3_2026-11-08-11-00-00', 'lisi', slices=1)         # 30 秒 < 3 分钟：被自动判定为标定（错放在 umi_normal）
    mk = lambda *a, **k: make_umi_session(data4, *a, **k)
    U1 = mk('umi_normal', '20261108', 'PRT_XZ_room001', 'UMI_v3_20261108_103000', 'zhangsan', root_file=True, log=rec_log('2026-11-08 10:30:00', 600.5))       # 录制日志 600.5 秒（conf 里的计划时长 360 不能被用）
    U2 = mk('umi_calibration', '20261108', 'PRT_XZ_room001', 'UMI_v3_20261108_095000', 'zhangsan', log=rec_log('2026-11-08 09:50:00', 120), videos=('left hand/cam-l.mp4', 'head/cam-h.mp4'),
            conf='device_SN: 01351CA8\nrecord_operator: zhangsan\ntask_name: 项目X\ntask_title: 开工标定\n')
    U3 = mk('umi_normal', '20261108', 'PRT_XZ_room001', 'UMI_v3_20261109_080000', 'lisi', frames=(bj(2026, 11, 9, 8, 0, 2), bj(2026, 11, 9, 8, 5, 2)),
            conf='device_SN: 01351B7C\nrecord_operator: lisi\ntask_name: 测试任务\nrecord_location: Default\n')   # 没有录制日志、用 frames.csv 曝光时间（300 秒）；目录日期 ≠ session 日期；采集位置 Default
    U4 = mk('umi_calibration', '20261108', 'PRT_XZ_room001', 'UMI_v3_20261108_090000', 'zhangsan', conf='device_SN: 01351CA8\n', dev='ZZZZ')                  # 什么时间信息都没有
    U5 = mk('umi_normal', '20261108', 'PRT_XZ_room001', 'UMI_v3_20261108_140000', 'wang', log=rec_log('2026-11-08 14:00:00', 600.0),
            conf='device_SN: 01351CA8\nrecord_operator: wang\ntask_name: 项目X\ntask_title: 收工标定\n')   # 任务名含「标定」，但 UMI_v3 不自动判定
    mk('umi_normal', '20261108', 'PRT_XZ_room001', 'UMI_v3_20261108_111111', skip=('metadata/conf.yaml',))                         # 缺 metadata/conf.yaml
    mk('umi_normal', '20261108', 'PRT_XZ_room001', 'UMI_v3_20261108_121212', skip=('left hand',))                                  # 缺 left hand
    for r_ in c.get('/api/state').get_json()['roots']: c.post('/api/roots', json={'remove': [r_]})
    c.post('/api/roots', json={'add': [dataI]}); c.post('/api/roots', json={'add': [data4]})
    check('umi_times：recording.log 优先（开始/停止/操作录制时长，含时区；conf 里的计划时长不用）', umi_times(U1, 'UMI_v3_20261108_103000') == {'duration': 600.5, 'start': '2026-11-08 10:30:00', 'end': '2026-11-08 10:40:00'}, umi_times(U1, 'UMI_v3_20261108_103000'))
    check('umi_times：没有日志 → 各相机 frames.csv 的 exposure_ns 最早到最晚', umi_times(U3, 'UMI_v3_20261109_080000') == {'duration': 300.0, 'start': '2026-11-09 08:00:02', 'end': '2026-11-09 08:05:02'}, umi_times(U3, 'UMI_v3_20261109_080000'))
    check('umi_times：什么都没有 → 开始取目录名、时长 0、不报错', umi_times(U4, 'UMI_v3_20261108_090000') == {'duration': 0.0, 'start': '2026-11-08 09:00:00', 'end': ''}, umi_times(U4, 'UMI_v3_20261108_090000'))
    bad_ = os.path.join(TMP, 'badlog'); os.makedirs(os.path.join(bad_, 'metadata')); os.makedirs(os.path.join(bad_, 'head'))
    open(os.path.join(bad_, 'metadata', 'recording.log'), 'w', encoding='utf-8').write('乱码\n2026-13-99 99:99:99.000 +0800 INFO 录制开始\n操作录制时长秒=abc\n'); open(os.path.join(bad_, 'head', 'x.frames.csv'), 'w').write('frame_index,sync_id,exposure_ns\n')
    check('umi_log_times / umi_frame_times：乱码日志、只有表头的 csv → 读不到但不抛错', umi_log_times(bad_) == (None, None, None) and umi_frame_times(bad_) == (None, None))

    # ---- 配置：两个桶
    cg = c.get('/api/config').get_json()
    check('配置：两种格式各有各的桶（默认 hw-upload / prt-huawei）', cg['bucket'] == 'hw-upload' and cg['bucket_umi'] == 'prt-huawei' and cg['buckets'] == {'isuit': 'hw-upload', 'umi_v3': 'prt-huawei'} and cg['formats'] == {'isuit': 'ISuit-V3', 'umi_v3': 'UMI_v3'}, cg)
    check('桶名校验：两个桶都要求小写字母/数字/-/.（大写、下划线、过短 → 400）', [c.post('/api/config', json={k_: b_}).status_code for k_ in ('bucket', 'bucket_umi') for b_ in ('PRT-HUAWEI', 'prt_huawei', 'a')] == [400] * 6)

    # ---- 一次扫描同时识别两种格式
    c.post('/api/scan', json={}); wait_scan(c); st = c.get('/api/state').get_json(); rows_ = {d['name']: d for d in st['datasets']}
    UMI_N = ['UMI_v3_20261108_103000', 'UMI_v3_20261108_095000', 'UMI_v3_20261109_080000', 'UMI_v3_20261108_090000', 'UMI_v3_20261108_140000']; ISU_N = ['ISuit-V3_2026-11-08-10-30-00', 'ISuit-V3_2026-11-08-11-00-00']
    check('两个磁盘一次扫描：2 个 ISuit-V3 + 5 个完整 UMI_v3 会话同时入列，残缺的不入列', set(rows_) == set(UMI_N + ISU_N), sorted(rows_))
    check('ISuit-V3 的设备规则不变：设备文件夹与 SN 后 4 位比对（1CA8 ←→ 01351CA8 一致）', rows_['ISuit-V3_2026-11-08-10-30-00']['device'] == '1CA8' and not rows_['ISuit-V3_2026-11-08-10-30-00']['dev_diff'])
    from umi.scan import row_info
    bad_isuit = dict(rows_['ISuit-V3_2026-11-08-10-30-00'], device='ZZZZ')
    check('ISuit-V3 设备文件夹写错（ZZZZ ←→ SN 后 4 位 1CA8）仍然提示；同样的情形对 UMI_v3 不提示', bool(row_info(bad_isuit)['dev_diff']) and not row_info(dict(rows_['UMI_v3_20261108_103000'], device='ZZZZ'))['dev_diff'])
    check('每行带格式与桶：ISuit-V3 → hw-upload，UMI_v3 → prt-huawei；OSS 路径用各自的桶', all(rows_[n]['fmt'] == 'isuit' and rows_[n]['bucket'] == 'hw-upload' and rows_[n]['oss_url'].startswith('oss://hw-upload/raw_data/') for n in ISU_N)
          and all(rows_[n]['fmt'] == 'umi_v3' and rows_[n]['bucket'] == 'prt-huawei' and rows_[n]['oss_url'].startswith('oss://prt-huawei/raw_data/') for n in UMI_N), [(n, d['fmt'], d['bucket']) for n, d in rows_.items()])
    check('/api/state 带两种格式的桶与名称', st['buckets'] == {'isuit': 'hw-upload', 'umi_v3': 'prt-huawei'} and st['formats'] == {'isuit': 'ISuit-V3', 'umi_v3': 'UMI_v3'})
    pr = '\n'.join(st['problems'])
    check('扫描问题：残缺的 UMI_v3（缺 metadata/conf.yaml、缺采集目录 left hand）有说明；没有「找不到数据集」', '缺少 metadata/conf.yaml' in pr and '缺少采集目录 left hand' in pr and '没有找到数据集' not in pr, st['problems'])
    a1 = rows_['UMI_v3_20261108_103000']
    check('UMI_v3 路径层级与 ISuit-V3 一样：设备文件夹、日期文件夹、PRT 房间；OSS 路径日期取自目录名', a1['oss_url'] == 'oss://prt-huawei/raw_data/raw_data_261108/umi_normal/PRT_XZ_room001/UMI_v3_20261108_103000/' and a1['rel'] == 'umi_normal/PRT_XZ_room001/UMI_v3_20261108_103000'
          and a1['device'] == '1CA8' and a1['dir_date'] == '20261108' and a1['room'] == 'PRT_XZ_room001' and not a1['dev_diff'] and not a1['date_warn'], a1)
    check('UMI_v3 采集信息：SN / 任务(项目名 / 任务名) / 子任务列表 / 操作员 / 地点；时长取自录制日志 600.5 秒；没有分片', a1['device_sn'] == '01351CA8' and a1['task'] == '测试项目 / 测试任务' and a1['sub_task'] == '子任务A、子任务B' and a1['operator'] == 'zhangsan' and a1['location'] == '测试地点'
          and a1['duration'] == 600.5 and a1['start'] == '2026-11-08 10:30:00' and a1['end'] == '2026-11-08 10:40:00' and a1['slices'] == 0, a1)
    a3 = rows_['UMI_v3_20261109_080000']
    check('UMI_v3 无日志：时长取 frames.csv（300 秒）；目录日期 ≠ 采集日期提示照常', a3['duration'] == 300.0 and a3['start'] == '2026-11-09 08:00:02' and bool(a3['date_warn']) and not a1['date_warn'], a3)
    check('UMI_v3 的设备只是设备文件夹名、仅作参考：与设备SN 毫无关系（文件夹 ZZZZ ←→ SN 01351CA8）也不报不一致', rows_['UMI_v3_20261108_090000']['device'] == 'ZZZZ' and rows_['UMI_v3_20261108_090000']['device_sn'] == '01351CA8' and not rows_['UMI_v3_20261108_090000']['dev_diff'], rows_['UMI_v3_20261108_090000'])
    check('UMI_v3 什么时间信息都没有：开始取目录名、时长 0', rows_['UMI_v3_20261108_090000']['duration'] == 0 and rows_['UMI_v3_20261108_090000']['start'] == '2026-11-08 09:00:00')
    check('各格式的标定判定互相独立：UMI_v3 不自动判错放（含「收工标定」的也不判）；ISuit-V3 照旧自动判定（30 秒的常规会话被判为标定）',
          all(not rows_[n]['misplaced'] for n in UMI_N) and '时长' in rows_['ISuit-V3_2026-11-08-11-00-00']['misplaced'] and not rows_['ISuit-V3_2026-11-08-10-30-00']['misplaced'], [(n, rows_[n]['misplaced']) for n in rows_ if rows_[n]['misplaced']])
    check('自动「移到标定」预览只包含 ISuit-V3 的错放，不含任何 UMI_v3 会话', [p_['name'] for p_ in c.get('/api/move/plan').get_json()['plans']] == ['ISuit-V3_2026-11-08-11-00-00'])

    # ---- ISuit-V3：自动移到标定
    r_ = c.post('/api/move', json={'ids': [rows_['ISuit-V3_2026-11-08-11-00-00']['id']]}).get_json(); st = c.get('/api/state').get_json(); rows_ = {d['name']: d for d in st['datasets']}; mI = rows_['ISuit-V3_2026-11-08-11-00-00']
    check('ISuit-V3 自动移到标定：类型=标定、rel 与 OSS 路径换成 umi_calibration（仍是 hw-upload 桶）', r_['ok'] and mI['type'] == 'calibration' and mI['rel'] == 'umi_calibration/PRT_XZ_room001/ISuit-V3_2026-11-08-11-00-00' and mI['oss_url'].startswith('oss://hw-upload/') and '/umi_calibration/' in mI['oss_url'] and not mI['misplaced'], (r_, mI['rel']))

    # ---- UMI_v3：手动移动（看视频后自己决定，两个方向）
    idU5, idU1 = rows_['UMI_v3_20261108_140000']['id'], rows_['UMI_v3_20261108_103000']['id']
    pv = c.post('/api/move/preview', json={'ids': [idU5], 'to': 'calibration'}).get_json()['plans']; tgt5 = os.path.join(data4, 'umi_calibration', '1CA8', '20261108', 'PRT_XZ_room001', 'UMI_v3_20261108_140000')
    check('UMI_v3 手动预览：目标路径把 umi_normal 换成 umi_calibration（设备/日期层级保留），可移动', len(pv) == 1 and pv[0]['target'] == tgt5 and pv[0]['to'] == 'calibration' and not pv[0]['blocked'] and pv[0]['reason'] == '手动选择', pv)
    check('手动预览：方向与现状相同 → 不能移动', c.post('/api/move/preview', json={'ids': [idU5], 'to': 'normal'}).get_json()['plans'][0]['blocked'] == '已经是常规数据')
    check('手动预览/移动：to 不合法 → 400；不带 to 的移动对 UMI_v3 拒绝（不是可移动的数据）', c.post('/api/move/preview', json={'ids': [idU5], 'to': 'x'}).status_code == 400 and c.post('/api/move', json={'ids': [idU5], 'to': 'x'}).status_code == 400
          and c.post('/api/move', json={'ids': [idU1]}).get_json()['failed'][0]['error'].startswith('不是可移动'))
    check('手动移动只用于 UMI_v3：ISuit-V3 会话被拒绝', plan_one({'id': 'x', 'name': 'ISuit-V3_2026-11-08-10-30-00', 'source': '/d/umi_normal/PRT_XZ_room001/ISuit-V3_2026-11-08-10-30-00', 'type': 'normal', 'rel': 'umi_normal/PRT_XZ_room001/ISuit-V3_2026-11-08-10-30-00'}, rels=set(), to='calibration')['blocked'] == '手动移动只用于 UMI_v3 数据')
    os.makedirs(tgt5); check('目标已存在 → 不能移动（不覆盖、不合并）', '目标已存在' in c.post('/api/move/preview', json={'ids': [idU5], 'to': 'calibration'}).get_json()['plans'][0]['blocked']); os.rmdir(tgt5)
    r_ = c.post('/api/move', json={'ids': [idU5], 'to': 'calibration'}).get_json(); st = c.get('/api/state').get_json(); mv = {d['name']: d for d in st['datasets']}['UMI_v3_20261108_140000']
    check('UMI_v3 手动移到标定：类型=标定、OSS 路径换成 umi_calibration（仍是 prt-huawei 桶）、时长保留、原位置不存在', r_['ok'] and mv['type'] == 'calibration' and mv['rel'] == 'umi_calibration/PRT_XZ_room001/UMI_v3_20261108_140000' and mv['oss_url'].startswith('oss://prt-huawei/') and '/umi_calibration/' in mv['oss_url']
          and not mv['misplaced'] and mv['duration'] == 600.0 and not os.path.exists(U5) and os.path.isfile(os.path.join(mv['source'], 'metadata', 'conf.yaml')), (r_, mv['rel']))
    r_ = c.post('/api/move', json={'ids': [mv['id']], 'to': 'normal'}).get_json(); st = c.get('/api/state').get_json(); mb = {d['name']: d for d in st['datasets']}['UMI_v3_20261108_140000']
    check('UMI_v3 手动移回常规：路径与 id 都回到原样', r_['ok'] and mb['type'] == 'normal' and os.path.isdir(U5) and not os.path.exists(tgt5) and mb['id'] == idU5, (r_, mb['rel']))

    # ---- 视频：列表 / Range 流式 / 越权路径 / 符号链接 / 仅本机
    idU2 = rows_['UMI_v3_20261108_095000']['id']; vj = c.get('/api/videos', query_string={'id': idU2}).get_json()
    check('/api/videos：列出会话里的视频（head 在 left hand 前，不含 metadata），带大小', [f_['f'] for f_ in vj['files']] == ['head/cam-h.mp4', 'left hand/cam-l.mp4'] and all(f_['size'] == 20480 for f_ in vj['files']) and vj['type'] == 'calibration', vj)
    vr = c.get('/api/video', query_string={'id': idU2, 'f': 'head/cam-h.mp4'})
    check('/api/video：200，video/mp4，声明支持 Range', vr.status_code == 200 and vr.mimetype == 'video/mp4' and len(vr.data) == 20480 and vr.headers.get('Accept-Ranges') == 'bytes', (vr.status_code, vr.mimetype))
    vr = c.get('/api/video', query_string={'id': idU2, 'f': 'head/cam-h.mp4'}, headers={'Range': 'bytes=100-199'})
    check('/api/video：Range 请求返回 206 与对应字节（浏览器拖动进度）', vr.status_code == 206 and vr.headers.get('Content-Range') == 'bytes 100-199/20480' and vr.data == (bytes(range(256)) * 80)[100:200], (vr.status_code, vr.headers.get('Content-Range')))
    esc_ = os.path.join(data4, 'outside.mp4'); open(esc_, 'wb').write(b'secret'); link_ok = True
    try: os.symlink(esc_, os.path.join(rows_['UMI_v3_20261108_095000']['source'], 'head', 'link.mp4'))
    except (OSError, NotImplementedError, AttributeError): link_ok = False
    bad_f = ['../UMI_v3_20261108_103000/head/a.bin', '../../outside.mp4', 'head/../../outside.mp4', 'metadata/conf.yaml', 'head/a.bin', esc_, '/etc/passwd', 'head/nope.mp4', '']
    if link_ok: bad_f.append('head/link.mp4')
    codes = [c.get('/api/video', query_string={'id': idU2, 'f': f_}).status_code for f_ in bad_f]
    check('/api/video：越权路径（..、绝对路径、非视频扩展名、指向会话外的符号链接、不存在）全部 404', codes == [404] * len(bad_f), list(zip(bad_f, codes)))
    check('/api/video、/api/videos：未知 id → 404；不是本机访问 → 403', c.get('/api/video', query_string={'id': 'nope', 'f': 'head/cam-h.mp4'}).status_code == 404 and c.get('/api/videos', query_string={'id': 'nope'}).status_code == 404
          and c.get('/api/video', query_string={'id': idU2, 'f': 'head/cam-h.mp4'}, environ_overrides={'REMOTE_ADDR': '10.0.0.5'}).status_code == 403 and c.get('/api/videos', query_string={'id': idU2}, environ_overrides={'REMOTE_ADDR': '10.0.0.5'}).status_code == 403)
    os.remove(os.path.join(rows_['UMI_v3_20261108_095000']['source'], 'head', 'link.mp4')) if link_ok else None
    check('前端：video.js 可访问', c.get('/static/js/video.js').status_code == 200 and 'openVideo' in c.get('/static/js/video.js').get_data(as_text=True))

    # ---- 混合上传（SDK）：一次任务里同时选两种格式，各传各的桶
    st = c.get('/api/state').get_json(); rows_ = {d['name']: d for d in st['datasets']}
    mix = ['ISuit-V3_2026-11-08-10-30-00', 'ISuit-V3_2026-11-08-11-00-00', 'UMI_v3_20261108_103000', 'UMI_v3_20261108_095000', 'UMI_v3_20261109_080000', 'UMI_v3_20261108_090000']
    FakeOss.seen.clear(); j = upload_all(c, [rows_[n]['id'] for n in mix], method='sdk', order='folder', ds_jobs='2')
    check('SDK 混合上传：2 个 ISuit-V3 + 4 个 UMI_v3 一次任务全部成功', j['prog']['ok'] == 6 and j['prog']['fail'] == 0, [l for l in j['log'] if '✘' in l])
    check('每个桶一个客户端：用到了 hw-upload 和 prt-huawei 两个桶的客户端', FakeOss.seen == {'hw-upload', 'prt-huawei'}, FakeOss.seen)
    walk_ = lambda root: sorted(os.path.relpath(os.path.join(r_, f_), root).replace(os.sep, '/') for r_, ds_, fs in os.walk(root) for f_ in fs if not os.path.relpath(os.path.join(r_, f_), root).startswith('@'))
    isu_files, umi_files = walk_(R), walk_(RB('prt-huawei'))
    check('桶隔离：ISuit-V3 只在 hw-upload（路径含 ISuit-V3_…），UMI_v3 只在 prt-huawei（路径含 UMI_v3_…），互不串', isu_files and umi_files and all('ISuit-V3_' in f_ for f_ in isu_files) and all('UMI_v3_' in f_ for f_ in umi_files)
          and not any('UMI_v3_' in f_ for f_ in isu_files) and not any('ISuit-V3_' in f_ for f_ in umi_files), (isu_files[:2], umi_files[:2]))
    base1, bI = RB('prt-huawei') + 'raw_data/raw_data_261108/umi_normal/PRT_XZ_room001/UMI_v3_20261108_103000/', R + 'raw_data/raw_data_261108/umi_normal/PRT_XZ_room001/ISuit-V3_2026-11-08-10-30-00/'
    rf = sorted(os.path.relpath(os.path.join(r_, f_), base1).replace(os.sep, '/') for r_, _, fs in os.walk(base1) for f_ in fs)
    check('UMI_v3 远端路径：含带空格的 left hand/ right hand/、recording.log、commit 标记齐全', rf == sorted(['head/a.bin', 'head/b.bin', 'left hand/l.bin', 'right hand/r.bin', 'metadata/conf.yaml', 'metadata/recording.log', 'session.json', 'manifest.json', '_READY']), rf)
    mf = json.loads(open(base1 + 'manifest.json', encoding='utf-8').read())
    check('UMI_v3 manifest：dataset_id=会话名，capture_date=YYYY-MM-DD，dataset_type=normal，清单按 UTF-16 排序', mf['dataset_id'] == 'UMI_v3_20261108_103000' and mf['capture_date'] == '2026-11-08' and mf['dataset_type'] == 'normal'
          and [f_['path'] for f_ in mf['files']] == sorted(f_['path'] for f_ in mf['files']) and 'left hand/l.bin' in [f_['path'] for f_ in mf['files']], [f_['path'] for f_ in mf['files']])
    check('ISuit-V3 照旧：manifest.json 与 _READY 在 hw-upload 桶里', os.path.isfile(bI + 'manifest.json') and os.path.isfile(bI + '_READY') and json.loads(open(bI + 'manifest.json', encoding='utf-8').read())['dataset_id'] == 'ISuit-V3_2026-11-08-10-30-00')
    W = [l.split()[1] for l in open(PROC_LOG) if l.startswith('W')]
    check('提交顺序：每个会话的 manifest.json 都在它的 _READY 之前（两个桶都一样）', len(W) == 12 and all(W.index(k_.replace('_READY', 'manifest.json')) < W.index(k_) for k_ in W if k_.endswith('_READY')), W)
    check('日志标明每个数据集的格式与桶', any('[ISuit-V3 → 桶 hw-upload]' in l for l in j['log']) and any('[UMI_v3 → 桶 prt-huawei]' in l for l in j['log']), [l for l in j['log'] if '▶' in l][:3])
    check('采集位置为 Default 的会话上传时给出提示（两种格式通用），其余不提示', sum('采集位置为 Default' in l for l in j['log']) == 1, [l for l in j['log'] if 'Default' in l])
    check('日志里的并发标签：ISuit-V3 取 HH-MM-SS，UMI_v3 取 HHMMSS', any('[10-30-00]' in l or '[11-00-00]' in l for l in j['log']) and any('[103000]' in l or '[095000]' in l or '[080000]' in l for l in j['log']))
    recs_ = [dict(r_) for r_ in dbx('SELECT * FROM uploads ORDER BY id DESC LIMIT 6')]
    check('上传记录里的 oss_url 是当时真正用的桶', all(r_['oss_url'].startswith('oss://prt-huawei/' if 'UMI_v3_' in r_['rel'] else 'oss://hw-upload/') for r_ in recs_) and {r_['oss_url'].split('/')[2] for r_ in recs_} == {'hw-upload', 'prt-huawei'}, [r_['oss_url'] for r_ in recs_])
    uu = c.get('/api/uploads', query_string={'fmt': 'umi_v3'}).get_json(); ui = c.get('/api/uploads', query_string={'fmt': 'isuit'}).get_json()
    check('上传记录按格式筛选：UMI_v3 只有这 4 条（带格式与桶）；ISuit-V3 的不含 UMI_v3', uu['total'] == 4 and all(r_['fmt'] == 'umi_v3' and r_['bucket'] == 'prt-huawei' for r_ in uu['rows']) and ui['total'] >= 2 and all(r_['fmt'] == 'isuit' and r_['bucket'] == 'hw-upload' for r_ in ui['rows']), (uu['total'], ui['total']))
    ur = c.get('/api/uploads', query_string={'cap_date': '2026-11-09'}).get_json()
    check('上传记录按采集日期筛选（UMI_v3 的 2026-11-09）', ur['datasets'] == 1 and ur['rows'][0]['rel'].endswith('UMI_v3_20261109_080000'), ur['datasets'])
    ex = c.get('/api/export'); exj = ex.get_json(); its = [i_ for g_ in (exj if isinstance(exj, list) else [exj]) for i_ in g_['items']]
    check('导出官方格式：同时含两个桶的路径', ex.status_code == 200 and any((i_['uri'] or '').startswith('oss://prt-huawei/') for i_ in its) and any((i_['uri'] or '').startswith('oss://hw-upload/') for i_ in its), ex.status_code)
    check('导出 Excel 含两种格式（带「格式」「桶」列）', c.get('/api/uploads/export').status_code == 200 and c.get('/api/uploads/export', query_string={'fmt': 'umi_v3'}).status_code == 200)

    # ---- 校验：各用各的桶
    ids = [rows_[n]['id'] for n in mix]
    c.post('/api/run', json={'action': 'verify', 'ids': ids}); wait_job(c); c.post('/api/run', json={'action': 'verify_deep', 'ids': ids}); wait_job(c)
    sv = {d['name']: d for d in c.get('/api/state').get_json()['datasets']}
    check('快速/深度校验：两种格式各自去自己的桶核对，全部「一致」', all(sv[n]['remote']['status'] == '一致' and sv[n]['remote']['deep'] and sv[n]['up_state'] == '已上传' for n in mix), {n: (sv[n]['remote'] or {}).get('status') for n in mix})
    shutil.rmtree(RB('prt-huawei'), True)                                          # 清掉 prt-huawei 桶里的数据 → UMI_v3 应「远端缺失」，ISuit-V3 仍然一致（证明校验没有串桶）
    c.post('/api/run', json={'action': 'verify', 'ids': ids}); wait_job(c); sv = {d['name']: d for d in c.get('/api/state').get_json()['datasets']}
    check('校验不串桶：prt-huawei 里的数据没了，只有 UMI_v3 变「未上传/缺失」，ISuit-V3 仍一致', all(sv[n]['remote']['status'] == '一致' for n in mix if n.startswith('ISuit')) and all(sv[n]['remote']['status'] != '一致' for n in mix if n.startswith('UMI')), {n: sv[n]['remote']['status'] for n in mix})

    # ---- ossutil 混合上传 + 校验：每个数据集用自己的桶（ossutil 进程的目标 URL 带各自的桶）
    for unit in ('slice', 'session'):
        sdk.Oss = BoomOss
        j = upload_all(c, [rows_['ISuit-V3_2026-11-08-10-30-00']['id'], rows_['UMI_v3_20261108_103000']['id']], method='ossutil', ossutil_unit=unit, ds_jobs='2', ossutil_path=ossutil)
        isu_f, umi_f = walk_(R), walk_(RB('prt-huawei'))
        check('ossutil %s：ISuit-V3 与 UMI_v3 混合上传成功，不碰 SDK，各在自己的桶（含带空格目录）' % unit, j['prog']['ok'] == 2 and j['prog']['fail'] == 0 and any('/left hand/l.bin' in f_ for f_ in umi_f) and any(f_.endswith('/_READY') for f_ in umi_f) and any(f_.endswith('/_READY') for f_ in isu_f)
              and not any('UMI_v3_' in f_ for f_ in isu_f) and not any('ISuit-V3_' in f_ for f_ in umi_f), (j['prog'], [l for l in j['log'] if '✘' in l]))
    c.post('/api/run', json={'action': 'verify', 'ids': [rows_['ISuit-V3_2026-11-08-10-30-00']['id'], rows_['UMI_v3_20261108_103000']['id']]}); wait_job(c); sv = {d['name']: d for d in c.get('/api/state').get_json()['datasets']}
    check('ossutil 方式的校验同样各用各的桶，全部一致', sv['ISuit-V3_2026-11-08-10-30-00']['remote']['status'] == '一致' and sv['UMI_v3_20261108_103000']['remote']['status'] == '一致')
    sdk.Oss = FakeOss

    # ---- 自定义桶名：两个桶可以分别改，路径与上传目标跟着变；任务进行中不能换桶
    state.SCAN['running'] = True; code_ = c.post('/api/config', json={'bucket_umi': 'umi-bkt'}).status_code; state.SCAN['running'] = False
    check('扫描/任务进行中不能换桶(409)，桶不变', code_ == 409 and c.get('/api/config').get_json()['bucket_umi'] == 'prt-huawei', code_)
    r_ = c.post('/api/config', json={'bucket': 'isuit-bkt', 'bucket_umi': 'umi-bkt'}).get_json(); st = c.get('/api/state').get_json(); rows_ = {d['name']: d for d in st['datasets']}
    check('分别改两个桶：state 与每行 OSS 路径都用新桶', r_['buckets'] == {'isuit': 'isuit-bkt', 'umi_v3': 'umi-bkt'} and st['buckets'] == {'isuit': 'isuit-bkt', 'umi_v3': 'umi-bkt'}
          and rows_['ISuit-V3_2026-11-08-10-30-00']['oss_url'].startswith('oss://isuit-bkt/') and rows_['UMI_v3_20261108_103000']['oss_url'].startswith('oss://umi-bkt/') and rows_['UMI_v3_20261108_103000']['bucket'] == 'umi-bkt', r_)
    j = upload_all(c, [rows_['ISuit-V3_2026-11-08-10-30-00']['id'], rows_['UMI_v3_20261108_103000']['id']], method='sdk', order='folder', ds_jobs='2')
    check('自定义桶：上传到各自新桶（isuit-bkt / umi-bkt）', j['prog']['ok'] == 2 and os.path.isfile(RB('isuit-bkt') + 'raw_data/raw_data_261108/umi_normal/PRT_XZ_room001/ISuit-V3_2026-11-08-10-30-00/_READY')
          and os.path.isfile(RB('umi-bkt') + 'raw_data/raw_data_261108/umi_normal/PRT_XZ_room001/UMI_v3_20261108_103000/_READY'), j['log'][-3:])
    check('桶留空 = 各自恢复默认（hw-upload / prt-huawei）', c.post('/api/config', json={'bucket': '', 'bucket_umi': ''}).status_code == 200 and c.get('/api/config').get_json()['buckets'] == {'isuit': 'hw-upload', 'umi_v3': 'prt-huawei'})

    # ---- 多出口负载均衡：并发建议接口与配置
    np_ = c.get('/api/net/plan', query_string={'wan': 3, 'ds_jobs': 2, 'parallel': 4}).get_json()
    check('/api/net/plan：按出口数给建议，并估算当前设置的连接数（默认设置对 3 个出口「偏少」）', np_['recommend']['ds_jobs'] == 6 and np_['recommend']['parallel'] == 6 and np_['recommend']['enough'] and np_['current']['conns'] == 24 and np_['current']['enough'] is False, np_)
    check('出口数配置：1~16，可保存；非法 → 400；接口参数非法 → 400', c.post('/api/config', json={'wan': '3'}).status_code == 200 and c.get('/api/config').get_json()['wan'] == '3' and c.post('/api/config', json={'wan': '0'}).status_code == 400
          and c.post('/api/config', json={'wan': '17'}).status_code == 400 and c.get('/api/net/plan', query_string={'wan': 'x'}).status_code == 400)
    c.post('/api/config', json={'wan': ''})
    sdk.Oss = BoomOss
    for jb, pl in (('32', '8'), ('16', '4')):                                      # 页面配置的 jobs / parallel 必须原样传给 ossutil（--jobs=同时文件数 --parallel=每个大文件的分片并发）
        j = upload_all(c, [rows_['UMI_v3_20261109_080000']['id']], method='ossutil', ossutil_unit='slice', ds_jobs='4', jobs=jb, parallel=pl, ossutil_path=ossutil, ossutil_args='--jobs=1 --parallel=1')
        A_ = [l.split(' ', 2)[2] for l in open(PROC_LOG, encoding='utf-8', errors='replace').read().split('\n') if l.startswith('A ')]
        A_ = [x for x in A_ if x]                                           # 提交 manifest.json / _READY 的单文件 cp 不带并发参数（记成空行），只看文件夹上传的 cp
        check('并发参数落到 ossutil 命令：--jobs=%s --parallel=%s（额外参数里写的 --jobs/--parallel 被页面配置覆盖）' % (jb, pl), j['prog']['ok'] == 1 and A_ and all(('--jobs=%s' % jb) in a_.split() and ('--parallel=%s' % pl) in a_.split() and '--jobs=1' not in a_.split() for a_ in A_), (A_[:2], j['prog']))
    sdk.Oss = FakeOss; c.post('/api/config', json={'ds_jobs': '2', 'jobs': '16', 'parallel': '4', 'ossutil_args': ''})

    # ---- UMI_v3 没有设备文件夹这一层（umi_normal\日期\PRT_…\会话）：设备留空，不拿设备SN 顶替；ISuit-V3 同样情形仍取 SN 后 4 位
    dataN = os.path.join(TMP, 'data_nodev'); make_umi_session(dataN, 'umi_normal', '20261108', 'PRT_XZ_room001', 'UMI_v3_20261108_150000', dev=None, conf='device_SN: prt004\n')
    make_session(dataN, 'umi_normal', '20261108', 'PRT_XZ_room001', 'ISuit-V3_2026-11-08-16-00-00', 'zhangsan', slices=8, dev=None)
    for r_ in c.get('/api/state').get_json()['roots']: c.post('/api/roots', json={'remove': [r_]})
    c.post('/api/roots', json={'add': [dataN]}); c.post('/api/scan', json={}); wait_scan(c); rn = {d['name']: d for d in c.get('/api/state').get_json()['datasets']}
    check('没有设备文件夹：UMI_v3 的设备留空（设备SN 仍是 prt004，不拿它顶替、不报不一致）', rn['UMI_v3_20261108_150000']['device'] == '' and rn['UMI_v3_20261108_150000']['device_sn'] == 'prt004' and not rn['UMI_v3_20261108_150000']['dev_diff'], rn['UMI_v3_20261108_150000'])
    check('没有设备文件夹：ISuit-V3 照旧取 SN 后 4 位作设备', rn['ISuit-V3_2026-11-08-16-00-00']['device'] == '1CA8', rn['ISuit-V3_2026-11-08-16-00-00']['device'])

    # ---- 真实数据样本（用户提供的 UMI_v3_20261117_130624，视频与大 csv 已删减）：路径层级与 ISuit-V3 一样 umi_normal\{设备}\{日期}\PRT_…\会话
    fx = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'fixtures', 'UMI_v3_20261117_130624')
    data5 = os.path.join(TMP, 'data5'); sess5 = os.path.join(data5, 'umi_normal', 'prt004', '20261117', 'PRT_WX_room005', 'UMI_v3_20261117_130624'); shutil.copytree(fx, sess5)
    for r_ in c.get('/api/state').get_json()['roots']: c.post('/api/roots', json={'remove': [r_]})
    c.post('/api/roots', json={'add': [data5]}); c.post('/api/scan', json={}); wait_scan(c); st = c.get('/api/state').get_json()
    check('真实样本：识别 1 个会话、无扫描问题（conf.yaml 带 !<tag:yaml.org,2002:str> 标签也能解析）', len(st['datasets']) == 1 and st['problems'] == [], (st['problems'], len(st['datasets'])))
    rd = st['datasets'][0]
    check('真实样本：OSS 路径 = oss://prt-huawei/raw_data/raw_data_261117/umi_normal/PRT_WX_room005/UMI_v3_20261117_130624/', rd['oss_url'] == 'oss://prt-huawei/raw_data/raw_data_261117/umi_normal/PRT_WX_room005/UMI_v3_20261117_130624/' and rd['fmt'] == 'umi_v3', rd['oss_url'])
    check('真实样本：设备取设备文件夹 prt004（与 SN 一致，不报不一致）/ SN / 操作员 / 地点 / 任务(项目名 / 任务名)；sub_task 空列表 → 空串', rd['device'] == 'prt004' and rd['device_sn'] == 'prt004' and not rd['dev_diff'] and rd['dir_date'] == '20261117' and rd['operator'] == '采集员' and rd['location'] == '地址'
          and rd['task'] == '华为居家采集 / 玩具分类收纳-自然光' and rd['sub_task'] == '', rd)
    check('真实样本：录制日志给出 13:06:24 → 13:17:15，操作录制时长 651.5 秒', rd['start'] == '2026-11-17 13:06:24' and rd['end'] == '2026-11-17 13:17:15' and rd['duration'] == 651.5 and rd['slices'] == 0, (rd['start'], rd['end'], rd['duration']))
    check('真实样本：没有任何假警告（日期/房间/设备文件夹/错放）', not (rd['date_warn'] or rd['room_bad'] or rd['room_diff'] or rd['dev_diff'] or rd['misplaced']), rd)
    j = upload_all(c, [rd['id']], method='sdk', order='folder', ds_jobs='1')
    base5 = RB('prt-huawei') + 'raw_data/raw_data_261117/umi_normal/PRT_WX_room005/UMI_v3_20261117_130624/'
    rf = sorted(os.path.relpath(os.path.join(r_, f_), base5).replace(os.sep, '/') for r_, _, fs in os.walk(base5) for f_ in fs); mf5 = json.loads(open(base5 + 'manifest.json', encoding='utf-8').read())
    check('真实样本：SDK 上传到 prt-huawei，远端含 metadata/conf.yaml、带空格的 left hand/ right hand/、manifest.json、_READY；清单 capture_date=2026-11-17',
          j['prog']['ok'] == 1 and 'metadata/conf.yaml' in rf and 'left hand/camera-ll-hand.frames.csv' in rf and 'right hand/camera-rr-hand.frames.csv' in rf and '_READY' in rf and 'manifest.json' in rf
          and mf5['dataset_id'] == 'UMI_v3_20261117_130624' and mf5['capture_date'] == '2026-11-17', (j['prog'], rf))
    nolog = os.path.join(TMP, 'data5nolog'); shutil.copytree(fx, nolog); os.remove(os.path.join(nolog, 'metadata', 'recording.log')); tt = umi_times(nolog, 'UMI_v3_20261117_130624')
    check('真实样本去掉录制日志：用 frames.csv 曝光时间兜底（≈651.47 秒，取三路相机最早/最晚）', abs(tt['duration'] - 651.47) < 0.01 and tt['start'] == '2026-11-17 13:06:24', tt)
    for r_ in c.get('/api/state').get_json()['roots']: c.post('/api/roots', json={'remove': [r_]})
    c.post('/api/roots', json={'add': [data]})

    print('7 生命周期')
    dbx("INSERT INTO runs(action,method,ds_jobs,total,ok,fail,status,started_at) VALUES('upload','sdk·逐文件夹',2,9,0,0,'running','t')")
    init_db()                                                                  # 模拟：上次进程被直接关闭，重启后检测
    n = c.get('/api/state').get_json()['notice']
    check('被中断的任务重启后提示', n and n['status'] == 'interrupted' and n['total'] == 9, n)
    c.post('/api/notice/ack'); check('确认后不再提示', c.get('/api/state').get_json()['notice'] is None)
    p = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)']); register_child(p); terminate_children(); time.sleep(.3)
    check('关闭时终止子进程', p.poll() is not None)
    called = []; api_mod.shutdown_later = lambda *a, **k: called.append(1)
    check('退出服务接口', c.post('/api/shutdown', json={}).status_code == 200 and called)

    node = shutil.which('node')
    if node:
        bad = [f for f in sorted(os.listdir(os.path.join(ROOT, 'web', 'js'))) if subprocess.run([node, '--check', os.path.join(ROOT, 'web', 'js', f)], capture_output=True).returncode]
        check('前端 JS 语法', not bad, bad)
    shutil.rmtree(TMP, True)
    print('\n%s：%d 项失败' % ('失败' if FAILS else '全部通过', len(FAILS)) if FAILS else '\n全部通过')
    sys.exit(1 if FAILS else 0)


if __name__ == '__main__':
    main()
