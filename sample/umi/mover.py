# -*- coding: utf-8 -*-
"""在 umi_normal 与 umi_calibration 之间移动会话（同一层级结构，只替换类型目录名）。
本地数据红线的唯一例外：只移动（os.rename，同盘原子操作，不复制不删除），且必须由用户在页面确认；目标已存在则拒绝，不覆盖、不合并。
两种用法（两种数据格式各用各的，互不影响）：
  ① 自动（ISuit-V3）：把 umi_normal 下被判定为标定的会话（scan.calib_reasons：时长 < 3 分钟 / 任务名含「标定」/ 采集配置声明）移到 umi_calibration。
  ② 手动（UMI_v3）：标定会话无法自动区分（要看视频里有没有二维码），用户在页面里看过视频后，自己选会话 常规 → 标定，或移回 标定 → 常规（to 参数）。
例：F:\\data\\umi_normal\\PRT_WX_room005\\UMI_v3_20261117_130624 → F:\\data\\umi_calibration\\PRT_WX_room005\\UMI_v3_20261117_130624"""
import os
from pathlib import Path
import upload_umi_sdk as sdk
from .state import DATASETS, LOCAL_DIRS, PL
from .db import save_datasets
from .util import make_ds
from .scan import inspect, session_sig

CAL, NOR = LOCAL_DIRS['calibration'][0], LOCAL_DIRS['normal'][0]
TYPE_DIR = {'normal': NOR, 'calibration': CAL}
TYPE_NAME = {'normal': '常规', 'calibration': '标定'}


def target_of(src, to='calibration'):
    """把路径里最靠后的类型目录换成目标类型目录：to=calibration：umi_normal（或旧拼写 umi_noraml）→ umi_calibration；to=normal：反过来。路径里没有源类型目录则返回 None。"""
    parts = Path(src).parts; names = set(LOCAL_DIRS['calibration' if to == 'normal' else 'normal'])
    idx = [i for i, p in enumerate(parts[:-1]) if p.lower() in names]
    return str(Path(*parts[:idx[-1]], TYPE_DIR[to], *parts[idx[-1] + 1:])) if idx else None


def new_rel_of(rec, to='calibration'):
    return TYPE_DIR[to] + '/' + rec['rel'].split('/', 1)[1]


def plan_one(rec, rels=None, to=None):
    """一个数据集的移动计划：{id,name,source,target,to,reason,blocked}。blocked 非空 = 不能移动（原因）。
    to=None：自动模式，只处理「常规目录里被判定为标定」的会话，其他返回 None；to=normal/calibration：手动模式（仅 UMI_v3），用户指定方向。"""
    if to is None:
        if rec['type'] != 'normal' or not rec.get('misplaced'): return None
        to, reason = 'calibration', rec['misplaced'].split('：', 1)[-1]
    else: reason = '手动选择'
    src, tgt = rec['source'], target_of(rec['source'], to)
    p = {'id': rec['id'], 'name': rec['name'], 'source': src, 'target': tgt or '', 'to': to, 'reason': reason, 'blocked': ''}
    if reason == '手动选择' and sdk.fmt_of_name(rec['name']) != 'umi_v3': p['blocked'] = '手动移动只用于 UMI_v3 数据'
    elif rec['type'] == to: p['blocked'] = '已经是%s数据' % TYPE_NAME[to]
    elif not tgt: p['blocked'] = '路径里找不到 %s 目录' % (CAL if to == 'normal' else NOR)
    elif not os.path.isdir(src): p['blocked'] = '本地目录不存在（可能已被移动/改名，请重新扫描）'
    elif os.path.exists(tgt): p['blocked'] = '目标已存在：%s' % tgt
    elif new_rel_of(rec, to) in (rels if rels is not None else {d['rel'] for d in list(DATASETS.values())}): p['blocked'] = '已存在相同 OSS 路径的数据集：%s' % new_rel_of(rec, to)
    return p


def plans(ids=None, to=None):
    """移动预览（只读）。to=None：自动模式，列出全部「错放」；to 给定：只列 ids 里的会话（手动模式）。"""
    with PL: cur = dict(DATASETS)
    recs = list(cur.values())
    rels = {r['rel'] for r in recs}                                  # 预先收集，避免每个候选都遍历全部数据集
    pick = [cur[i] for i in (ids or []) if i in cur] if to else recs
    return sorted((p for p in (plan_one(r, rels, to) for r in pick) if p), key=lambda p: p['source'])


def move_ids(ids, to=None):
    """移动选中的数据集，返回 (moved, failed)。每个都以现在的磁盘状态重新校验；成功后只重新统计被移动的会话，更新内存与 datasets 表。"""
    moved, failed = [], []
    for i in ids:
        with PL: rec = DATASETS.get(i)
        if not rec: failed.append({'id': i, 'name': '', 'error': '数据集不存在，请重新扫描'}); continue
        p = plan_one(rec, to=to)
        if not p: failed.append({'id': i, 'name': rec['name'], 'error': '不是可移动的数据（不在 umi_normal 或未判定为标定）'}); continue
        if p['blocked']: failed.append({'id': i, 'name': rec['name'], 'error': p['blocked']}); continue
        if rec.get('sig') and session_sig(rec['source']) != rec['sig']:    # 判定是扫描时的快照：之后内容变了（如还在录制）就不按旧状态移动
            failed.append({'id': i, 'name': rec['name'], 'error': '本地内容在上次扫描后有变化（可能还在采集），请重新扫描后再移动'}); continue
        try:
            os.makedirs(os.path.dirname(p['target']), exist_ok=True)
            os.rename(rec['source'], p['target'])                    # 同盘改名：原子，数据不复制；失败（被占用/无权限，如视频还在播放）则原样保留
        except OSError as e:
            failed.append({'id': i, 'name': rec['name'], 'error': str(e)}); continue
        try:
            new = inspect(make_ds(dict(rec, type=p['to'], source=p['target'], rel=new_rel_of(rec, p['to']))), None); new.pop('cached', None)
        except Exception:                                                # 文件夹已移动成功；重新统计失败时不留旧记录，提示重新扫描即可
            new = None
        with PL:
            DATASETS.pop(i, None)
            if new: DATASETS[new['id']] = new
        moved.append({'id': i, 'new_id': new['id'] if new else '', 'name': rec['name'], 'target': p['target'], 'to': p['to'], 'rescan': new is None})
    if moved:
        with PL: recs = list(DATASETS.values())
        save_datasets(recs)
    return moved, failed
