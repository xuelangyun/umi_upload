# -*- coding: utf-8 -*-
"""生命周期：直接关闭 cmd 窗口 / Ctrl+C / 页面「退出服务」都走同一个 cleanup()：
终止 ossutil 子进程 → 把未结束的任务标记为中断 → 删除临时目录。
数据安全由设计保证（与是否走到 cleanup 无关）：远端没有 manifest.json/_READY 就等于「没传完」，下次上传会跳过已完成的、从断点继续。"""
import atexit
import os
import shutil
import signal
import threading
import time
from .state import CHILDREN, PL, STATE_DIR
from .db import dbx
from .util import fmt_ts

_done = False
_refs = []                                               # 持有 Windows 回调/句柄，防止被垃圾回收


def register_child(p):
    with PL: CHILDREN.add(p)


def unregister_child(p):
    with PL: CHILDREN.discard(p)


def terminate_children():
    with PL: ps = list(CHILDREN)
    for p in ps:
        try: p.terminate()
        except Exception: pass
    t0 = time.time()
    for p in ps:
        try: p.wait(timeout=max(0.1, 2 - (time.time() - t0)))
        except Exception:
            try: p.kill()
            except Exception: pass


def cleanup():
    global _done
    if _done: return
    _done = True
    terminate_children()
    try: dbx("UPDATE runs SET status='interrupted', finished_at=? WHERE status='running'", (fmt_ts(time.time()),))
    except Exception: pass
    shutil.rmtree(STATE_DIR, True)


def exit_now(code=0):
    cleanup()
    os._exit(code)                                       # 不等其他线程，立即退出


def shutdown_later(delay=0.4):
    """页面「退出服务」：先让 HTTP 响应发出去，再退出。"""
    threading.Timer(delay, exit_now).start()


def install_handlers():
    atexit.register(cleanup)
    for name in ('SIGINT', 'SIGTERM', 'SIGBREAK', 'SIGHUP'):
        sig = getattr(signal, name, None)
        if sig is not None:
            try: signal.signal(sig, lambda *_: exit_now())
            except (ValueError, OSError): pass
    if os.name == 'nt':
        _win_console_handler()
        _win_job_object()


def _win_console_handler():
    """Windows：点 cmd 窗口的 ✕（CTRL_CLOSE_EVENT）时，Python 不会产生信号，需要自己注册控制台事件回调。"""
    try:
        import ctypes
        from ctypes import wintypes
        proto = ctypes.WINFUNCTYPE(wintypes.BOOL, wintypes.DWORD)

        def handler(ctrl):                               # 0 Ctrl+C  1 Ctrl+Break  2 关闭窗口  5 注销  6 关机
            cleanup()
            return False
        _refs.append(proto(handler))
        ctypes.windll.kernel32.SetConsoleCtrlHandler(_refs[-1], True)
    except Exception:
        pass


def _win_job_object():
    """Windows：把本进程放进「关闭即杀」的 Job 对象，进程无论怎么结束，ossutil 等子进程都会一起被系统终止（兜底）。"""
    try:
        import ctypes
        from ctypes import wintypes

        class Basic(ctypes.Structure):
            _fields_ = [('PerProcessUserTimeLimit', ctypes.c_int64), ('PerJobUserTimeLimit', ctypes.c_int64), ('LimitFlags', wintypes.DWORD),
                        ('MinimumWorkingSetSize', ctypes.c_size_t), ('MaximumWorkingSetSize', ctypes.c_size_t), ('ActiveProcessLimit', wintypes.DWORD),
                        ('Affinity', ctypes.c_size_t), ('PriorityClass', wintypes.DWORD), ('SchedulingClass', wintypes.DWORD)]

        class Io(ctypes.Structure):
            _fields_ = [(n, ctypes.c_uint64) for n in ('r', 'w', 'o', 'rb', 'wb', 'ob')]

        class Ext(ctypes.Structure):
            _fields_ = [('Basic', Basic), ('Io', Io), ('ProcessMemoryLimit', ctypes.c_size_t), ('JobMemoryLimit', ctypes.c_size_t),
                        ('PeakProcessMemoryUsed', ctypes.c_size_t), ('PeakJobMemoryUsed', ctypes.c_size_t)]
        k = ctypes.windll.kernel32
        k.CreateJobObjectW.restype = wintypes.HANDLE
        k.GetCurrentProcess.restype = wintypes.HANDLE
        k.AssignProcessToJobObject.argtypes = [wintypes.HANDLE, wintypes.HANDLE]
        k.SetInformationJobObject.argtypes = [wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p, wintypes.DWORD]
        job = k.CreateJobObjectW(None, None)
        info = Ext(); info.Basic.LimitFlags = 0x2000     # JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
        k.SetInformationJobObject(job, 9, ctypes.byref(info), ctypes.sizeof(info))
        k.AssignProcessToJobObject(job, k.GetCurrentProcess())
        _refs.append(job)
    except Exception:
        pass
