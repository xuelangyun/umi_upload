# -*- coding: utf-8 -*-
"""进程级共享状态与常量：路径、时区、内存中的数据集/任务/扫描状态、临时目录。所有模块只通过这里共享可变状态（字典/列表原地修改，不要重新赋值）。"""
import atexit
import datetime
import shutil
import tempfile
import threading
from pathlib import Path
import upload_umi_sdk as sdk


HERE = Path(__file__).resolve().parent.parent                    # umi_web.py 所在目录（数据库、ossutil.exe 默认放这里）


DB_PATH = str(HERE / 'umi_web.db')


CKPT_DIR = str(HERE / 'umi_ckpt')                           # ossutil 断点续传目录（持久化：断网/重启后可续传；数据集提交成功后自动删除，可随时手动删除）


TZ = datetime.timezone(datetime.timedelta(hours=8))


LOCAL_DIRS = {'normal': ('umi_normal', 'umi_noraml'), 'calibration': ('umi_calibration',)}   # umi_noraml 仅兼容旧拼写


SKIP_DIRS = {'windows', 'program files', 'program files (x86)', 'programdata', 'system volume information', 'node_modules'}


OK_STATES = ('成功', '已存在')                              # 上传记录里算「已上传」的状态


PL = threading.Lock()
MOVING = threading.Lock()                                   # 「移到标定」进行中：此时拒绝扫描/上传/校验（会改本地目录）


STATE_DIR = tempfile.mkdtemp(prefix='umi_web_')            # 仅 oss2 断点续传 checkpoint，退出即删


SDK_DEFAULTS = {'endpoint': sdk.ENDPOINT, 'ak': sdk.ACCESS_KEY_ID, 'sk': sdk.ACCESS_KEY_SECRET, 'bucket': sdk.BUCKET, 'bucket_umi': sdk.BUCKET_UMI}


SCAN = {'id': 0, 'running': False, 'stage': '', 'done': 0, 'total': 0, 't0': 0.0, 't1': 0.0, 'result': None}


DATASETS, JOBS, PROBLEMS, CUR = {}, {}, [], {'job': None}
CHILDREN = set()                                           # 正在运行的 ossutil 子进程（关闭时统一终止）


atexit.register(shutil.rmtree, STATE_DIR, True)
