# agent.md — AI 入口

**先读 `README.md`**（§3 结构、§4 业务规则、§11 工作指南）。改完必须跑：`python tests/smoke.py`（全部通过才算完成）。

## 红线（摘要，详见 README §11）
- 本地数据只读（唯一例外：页面确认后的「移到标定」，`umi/mover.py`，只做 `os.rename`）；不写日志文件（日志只在内存）；校验对远端只读。
- 上传提交顺序不可调换：数据文件 → 复核 → `manifest.json` → `_READY`。
- 方式=ossutil 时不得创建 `sdk.Oss`（它会要求 oss2/boto3），所有 OSS 操作走 `OssutilOss`；
- 复用 `upload_umi_sdk.py` 的清单/哈希/提交逻辑；AK/SK 不下发前端、不进日志。
- 模块分层：`state < util < db < lifecycle < ossutil_client < progress < scan < mover < uploader < verify < jobs < api`，下层不得导入上层；共享可变状态只放 `umi/state.py`，原地修改。
- 前端用 `data-act` 事件委托，不写内联 onclick；改表结构先改 `umi/schema.sql` 并升 `DB_VERSION`。

- 两种数据格式并存（ISuit-V3 / UMI_v3）：**目录名决定格式，格式决定桶**（`hw-upload` / `prt-huawei`，配置里可改），规则只在 `upload_umi_sdk.py`（`FORMATS`/`fmt_of_name`/`bucket_for`）定义；不要在别处硬编码 `ISuit-V3` 正则或桶名，用 `sdk.session_date(name)` / `util.cap_of` / `sdk.bucket_for`。每个数据集带自己的桶（`ds['bucket']`），上传/校验按数据集建客户端，**不要再用 `sdk.BUCKET` 全局代替数据集的桶**。ISuit-V3 自动判定标定，UMI_v3 没有自动判定（标定要看视频里的二维码），由用户在页面看视频后手动移动；`umi/videos.py`、`umi/nettune.py` 是无依赖的叶子模块。详见 README §4.11 / §4.12。

## 原始业务背景（数据格式，供需要时查阅）
- 会话目录 `ISuit-V3_YYYY-MM-DD-HH-MM-SS/`：`datacollect_conf.yaml`（device_SN、record_purpose Normal/Start/End、record_operator、record_location、task_name、location_type…）、`app_ops.yaml`、分片目录 `<会话>_N/`（约 30 秒一片，内含 `<会话>_N.yaml`：`head_time`/`tail_time` 纳秒级 Unix 时间戳 + 相机/雷达标定、`timestamps.record`、`camera|imu|encoder|system/*.record`）。
- UMI_v3（桶 prt-huawei）：会话目录 `UMI_v3_YYYYMMDD_HHMMSS/`，含 `metadata/conf.yaml`、`head/`、`left hand/`、`right hand/`，没有分片；采集时长取自 conf（读不到记 0），见 README §4.11。
- `.record` 为 Apollo CyberRT 容器，`record_encrypt: true` 时数据块加密，**无需也不应尝试解密**。
- 采集时长 = 各分片 `tail_time − head_time` 之和；采集时间转北京时间（UTC+8）。
- 编码容错读取 yaml：`utf-8 → gb18030 → latin1`。
- 错放：常规数据在 `umi_calibration`、或标定数据（Start/End/Calibration 或 location_type=标定）在 `umi_normal`。

## 待做（需求池）
- 操作员工作量汇总页；开/收工标定与常规数据的每日闭环表（主键：设备编号 + 采集日 + 操作员；Start=开工、End=收工，统计其间 Normal 会话数与时长，标记 完整/缺开工/缺收工）。
