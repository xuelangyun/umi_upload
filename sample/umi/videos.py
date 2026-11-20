# -*- coding: utf-8 -*-
"""会话里的视频：列出 + 安全解析路径（给页面「点会话直接看视频」用）。
UMI_v3 的标定会话要看视频里有没有二维码才能确定，所以页面里点会话就弹出播放器。视频由 /api/video 以 Range 方式流式返回（可拖动进度）。
只允许读取会话目录内的视频文件（realpath 校验，拒绝 .. 和指向目录外的符号链接）。本模块不依赖其他 umi 模块。"""
import os

VIDEO_EXT = ('.mp4', '.m4v', '.mov', '.webm')
GROUP_ORDER = {'head': 0, 'left hand': 1, 'right hand': 2}        # 头部、左手、右手


def list_videos(src):
    """会话目录里的视频 [{'f': 'head/camera-hl-head.mp4', 'size': 字节}]：会话根与第一级采集目录（head / left hand / right hand），不含 metadata。"""
    out = []
    try:
        for d in os.scandir(src):
            if d.is_file(follow_symlinks=False) and d.name.lower().endswith(VIDEO_EXT): out.append((d.name, d.stat().st_size))
            elif d.is_dir(follow_symlinks=False) and d.name != 'metadata':
                for f in os.scandir(d.path):
                    if f.is_file(follow_symlinks=False) and f.name.lower().endswith(VIDEO_EXT): out.append((d.name + '/' + f.name, f.stat().st_size))
    except OSError:
        pass
    out.sort(key=lambda t: (GROUP_ORDER.get(t[0].split('/')[0], 9), t[0].lower()))
    return [{'f': f, 'size': s} for f, s in out]


def resolve_video(src, f):
    """会话目录 src + 相对路径 f（正斜杠）→ 视频的真实路径；不是会话内的视频文件返回 None。"""
    parts = [x for x in (f or '').replace('\\', '/').split('/') if x]
    if not parts or '..' in parts or not parts[-1].lower().endswith(VIDEO_EXT): return None
    base = os.path.normcase(os.path.realpath(src)); p = os.path.realpath(os.path.join(src, *parts))
    return p if os.path.normcase(p).startswith(base + os.sep) and os.path.isfile(p) else None
