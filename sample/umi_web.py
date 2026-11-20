#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""UMI 数据中心 —— 入口。用法：python umi_web.py [--port 8765] [--host 127.0.0.1] [--db 数据库路径] [--no-browser]
直接关闭 cmd 窗口即可退出（会自动终止 ossutil 子进程并清理，未完成的上传下次可继续）。结构说明见 README.md。"""
import argparse
import logging
import threading
import webbrowser

from umi import state
from umi.api import app
from umi.db import apply_cfg, init_db
from umi.lifecycle import install_handlers


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--port', type=int, default=8765)
    ap.add_argument('--host', default='127.0.0.1')
    ap.add_argument('--db', default=state.DB_PATH)
    ap.add_argument('--no-browser', action='store_true')
    a = ap.parse_args()
    state.DB_PATH = a.db
    init_db(); apply_cfg(); install_handlers()
    logging.getLogger('werkzeug').setLevel(logging.WARNING)      # 页面每 0.6 秒轮询一次，别让请求日志刷屏
    url = 'http://%s:%d' % ('127.0.0.1' if a.host in ('0.0.0.0', '') else a.host, a.port)
    print('UMI 数据中心已启动：%s\n数据库：%s\n直接关闭本窗口即可退出。' % (url, state.DB_PATH))
    if not a.no_browser:
        threading.Timer(1.0, webbrowser.open, args=(url,)).start()
    app.run(host=a.host, port=a.port, threaded=True, use_reloader=False)


if __name__ == '__main__':
    main()
