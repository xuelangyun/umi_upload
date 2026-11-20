# -*- coding: utf-8 -*-
"""SQLite：连接、迁移、配置、任务记录。表结构（含逐字段注释）在 umi/schema.sql。"""
import os
import sqlite3
import time
from pathlib import Path
import upload_umi_sdk as sdk
from . import state
from .state import SDK_DEFAULTS
from .util import fmt_ts

DB_VERSION = 6                                           # 结构变更时加 1，并在 init_db() 里补迁移
SCHEMA_FILE = Path(__file__).with_name('schema.sql')
DS_COLS = ('id', 'rel', 'name', 'type', 'source', 'dir_date', 'cap_date', 'device', 'device_sn', 'task', 'sub_task', 'purpose', 'operator', 'location',
           'slices', 'size', 'duration', 'start_time', 'end_time', 'misplaced', 'room', 'prt_local', 'prt_dir', 'sig')
UPLOAD_NEW_COLS = {'run_id': 'INTEGER', 'device_sn': 'TEXT', 'cap_date': 'TEXT', 'sub_task': 'TEXT', 'room': 'TEXT'}      # 旧版 uploads 表缺的列，启动时 ALTER 补上


def db():
    """每次调用一个新连接（线程安全）；WAL + synchronous=NORMAL：被直接关掉进程也不会损坏。"""
    c = sqlite3.connect(state.DB_PATH, timeout=30)
    c.row_factory = sqlite3.Row
    c.execute('PRAGMA synchronous=NORMAL')
    return c


def dbx(sql, args=()):
    with db() as c:
        return c.execute(sql, args).fetchall()


def _ensure_cols(c, table, cols):
    have = [r[1] for r in c.execute('PRAGMA table_info(%s)' % table)]
    if have:                                                  # 表已存在才补列
        for k, t in cols.items():
            if k not in have: c.execute('ALTER TABLE %s ADD COLUMN %s %s' % (table, k, t))


def init_db():
    """建表/迁移。v4/v5→v6：datasets 逐列增加了字段（下次扫描重建），uploads 用 ALTER 补列并回填，其余表原样保留；未知旧版本整体重建。"""
    with db() as c:
        c.execute('PRAGMA journal_mode=WAL')
        ver = c.execute('PRAGMA user_version').fetchone()[0]
        if ver not in (0, 4, 5, DB_VERSION):
            for t in [r[0] for r in c.execute("SELECT name FROM sqlite_master WHERE type IN ('table','view')")]:
                c.execute('DROP TABLE IF EXISTS "%s"' % t) if not t.startswith('v_') else c.execute('DROP VIEW IF EXISTS "%s"' % t)
        if ver in (4, 5):
            c.execute('DROP TABLE IF EXISTS datasets')
        _ensure_cols(c, 'uploads', UPLOAD_NEW_COLS)             # 必须在 executescript 之前：schema 里的索引会用到新列
        c.executescript(SCHEMA_FILE.read_text(encoding='utf-8'))
        c.execute("UPDATE uploads SET cap_date=substr(rel,-19,10) WHERE cap_date IS NULL OR cap_date=''")
        c.execute("UPDATE uploads SET room=substr(rel, instr(rel,'/')+1, instr(substr(rel, instr(rel,'/')+1),'/')-1) WHERE room IS NULL OR room=''")
        c.execute('PRAGMA user_version=%d' % DB_VERSION)
        # 上次进程被直接关闭（关 cmd / 断电）时，任务仍是 running：标记为 interrupted，页面会提示
        c.execute("UPDATE runs SET status='interrupted', finished_at=? WHERE status='running'", (fmt_ts(time.time()),))
    try:
        os.chmod(state.DB_PATH, 0o600)
    except OSError:
        pass
    state.DATASETS.update(load_datasets())


# ------------------------------------------------------------ 配置
def get_cfg():
    c = dict(SDK_DEFAULTS, jobs='16', parallel='4', method='sdk', ds_jobs='2', wan='1', ossutil_path='', ossutil_args='', net_wait='30', order='folder', ossutil_unit='slice')
    c.update({r['k']: r['v'] for r in dbx('SELECT k,v FROM settings')})
    return c


def apply_cfg():
    """把页面配置写进 sdk 模块。两种数据格式各有各的桶：ISuit-V3 → bucket，UMI_v3 → bucket_umi（由会话目录名决定格式，见 sdk.bucket_for）。"""
    c = get_cfg()
    sdk.ENDPOINT, sdk.ACCESS_KEY_ID, sdk.ACCESS_KEY_SECRET, sdk.BUCKET, sdk.BUCKET_UMI = c['endpoint'], c['ak'], c['sk'], c['bucket'], c['bucket_umi']
    return c


def set_setting(k, v):
    dbx('INSERT OR REPLACE INTO settings VALUES(?,?)', (k, str(v)))


# ------------------------------------------------------------ 扫描结果（datasets 表 ↔ 内存里的数据集字典）
def save_datasets(recs):
    """整体替换 datasets 表。内存里的字段名 start/end 对应列 start_time/end_time。"""
    now = fmt_ts(time.time())
    rows = [tuple({'start_time': r['start'], 'end_time': r['end']}.get(c, r.get(c)) for c in DS_COLS) + (now,) for r in recs]
    with db() as c:
        c.execute('DELETE FROM datasets')
        c.executemany('INSERT INTO datasets(%s,scanned_at) VALUES(%s)' % (','.join(DS_COLS), ','.join('?' * (len(DS_COLS) + 1))), rows)


def load_datasets():
    out = {}
    for r in dbx('SELECT * FROM datasets'):
        d = dict(r); d['start'], d['end'] = d.pop('start_time'), d.pop('end_time'); d.pop('scanned_at', None); out[d['id']] = d
    return out


# ------------------------------------------------------------ 任务运行记录
def start_run(action, method, ds_jobs, total):
    with db() as c:
        return c.execute('INSERT INTO runs(action,method,ds_jobs,total,ok,fail,status,started_at) VALUES(?,?,?,?,0,0,?,?)',
                         (action, method, ds_jobs, total, 'running', fmt_ts(time.time()))).lastrowid


def finish_run(run_id, ok, fail):
    if run_id:
        dbx("UPDATE runs SET ok=?, fail=?, status='done', finished_at=? WHERE id=? AND status='running'", (ok, fail, fmt_ts(time.time()), run_id))


def interrupted_notice():
    """最近一次「被中断」且未确认的任务（页面顶部提示条）。"""
    r = dbx("SELECT * FROM runs WHERE status='interrupted' AND id>CAST(COALESCE((SELECT v FROM settings WHERE k='ack_run'),'0') AS INTEGER) ORDER BY id DESC LIMIT 1")
    if not r: return None
    d = dict(r[0]); d['ok'] = dbx("SELECT COUNT(*) FROM uploads WHERE run_id=? AND status IN ('成功','已存在')", (d['id'],))[0][0]
    return d
