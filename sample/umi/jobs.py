# -*- coding: utf-8 -*-
"""后台任务：数据集级并发的上传/校验，写上传记录与校验结果。"""
import concurrent.futures as cf
import sqlite3
import time
import upload_umi_sdk as sdk
from .state import CUR, PL, STATE_DIR
from .db import apply_cfg, dbx, finish_run
from .util import fmt_ts, make_ds, mlabel, resolve_ossutil, tag_of
from .progress import log
from .ossutil_client import OssutilOss, write_ossutil_cfg
from .uploader import do_upload, net_cb
from .verify import save_check, verify_one


# ------------------------------------------------------------ 后台任务（数据集级并发）
class QuietUi:
    def line(self, *a, **k): pass


def make_oss(c):
    """c['bucket'] = 这个数据集的目标桶（ISuit-V3 与 UMI_v3 各用各的桶，process_one 按数据集填入）。
    方式=ossutil：所有 OSS 操作走 ossutil（不需要 oss2 / boto3）；方式=SDK：用 sdk.Oss（阿里云需要 oss2，S3/MinIO 需要 boto3）。"""
    if c['method'] == 'ossutil': return OssutilOss(c)
    return sdk.Oss(STATE_DIR, int(c['jobs'] or 16), int(c['parallel'] or 4), QuietUi(), c['bucket'])


def new_ctx(job, rec, ds):
    """每个数据集一个进度上下文：与 job 共享日志与速度计，进度字段各自独立，由 prog_view 汇总。"""
    return {'log': job['log'], 'meter': job['meter'], 'run_id': job.get('run_id'), 'last_speed': 0, 'tag': tag_of(rec['name']),
            'prog': {'phase': '准备', 'name': rec['rel'], 'url': ds['raw_uri'], 'sent': 0, 'hashed': 0, 'total': 0, 'files': 0, 'total_files': 0,
                     'raw': '', 'rep_speed': None, 'file': '', 'fdone': 0, 'ftotal': 0}}


UPLOAD_COLS = ('run_id', 'rel', 'oss_url', 'source', 'device', 'device_sn', 'dir_date', 'cap_date', 'task', 'sub_task', 'operator', 'location', 'purpose', 'type', 'room',
               'start_time', 'end_time', 'slices', 'size', 'duration', 'files', 'status', 'method', 'revision', 'elapsed', 'avg_speed', 'error', 'created_at')


def add_upload(rec, ds, c, status, nf, rev, elapsed, speed, err, run_id=None, method=None):
    """写一条上传记录：同时带上数据列表的全部字段（快照）。method 缺省按当前上传方式生成。"""
    v = dict(run_id=run_id, rel=rec['rel'], oss_url=ds['raw_uri'], source=rec['source'], device=rec['device'], device_sn=rec.get('device_sn', ''), dir_date=rec['dir_date'],
             cap_date=rec['cap_date'], task=rec['task'], sub_task=rec.get('sub_task', ''), operator=rec['operator'], location=rec['location'], purpose=rec['purpose'],
             type=rec['type'], room=rec['room'], start_time=rec['start'], end_time=rec['end'], slices=rec['slices'], size=rec['size'], duration=rec['duration'], files=nf,
             status=status, method=method or mlabel(c), revision=rev, elapsed=round(elapsed), avg_speed=speed, error=err, created_at=fmt_ts(time.time()))
    dbx('INSERT INTO uploads(%s) VALUES(%s)' % (','.join(UPLOAD_COLS), ','.join('?' * len(UPLOAD_COLS))), tuple(v[k] for k in UPLOAD_COLS))


def backfill_upload(rec, ds, r, run_id):
    """校验发现远端已提交（状态「一致」）但上传记录里没有这个数据集 → 补一条（方式=校验补录，状态=已存在）。
    禁止重复：已有「成功/已存在」记录，或已经补录过（唯一索引 ux_uploads_backfill），都不再插入。"""
    if dbx("SELECT 1 FROM uploads WHERE rel=? AND status IN ('成功','已存在') LIMIT 1", (rec['rel'],)):
        return False
    try:
        add_upload(rec, ds, {}, '已存在', r['remote_files'], r.get('revision', ''), 0, 0, '', run_id, method='校验补录')
    except sqlite3.IntegrityError:
        return False
    return True


