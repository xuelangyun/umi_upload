# -*- coding: utf-8 -*-
"""多出口（多条 WAN 负载均衡）下的并发建议。本模块不依赖其他 umi 模块。

为什么要「更多并发」：路由器做负载均衡一般是按「连接」把流量分到各个出口（同一条 TCP 连接始终走同一个出口）。
一条连接的速度有上限（受往返延迟、丢包限制，往往只有几十 Mbps），所以想把 N 个出口都跑满，就得有足够多的并发连接，
并且连接数要远大于出口数——这样哈希出来的分布才比较均匀，不会有的出口很挤、有的出口闲着。

ossutil 的连接数 ≈ 同时运行的 ossutil 进程数（数据集并发 ds_jobs）× 每个进程里同时传的大文件数 × 每个大文件的分片并发数（--parallel）。
一个文件夹里通常只有 2~3 个大视频，所以按「每个进程约 3 个大文件在传」估算。经验值：每个出口 ≥ 24 条连接。
"""

BIG_FILES = 3          # 估算用：每个 ossutil 进程里同时在传的大文件数（UMI_v3 每个文件夹约 2 个 mp4 + 几个大 csv）
PER_WAN = 24           # 经验值：每个出口至少这么多条并发连接，负载均衡才容易均匀


def estimate(ds_jobs, parallel, wan=1):
    """估算同时连接数（只算分片上传的大文件）：ds_jobs × BIG_FILES × parallel；per_wan = 平均每个出口多少条。"""
    conns = int(ds_jobs) * BIG_FILES * int(parallel)
    return {'conns': conns, 'per_wan': round(conns / max(1, int(wan)), 1), 'enough': conns >= PER_WAN * max(1, int(wan))}


def recommend(wan):
    """出口数 → 建议的并发参数 {ds_jobs, jobs, parallel}（页面顶栏「并发」下拉只有 1/2/3/4/6/8，所以数据集并发封顶 8）。
    1 个出口 = 程序默认值；每多一个出口，数据集并发 +2（多开 ossutil 进程，各自一批连接），出口多时再加大分片并发与文件并发。"""
    wan = min(max(int(wan), 1), 16)
    ds_jobs = min(2 * wan, 8)
    parallel = 4 if wan == 1 else 6 if wan <= 3 else 8
    jobs = 16 if wan <= 2 else 32
    return dict({'ds_jobs': ds_jobs, 'jobs': jobs, 'parallel': parallel}, **estimate(ds_jobs, parallel, wan))
