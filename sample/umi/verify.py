# -*- coding: utf-8 -*-
"""校验（只读）：本地 vs 远端 的文件列表/大小，深度校验再对照 manifest 里的 sha256。"""
import json
import time
import upload_umi_sdk as sdk
from .db import dbx
from .util import fmt_ts
from .uploader import inventory


# ------------------------------------------------------------ 校验（远程 vs 本地，只读）
def local_inventory(src):
    return {r: st.st_size for r, _, st in inventory(src)}


def verify_one(oss, ds, deep, job):
    raw, local = ds['raw_uri'], local_inventory(ds['source'])
    remote = {f['name']: f['size'] for f in oss.list_objects(raw) if not sdk.is_excluded(f['name'])}
    missing = sorted(k for k in local if k not in remote); extra = sorted(k for k in remote if k not in local)
    diff = sorted(k for k in local if k in remote and remote[k] != local[k])
    rd = oss.read_object(raw + '_READY'); ready, bad, revision = rd is not None, [], ''
    if ready:
        try: revision = json.loads(rd.decode('utf-8')).get('revision', '')
        except Exception: pass
    if deep and ready:
        rh = {f['path']: f['sha256'] for f in json.loads(oss.read_object(raw + 'manifest.json').decode('utf-8'))['files']}
        job['prog'].update(phase='深度校验 sha256', total=sum(local.values()), hashed=0)
        bad = sorted(f['path'] for f in sdk.snapshot(ds['source'], 8) if rh.get(f['path']) != f['sha256'])
    status = '未上传' if not remote else '不一致' if (missing or extra or diff or bad) else '缺_READY' if not ready else '一致'
    return {'status': status, 'local_files': len(local), 'remote_files': len(remote), 'missing': missing[:20], 'extra': extra[:20],
            'size_diff': diff[:20], 'hash_diff': bad[:20], 'ready': ready, 'revision': revision, 'deep': deep, 'checked_at': fmt_ts(time.time())}


def save_check(rel, r):
    dbx('INSERT OR REPLACE INTO checks VALUES(?,?,?,?,?,?)', (rel, r['status'], int(r['ready']), int(r['deep']), json.dumps(r, ensure_ascii=False), r['checked_at']))