def process_one(job, i, rec, action, c):
    it = job['items'][i]; it['state'] = 'running'
    ds, t0 = make_ds(rec), time.time(); ctx = new_ctx(job, rec, ds)
    c = dict(c, bucket=ds['bucket'])                              # 这个数据集用自己格式对应的桶（OSS 客户端、断网探测都按它）
    with PL: job['actives'].append(ctx)
    if action == 'verify_deep': CUR['job'] = ctx
    try:
        oss = make_oss(c)
        if c['method'] == 'ossutil': oss.on_net = net_cb(ctx)         # 收尾阶段（列表/写 manifest/_READY）断网时同样显示等待状态
        log(ctx, '▶ [%d/%d] %s  [%s → 桶 %s]' % (i + 1, job['count'], rec['rel'], sdk.FORMATS[ds['fmt']]['label'], ds['bucket'])); log(ctx, '   OSS 路径: ' + ds['raw_uri'])
        if action == 'upload' and str(rec.get('location') or '').strip() == 'Default':      # 与官方客户端一致（两种格式通用）
            log(ctx, '   提示：采集位置为 Default，后续交付需要正确的房间元数据；上传程序不会自动猜测房间。')
        if action == 'upload':
            st, rev, nf = do_upload(ctx, oss, rec, ds, c)
            el = max(time.time() - t0, 0.001)
            add_upload(rec, ds, c, st, nf, rev, el, rec['size'] / el if st == '成功' else 0, '', ctx['run_id'])
            save_check(rec['rel'], {'status': '一致', 'local_files': nf, 'remote_files': nf, 'missing': [], 'extra': [], 'size_diff': [], 'hash_diff': [], 'ready': True, 'deep': False, 'checked_at': fmt_ts(time.time())})
            it.update(state='done', msg=st)
            log(ctx, '   ✔ %s  %d 个文件 %s  用时 %.0fs' % (st, nf, sdk.format_size(rec['size']), el) + ('  平均 %s/s' % sdk.format_size(rec['size'] / el) if st == '成功' else ''))
        else:
            r = verify_one(oss, ds, action == 'verify_deep', ctx); save_check(rec['rel'], r)
            it.update(state='done', msg=r['status']); log(ctx, '   校验: %s  本地%d / 远端%d 个文件' % (r['status'], r['local_files'], r['remote_files']))
            if r['status'] == '一致' and backfill_upload(rec, ds, r, ctx['run_id']): log(ctx, '   已补录上传记录（远端已提交，本地原无记录）')
    except Exception as e:
        log(ctx, '   ✘ 失败: %s' % e); it.update(state='fail', msg=str(e)[:200])
        if action == 'upload': add_upload(rec, ds, c, '失败', 0, '', time.time() - t0, 0, str(e)[:500], ctx['run_id'])
    finally:
        with PL: job['base'] += job['units'][i]; job['actives'].remove(ctx)


def run_job(job, action, recs, c):
    """按清单顺序依次启动数据集，同时最多 ds_jobs 个并发（深度校验受磁盘限制固定为 1）。"""
    try:
        apply_cfg(); c = dict(c, ossutil_path=resolve_ossutil(c['ossutil_path']))
        if c['method'] == 'ossutil': c['_cfg'] = write_ossutil_cfg(c)
        with cf.ThreadPoolExecutor(1 if action == 'verify_deep' else max(1, int(c['ds_jobs'] or 1))) as pool:
            for f in [pool.submit(process_one, job, i, r, action, c) for i, r in enumerate(recs)]: f.result()
    except Exception as e:
        log(job, '[错误] %s' % e)
        for it in job['items']:
            if it['state'] in ('queued', 'running'): it.update(state='fail', msg=str(e)[:200])
    finish_run(job.get('run_id'), sum(i['state'] == 'done' for i in job['items']), sum(i['state'] == 'fail' for i in job['items']))
    job['done'], job['t_end'] = True, time.time(); CUR['job'] = None
