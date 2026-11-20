# UMI 数据中心

把 ISuit‑V3 / UMI 采集数据（`umi_normal` 常规、`umi_calibration` 标定）**扫描 → 统计 → 上传到 OSS/MinIO → 校验** 的本地 Web 工具。
浏览器操作，后端是 Python（Flask + SQLite）。上传逻辑与官方客户端一致（按清单顺序、`manifest.json` / `_READY` 最后写入），支持 SDK 与 ossutil 两种方式。

> 给 AI / 新维护者：先读 **§3 项目结构**、**§4 业务规则**、**§11 给 AI 的工作指南**，改完跑 `python tests/smoke.py`。

---

## 1. 快速开始（Windows）

1. 安装 Python 3.9+。
2. 双击 `start.bat`（自动安装依赖 flask/pyyaml 并启动，浏览器自动打开 `http://127.0.0.1:8765`）。**用 ossutil 方式不需要 oss2 / boto3**；只有选「SDK」方式才需要（阿里云 OSS 装 `oss2`，MinIO/S3 装 `boto3`）。
3. 页面里：**＋ 添加目录**（可一次勾选多个盘符）→ 填**城市 / 房间**（如 `xz` / `001`，生成 `PRT_XZ_room001`；都留空则取本地路径里的 `PRT_…` 目录）→ **扫描** → 勾选数据 → **上传选中**。
4. 首次使用在右上角 **配置** 里填 OSS 的 Endpoint / 两个桶 / AK / SK，点「保存并测试」。**两种数据格式可以同时使用**：会话目录名决定格式（`ISuit-V3_…` / `UMI_v3_…`），格式决定上传到哪个桶（ISuit-V3 → `hw-upload`，UMI_v3 → `prt-huawei`，可在配置里改），详见 §4.11。多条 WAN 出口做负载均衡时，在配置里填「网络出口数」并点「套用建议并发」，见 §4.12。
5. **直接关闭 cmd 窗口即可退出**（见 §9）。也可点右上角「退出」。

命令行参数：`python umi_web.py [--port 8765] [--host 127.0.0.1] [--db 路径] [--no-browser]`。
`ossutil.exe`（v1.7.19）放在 `umi_web.py` 同级即可，配置里「ossutil 路径」留空自动使用。

## 2. 功能一览

| 功能 | 说明 |
|---|---|
| 两种会话格式（并存） | 一次扫描同时识别 `ISuit-V3_…` 与 `UMI_v3_YYYYMMDD_HHMMSS`，每个会话按目录名定格式、按格式定桶（ISuit-V3 → hw-upload，UMI_v3 → prt-huawei）；各自的标定整理（ISuit 自动判定 / UMI 看视频手动）、各自上传到各自的桶、各自校验；上传/提交协议相同，见 §4.11 |
| 数据源 | 多个目录/盘符；在每个根（3 层内）里找 `umi_normal` / `umi_calibration`；后台扫描，带进度，防重复点击 |
| 数据列表 | 任务名/操作员/地点/采集时间/时长/大小/上传状态/远端校验/OSS 路径；一行过滤 + 「查询」；累计采集时长；日期/房间不一致整行标黄 |
| 上传 | SDK 或 ossutil；数据集并发；按清单顺序逐文件夹；单一总进度条、实时速度、剩余时间；已上传自动跳过 |
| 错放移动 | 「移到标定」：把 `umi_normal` 下被判定为标定的会话（时长 < 3 分钟 / 任务名含「标定」）移动到 `umi_calibration` 同层级目录；先预览勾选再确认，只改名移动，见 §4.9 |
| 校验 | 快速（文件列表+大小）/ 深度（再对照 manifest 里的 sha256），只读 |
| 上传记录 | **按查询条件导出 Excel**（上传记录/汇总/按设备/按人员 4 个工作表）；SQLite；带数据列表全部字段 + OSS 全路径(含 manifest.json/_READY)；按目录日期/任务/地点/采集日期/人员过滤；累计采集时长（按数据集去重）；可导出官方客户端 `upload-history` 格式 |
| 关闭安全 | 关 cmd / Ctrl+C / 页面「退出」都会终止 ossutil、记录中断；下次启动提示，可继续 |

---

## 3. 项目结构

```
umi_web.py              入口：解析参数 → init_db → 安装退出处理 → 启动 Flask
upload_umi_sdk.py       OSS 上传 SDK（官方逻辑：清单/哈希/分片/提交）。本项目复用，不要重写；运行时由页面配置覆盖其中的连接参数。
                        也是「目录名 → 会话格式 → 桶」的唯一定义处：FORMATS / fmt_of_name / session_date / bucket_for / validate_session；Oss 客户端按桶实例化（`Oss(..., bucket)`）
ossutil.exe             ossutil v1.7.19（Windows x64）
requirements.txt        flask / pyyaml / boto3（阿里云 OSS 另装 oss2）
start.bat / start.sh    一键安装依赖并启动
agent.md                AI 入口（红线 + 指向本文件）
tests/smoke.py          冒烟测试：假 OSS + 假 ossutil 跑完整流程
umi/                    后端包（分层：下层不得导入上层）
  state.py              常量与进程内共享状态（PL 锁、DATASETS、JOBS、SCAN、CHILDREN、STATE_DIR、DB_PATH）
  util.py               read_text(编码容错)、fmt_ts、rel_of/full_uri/cap_of(OSS 路径，按当前桶格式解析会话名)、make_ds、tag_of、resolve_ossutil、mlabel
  db.py + schema.sql    SQLite：连接(WAL)、init_db(迁移)、配置(apply_cfg，同时写入 ISuit-V3 桶与 UMI_v3 桶)、datasets 存取、runs(任务记录)
  lifecycle.py          关闭安全：cleanup、子进程登记/终止、Windows 控制台事件、Job 对象
  ossutil_client.py     ossutil 版 OSS 客户端 OssutilOss（ls/cp 读写，接口同 sdk.Oss）+ ossutil 临时配置文件
  progress.py           Meter(滑动窗口速度)、log、add_sent、hash_file、prog_view(多数据集汇总)
  mover.py              会话在 umi_normal ⇄ umi_calibration 间的移动：target_of(路径替换)、plan_one(预览/阻止原因；to=None 自动判定错放，to=normal|calibration 手动)、move_ids(os.rename + 只重新统计被移动的会话)
  videos.py             会话里的视频：list_videos、resolve_video（realpath 校验，只允许会话目录内的视频；无其他模块依赖）
  nettune.py            多出口并发建议：recommend(出口数)、estimate(并发→估算连接数)；无其他模块依赖
  scan.py               stats/inspect(统计会话，带签名缓存；按格式读 datacollect_conf.yaml / metadata/conf.yaml，umi_times 推 UMI_v3 的起止与时长)、find_dirs、row_info(日期/房间提示)、local_prt、run_scan(后台扫描)
  uploader.py           inventory、commit(manifest→_READY)、SDK 逐文件上传、ossutil 整夹上传、upload_ordered、do_upload
  verify.py             verify_one(只读校验)、save_check
  jobs.py               process_one / run_job：数据集级并发、写 uploads/checks/runs
  xlsx.py               最小 xlsx 写入器（只用标准库，导出 Excel 用）
  api.py                Flask 路由 + 静态文件
web/                    前端（无构建步骤，无外部依赖，内网可用）
  index.html            页面骨架（交互用 data-act / data-change）
  css/app.css           设计变量(含深色) + 组件样式
  js/util.js            全局状态 S、格式化、tag/cls、toast
  js/api.js             fetch 封装、setCfg
  js/panel.js           应用栏控件、中断提示、数据源、进度卡片、日志
  js/filters.js         过滤条（暂存—查询）、下拉选项、前端过滤、加载上传记录
  js/tables.js          DATA_COLS / REC_COLS 列定义、draw()、勾选、复制
  js/dialogs.js         弹窗：扫描提示、目录选择、OSS 配置、退出
  js/main.js            ACT 事件表、轮询 poll()、tab/scan/run 主流程
```

运行时产生的文件：`umi_web.db`（+ `-wal`/`-shm`，已在 .gitignore）。**不产生任何日志文件**（日志只在内存，页面显示）。

---

## 4. 业务规则（单一事实来源）

### 4.1 目录 → OSS 路径
```
本地  <根>/umi_normal|umi_calibration/<设备>/<日期文件夹>/[PRT_城市_room房间/]ISuit-V3_YYYY-MM-DD-HH-MM-SS/
OSS   oss://<桶>/raw_data/raw_data_<YYMMDD>/<umi_normal|umi_calibration>/PRT_<CITY>_room<ROOM>/<会话名>/
```
- **日期来自会话目录名**（不是日期文件夹，也不用手选）。例：`ISuit-V3_2026-11-08-10-30-00` → `raw_data_261108`。
- **房间（PRT 段）**：默认**跟随本地文件夹名**（`PRT_WX_room002`），改了文件夹名后重新扫描，OSS 路径随之更新。规范格式是 `PRT_城市_room三位数`；`PRT_WX_room2` 这类不规范写法在列表「房间」格显示**橙色**。处理办法：点击「房间」格 → 在资源管理器打开对应文件夹 → 改名为 `PRT_WX_room002` → 重新扫描；或扫描时勾选「补零」（只让 OSS 路径用 `room002`，本地不动，该格显示**紫色**表示与本地不同）。
- **城市 + 房间（界面，可选覆盖）**：两者都填才覆盖（`xz`+`001` → `PRT_XZ_room001`，城市自动大写、房间只取数字保留前导零）；都留空 = 跟随文件夹名；只填一个会被忽略并提示。**这两个值不再保存/恢复**（上一版自动恢复会让路径一直被旧值覆盖，导致「改了文件夹名没效果」）；覆盖生效时数据源卡片显示「覆盖本地 → PRT_…」，被覆盖的行紫色标出并在扫描提示里列出。实现：给 SDK 路线加 `city`/`room` 字段（`scan.run_scan`）。
- 本地目录兼容旧拼写 `umi_noraml`（仅扫描识别）；OSS 与数据库里一律 `umi_normal`。
- `rel` = OSS 路径去掉桶和 `raw_data/raw_data_YYMMDD/`，从 `umi_normal|umi_calibration` 开始，是数据库里数据集的主键。

### 4.2 数据行提示（只提示，不拦截；`scan.row_info`）
① `date_warn`：会话名日期 ≠ 所在日期文件夹 → 整行标黄、目录/采集日期变色；② `room_bad`：OSS 房间不是 3 位格式 → 「房间」格橙色；③ `room_diff`：OSS 房间 ≠ 本地目录里的 PRT（被覆盖/补零）→ 「房间」格紫色、整行标黄。扫描结束提示、页头「⚠ 提示 N」、上传前确认框、任务日志都会列出。点击「房间」格 = 在本机打开该 PRT 文件夹（`POST /api/open`，只允许本机访问，路径取自已扫描的数据集/上传记录）。

### 4.3 上传顺序与提交协议
1. 文件清单按路径 UTF‑16 排序（= `manifest.json` 里 `files` 的顺序；`_1`、`_10`、`_11` 排在 `_2` 前，与官方客户端一致）。
2. 「文件夹」= 会话下第一级目录（各分片 `_N/`），会话根目录文件单独一组；按首次出现顺序上传。
3. 全部传完 → 复核本地未变化（stat 签名）→ 远端列表核对文件名+大小 → 写 `manifest.json` → 读回确认 → 最后写 `_READY` → 读回确认。**任何一步失败都不写这两个文件**，远端没有它们就等于「没传完」。
4. 已提交（有 `_READY`）且文件大小一致 → 不再哈希/上传，记「已存在」。
5. **已上传** = `uploads` 最新状态为「成功/已存在」，或 `checks.ready=1`；校验发现远端为空则「远端缺失」。上传默认跳过已上传（勾选「含已上传」才重新核对）。

### 4.4 两种上传方式（页头一键切换，参数共用）
共用：endpoint/bucket/AK/SK、`jobs`(同时上传文件数)、`parallel`(单文件分片并发)、`ds_jobs`(同时上传的数据集数，默认 2)。
- **SDK**：文件夹内按 `jobs` 并发，每个文件「哈希→上传」单趟；`order=folder` 上个文件夹传完再传下一个，`order=pipeline` 按清单顺序流水线。
- **ossutil 断网续传**（§4.10）。
- **ossutil**：一次一个文件夹一个进程，`ossutil cp <本地文件夹/> <OSS 文件夹/> -r -f --config-file <临时cfg> --disable-ignore-error --output-dir --checkpoint-dir --jobs=N --parallel=M --bigfile-threshold --part-size`（根目录文件加 `--only-current-dir`），并发交给 ossutil 的 `--jobs`。
  `ossutil_unit=slice`（逐分片，严格有序，每个文件夹传完即核对）或 `session`（先用根目录文件做目录语义预检，再一个进程传整个会话，无空档、带宽利用最高）。
  sha256 由 `AheadHasher` 单线程后台计算，最多领先已上传字节 256MB（趁文件还在缓存里哈希，避免磁盘读两遍）。
  配置文件由页面参数临时生成（权限 600、退出即删，密钥不上命令行）。
  **方式选 ossutil 时，所有 OSS 操作都走 ossutil**（`ossutil_client.OssutilOss`，接口同 `sdk.Oss`）：上传文件、`ls` 列表、`cp` 读/写 `manifest.json` 与 `_READY`、校验、连接测试，**完全不需要 oss2 / boto3**（`sdk.Oss` 在 Endpoint 含 `.aliyuncs.com` 时一创建就要 `import oss2`，所以 ossutil 方式绝不创建它）。命令与官方客户端 `ossutil.js` 一致：`ls <uri>` 解析长格式（解析不完整就停止提交，不当成空目录）、`cp <uri> <本地> -f` 读（`NoSuchKey` → 不存在）、`cp <临时文件> <uri> -f --disable-ignore-error --output-dir` 写。
  **整会话模式的收尾**：整夹进程结束后 → 远端列表核对 → 读回确认 → ossutil 写 `manifest.json` → 读回确认 → ossutil 写 `_READY` → 读回确认（共 4 个 ossutil 进程：根目录预检、整会话、manifest.json、_READY，均一次一个）。
  若本地数据集含 `_pipeline`/`manifest.json`/`_READY` 等排除项，整夹上传会一并传上去，该数据集自动改为 **ossutil 逐文件上传**（仍单进程、按清单顺序，不退回 SDK）。
- ossutil 事实（取自你提供的 exe）：v1.7.19；进度行 `Total num: N, size: X. Dealed num: N, Progress: 25.123%, Speed: 1234.56KB/s`；结束行 `Succeed: Total num: …`。解析不到进度不影响正确性（结束后一定按远端列表复核）。
- **未在真机验证**：`cp -r 本地目录/ OSS目录/` 的目录语义。已加保护：每个文件夹传完立即核对远端，不符立刻报错并说明差异；整会话模式有根目录预检。

### 4.5 进度、速度、并发
- 总进度 = (已完成数据集的全部工作量 + 进行中数据集的「哈希字节 + 已传字节」) ÷ 总工作量（上传：每数据集 2×字节）；只有一个进度条。
- 速度 = 最近 5 秒滑动窗口（`progress.Meter`）；ossutil 输出 speed 时显示其反馈值（各进程之和）；ossutil 没有进度输出时显示上一文件夹平均速度。
- 数据集级并发：每个数据集独立进度上下文（`jobs.new_ctx`），`progress.prog_view` 汇总。深度校验受磁盘限制固定并发 1。

### 4.6 校验（只读）
- 快速：本地文件列表/大小 vs 远端列表/大小，并检查 `_READY`。状态：未上传 / 一致 / 不一致 / 缺_READY。
- 深度：再重算本地 sha256，对照远端 `manifest.json`（在内存里读取，不落盘）。比的是「本地现在」vs「上传时」，**不重算远端内容哈希**。

### 4.7 扫描与提速（`scan.py`）
`os.scandir` 一次遍历（Windows 上目录项自带体积，不逐文件 stat）；分片 yaml 只读文件末尾 600 字节取 `head_time/tail_time`；统计线程数按 CPU 放大。**会话签名缓存**：签名 = 会话目录 mtime : 分片目录数 : 最新分片目录 mtime，存在 `datasets.sig`；重复扫描签名没变就复用统计，只重算路径相关字段（所以改文件夹名一定重新生效，因为路径变了）。「完整」选项忽略缓存。已知限制：文件内容原地改动不改变签名，需用「完整」。
实测（Linux、4 万文件量级的模拟数据、热缓存）：改前 0.22s → 完整统计 0.09s → 重复扫描 0.004s；Windows/移动硬盘上收益应更大，但未实测。

### 4.8 校验补录与「采集时间」
- 快速/深度校验发现远端已提交（状态「一致」）但上传记录里没有该数据集 → 自动补一条（方式=`校验补录`，状态=`已存在`，revision 取自 `_READY`）。**禁止重复**：已有「成功/已存在」记录则不补；唯一索引 `ux_uploads_backfill` 保证每个数据集最多一条补录。不一致的不补。
- 上传记录以**采集时间**为准：首列是采集时间，按采集时间倒序；登记时间（`created_at`）仅作审计，显示在后面的「登记时间」列。
- 「累计采集时长」旁的「明细」按钮：按设备（含 device_SN）、按人员分组汇总（数据集数/累计采集时长/占比/体积）。数据列表在前端汇总，上传记录由后端按数据集去重汇总（`/api/uploads` 的 `by_device`/`by_operator`）。

---

### 4.10 ossutil 断网续传
- **断网自动等待**：ossutil 失败且输出像网络问题（见 `ossutil_client.NET_RE`：timeout / dial tcp / no such host / connection reset / EOF …）时，**不消耗**普通的 3 次重试，而是进入「等待网络恢复」：每隔 5→10→…→60 秒用 `ossutil ls -d` 探测，恢复后自动续传；页面阶段显示「⚠ 网络中断，等待恢复…」并计时，日志记录中断/恢复。累计等待超过 **断网自动等待（分钟，设置里的 `net_wait`，默认 30，`0`=不等待）** 才算失败。认证错误（AccessDenied 等）、本地文件错误不算网络问题，仍走 3 次快速重试。收尾阶段（远端列表 / 写 manifest.json / _READY，`OssutilOss._run`）同样会等网。
- **续传方式**：① 断点目录**持久化**在 `umi_ckpt/`（`state.CKPT_DIR`，按数据集/上传单元分目录；**数据集提交成功后自动删除**，也可随时手动删），大文件（分片上传）按 checkpoint 接着传，程序重启后仍有效；② 单元目录里有 `started` 标记（= 上次没传完）或本次已重试过 → 加 `--update`，**已传到 OSS 的文件跳过**，不再整夹重传。
- 放弃后（等待超时/其他错误）断点保留，网络恢复后在页面重新点「上传」即可续传（本地内容变了则断点失效，重新传）。
- 仅 ossutil 方式；SDK 方式行为不变（oss2 自带的重试，整文件重传）。

### 4.9 设备 / 会话目录 / 错放移动
- **设备列 = 设备文件夹名**：路径里 `umi_normal|umi_calibration` 的下一级（`...\umi_normal\1CF3\20261111\PRT_WX_room002\ISuit…` → `1CF3`，`scan.device_dir`）。正常应等于 device_SN 后 4 位；不等（文件夹名写错）时「设备」格橙色并计入「⚠ 提示」（`row_info.dev_diff`），设备SN 列仍显示真实 SN。找不到设备文件夹时（会话直接在类型目录下，或下一级是 8 位日期文件夹=没有设备这一层）回退 SN 后 4 位。上传记录/按设备汇总里的「设备」同样是文件夹名。
- **会话目录列**：显示 `ISuit-V3_YYYY-MM-DD-HH-MM-SS` 这一层，点击在本机打开该会话目录（`POST /api/open` 带 `target:"session"`）；悬浮显示本地全路径。「房间」格仍是打开 PRT 目录。**任务名**悬浮显示本地全路径（`source`；上传记录里的任务名同样）。
- **标定判定依据**（`scan.calib_reasons`，任一成立；`datasets.misplaced` 里写明原因）：① 时长 < 3 分钟（`CAL_MAX_SEC=180`；分片 yaml 读不出时间、时长为 0 时不按此条判）② 任务名含「标定」③ 采集配置自己声明（record_purpose=Start/End/Calibration 或 location_type=标定）。`umi_normal` 里满足任一条 = 「错放」；`umi_calibration` 里一条都不满足 = 「常规数据放在 umi_calibration」（只提示，不提供移动）。改判定规则后，扫描时勾选「全量」才会让未变化的会话也按新规则重新统计。
- **移动**（`mover.py`，页面「移到标定」）：目标 = 路径里最靠后的 `umi_normal`（或旧拼写 `umi_noraml`）换成 `umi_calibration`，其余层级不变，如 `F:\data\umi_noraml\1CF3\20261111\PRT_WX_room002\ISuit-V3_2026-11-11-12-53-20` → `F:\data\umi_calibration\1CF3\…同上`。流程：预览（`GET /api/move/plan`，只读）→ 勾选 → 二次确认 → `POST /api/move`。**UMI_v3 的手动移动**用 `POST /api/move/preview` + `POST /api/move` 带 `to`（calibration / normal，两个方向），不依赖「错放」判定，只允许 UMI_v3 的会话（见 §4.11）。**只做 `os.rename`**（同盘原子，不复制、不删除、不覆盖、不合并）；目标已存在、本地目录不存在、同 OSS 路径的数据集已存在 → 标「不能移动」；仅本机可用；扫描/上传/校验进行中拒绝（`state.MOVING` 互斥，移动期间扫描/上传也会被拒）；**移动前会再检查该会话自上次扫描后没有变化**（还在采集/内容变了 → 拒绝，请重新扫描），因为判定依据（时长等）是扫描时的快照。移动后只重新统计被移动的会话，内存与 `datasets` 表即时更新（`rel` 与 id 随之变成 `umi_calibration/…`），移动留下的空目录不会删除。
- 已上传的数据被移动：新 `rel` 视为未上传（需重新上传到标定路径），OSS 上旧的 `umi_normal` 路径与旧上传记录保留，需人工处理；预览里会标「已上传」提醒。

### 4.11 两种数据格式并存（ISuit-V3 → hw-upload，UMI_v3 → prt-huawei）
**会话目录名决定格式，格式决定桶**；两种格式可以同时出现（比如两个磁盘各放一种），一次扫描全部识别，列表里每行带「格式」标签。UMI_v3 是新增的一种，**不替换** ISuit-V3，两边功能互相独立。规则只在 `upload_umi_sdk.py` 定义一次（`FORMATS`、`fmt_of_name`、`session_date`、`bucket_for`、`validate_session`）。

| | ISuit-V3 | UMI_v3 |
|---|---|---|
| 会话目录名 | `ISuit-V3_YYYY-MM-DD-HH-MM-SS` | `UMI_v3_YYYYMMDD_HHMMSS` |
| 上传到的桶 | 配置里的「ISuit-V3 桶」（默认 `hw-upload`） | 配置里的「UMI_v3 桶」（默认 `prt-huawei`） |
| 采集配置 | `datacollect_conf.yaml` | `metadata/conf.yaml` |
| 必须有 | 数字编号的分片目录 `<会话>_N/` | `metadata/conf.yaml` + `head`、`left hand`、`right hand` 三个目录 |
| 本地层级 | `umi_normal\{设备}\{日期}\PRT_城市_roomNNN\会话`（两种格式相同） | 同左 |
| 采集时长/起止 | 各分片 yaml 的 `head_time`/`tail_time` | `metadata/recording.log`，备用各相机 `frames.csv` 的 `exposure_ns`（见下） |
| 标定整理 | **自动判定**：时长 < 3 分钟 / 任务名含「标定」/ 配置声明 → 列出错放，确认后移到 `umi_calibration` | **手动**：看视频里有没有二维码（见下） |

- **每个数据集带自己的桶**：`util.make_ds` 按目录名取格式、`sdk.bucket_for(fmt)` 取桶，写入 `ds['bucket']`/`raw_uri`；上传/校验时 `jobs.process_one` 把 `c['bucket']` 换成该数据集的桶，**每个桶一个 OSS 客户端**（SDK：`sdk.Oss(..., bucket)`；ossutil：`OssutilOss(c)` 用 `c['bucket']`）。所以同一次任务里可以同时选两种格式，各传各的桶；校验、导出也各用各的桶（上传记录里存的是当时真正用的 `oss_url`）。配置里两个桶可分别修改，格式为小写字母/数字/`-`/`.`（3~63 位），留空恢复默认；扫描/任务进行中不能改桶（409）。
- **页面**：数据列表有「格式」列和格式分组按钮（全部 / ISuit-V3 / UMI_v3，带数量；上传记录页签同样可按格式筛选，`GET /api/uploads?fmt=`）；数据源卡片显示「格式 → 桶」；「标定整理」按钮分格式：`ISuit 自动 (N)`、`UMI 移到标定 (选中 N)`、`UMI 移回常规`；上传确认框会列出每种格式各多少个 → 哪个桶。
- 找不到会话时，扫描提示会同时说明两种格式各自需要什么（目录名与必备文件）。残缺的会话（缺 `metadata/conf.yaml` / 缺 `head` 等）进「扫描问题」，不入列、不上传。
- **UMI_v3 的采集时间/时长**（`scan.umi_times`，依据真实数据 `UMI_v3_20261117_130624` 确定）：没有分片 yaml，按优先级取 ① `metadata/recording.log`：「INFO 录制开始」「INFO 停止录制」两行的时间（带 +0800 时区）+「操作录制时长秒=651.502299」；② 没有日志时，用各相机 `<head|left hand|right hand>/*.frames.csv` 里 `exposure_ns`（Unix 纳秒）的最早到最晚（只读文件头尾，不整个读）；③ 都没有 → 开始取目录名（北京时间）、时长 0（未知）。**`conf.yaml` 里的 `payload.task.duration_seconds`（如 360）是计划时长，不是实际时长，不用。**
- **UMI_v3 的 `metadata/conf.yaml`**（带 `!<tag:yaml.org,2002:str>` 标签，PyYAML 可直接解析；顶层有 `device_ID`(=UMI_v3 设备类型)、`device_SN`(如 prt004)、`record_operator`、`record_location`、`task_name`(项目名)、`task_title`(具体任务名)、`sub_task`(列表，可为空)）：「任务」列显示 `task_name / task_title`；`sub_task` 列表用「、」连接入库；**「设备」列只是设备文件夹名，仅作参考，与「设备SN」没有关系**：UMI_v3 不做文件夹与 SN 的比对（不报「设备≠SN」），没有设备文件夹这一层时设备留空（不拿 SN 顶替）；ISuit-V3 的规则不变（设备文件夹应为设备SN 后 4 位，不一致会提示，没有该层时取 SN 后 4 位）。
- **UMI_v3 的标定会话要看视频手动确认**：无法靠 conf/时长自动区分（要看视频里有没有二维码，标定视频通常 < 3 分钟），所以 UMI_v3 **不做自动「错放」判定**（`misplaced` 恒为空），只在类型旁给「<3分钟」提示。流程：**点会话名** → 弹出视频预览（选相机 / 倍速 / 上一个·下一个会话）→ 看到二维码 → 点「移到标定」（或勾选后点工具栏「UMI 移到标定 / UMI 移回常规」，可批量）→ 预览目标路径 → 确认。移动只是改名（`os.rename`，不复制不覆盖），选错了可「移回常规」。
- **视频预览**（`umi/videos.py`、`/api/videos`、`/api/video`）：本机文件由后端以 Range 方式流式返回，浏览器可拖动进度；只允许读取会话目录内的 `.mp4/.m4v/.mov/.webm`（realpath 校验，拒绝 `..`、绝对路径、指向目录外的符号链接），且仅限本机访问。关闭弹窗/移动前会先释放视频文件（Windows 下被占用的文件夹无法改名）。**采集视频是 H.265**：Edge / 新版 Chrome（需硬件解码，Windows 可能要装「HEVC 视频扩展」）/ Safari 能播；浏览器播不了时页面会提示，点「打开文件夹」改用本机播放器。
- 会话根下的第一级目录就是「文件夹」：UMI_v3 为 `head`、`left hand`、`right hand`、`metadata`（目录名含空格，SDK/ossutil 均已测试）。

### 4.12 多条 WAN 出口（负载均衡）下用满带宽
路由器做负载均衡一般是**按连接**把流量分到各出口（同一条 TCP 连接始终走同一个出口），而单条连接的速度有上限（受往返延迟、丢包限制）。所以要把 N 个出口都跑满，就需要**足够多的并发连接，并且远多于出口数**，分布才均匀。ossutil 的连接数 ≈ 同时运行的 ossutil 进程数（数据集并发 `ds_jobs`）× 每进程同时传的大文件数 × 每个大文件的分片并发（`--parallel`）；一个文件夹里通常只有 2~3 个大视频，估算按 3 个。
- **配置里填「网络出口数」**，点「套用建议并发」会填入建议值（公式在 `umi/nettune.py`，页面不重复定义）：1 个出口 = 默认（数据集并发 2 / 文件 16 / 分片并发 4，约 24 条连接）；每多一个出口数据集并发 +2（多开 ossutil 进程，上限 8，与顶栏下拉一致），3 个出口起文件并发 32，分片并发 6~8。配置里实时显示「当前设置约 N 条并发连接，平均每个出口 M 条」，经验值每个出口 ≥ 24 条算够用，不足会标「偏少」。接口：`GET /api/net/plan?wan=&ds_jobs=&parallel=`。
- 并发参数**原样**传给 ossutil：`--jobs=<同时文件数> --parallel=<分片并发>`（「ossutil 额外参数」里写的 `--jobs/--parallel` 会被页面配置覆盖；测试里用假 ossutil 记录命令行并断言）。
- 说明与边界：这里能做的是「提供足够多、足够分散的连接」，由路由器决定每条连接走哪个出口；如果路由器是按**源/目的 IP** 哈希（不是按连接），同一台电脑发往同一个 OSS 地址的流量只会走一个出口，需要在路由器上改成按连接/会话均衡。判断是否跑满：在路由器上看各 WAN 的实时流量，或用页面里的总上传速度对比各出口带宽之和；不满就把数据集并发（顶栏「并发」）和分片并发调高。并发太高会占满路由器连接表或触发 OSS 限流（503），建议逐步加。

## 5. 架构与数据流

```
浏览器 ──HTTP──> api.py ──> scan.py / jobs.py / db.py
                              │            │
 扫描  POST /api/scan ─> run_scan(线程) ─> datasets 表 + 内存 DATASETS
 上传  POST /api/run  ─> run_job(线程池 ds_jobs) ─> process_one ─> do_upload
                              └ uploader.upload_ordered ─┬ SDK: upload_one(hash→put_file)
                                                         └ ossutil: ossutil_ordered(AheadHasher + ossutil_unit)
                              └ commit(): 复核 → manifest.json → _READY
                              └ 写 uploads / checks / runs
 轮询  GET /api/state (0.6s，任务/扫描进行时) ─> 前端 poll() 重绘
```
共享状态只放 `umi/state.py`；可变对象一律**原地修改**（`DATASETS.clear(); DATASETS.update(...)`），不要重新赋值，否则其他模块持有的引用会失效。`DB_PATH` 是例外（命令行可改）：一律通过 `state.DB_PATH` 访问。

## 6. 数据库（SQLite，WAL）

表结构与逐字段注释见 **`umi/schema.sql`**：`settings`(配置) `roots`(目录) `datasets`(最近一次扫描，逐列) `runs`(任务运行记录) `uploads`(上传记录，含数据列表字段快照) `checks`(最近一次校验) + 视图 `v_latest_upload`。

改表结构的步骤：① 改 `schema.sql`（全部 `IF NOT EXISTS`）② `db.py` 的 `DB_VERSION` +1 ③ 在 `init_db()` 里补迁移（ALTER/数据搬运）并在 `tests/smoke.py` 的迁移用例里覆盖。
当前 v6：`datasets` 增加 sub_task/device_sn/room/prt_local/prt_dir/sig，`uploads` 增加 device_sn/sub_task/room/cap_date（均带注释）。v4/v5→v6：`datasets` 重建（下次扫描重新填），`uploads` 用 ALTER 补列并从 rel 回填 cap_date/room，其余表保留；未知旧版本整体重建。
上传记录**有意保存数据列表字段的快照**（重扫/删除本地数据后记录仍完整、可筛选汇总）。累计采集时长按 `rel` 去重。

## 7. HTTP API

| 接口 | 说明 |
|---|---|
| `GET /` `GET /static/*` | 前端 |
| `GET /api/state` | 数据集(含上传状态/校验/日期房间提示/OSS 路径)、扫描进度 `scan`、当前任务 `job`(进度+日志)、根目录、配置摘要、`notice`(中断提示)。`?light=1` 不含 `datasets`（任务/扫描进行中页面每 0.6 秒轮询用；完整状态每 5 秒或任务/扫描刚结束时才取，2 万条数据约 23MB） |
| `POST /api/scan` `{city,room,full,pad}` | 后台扫描，立即返回；扫描中/有任务时 409。`full`=忽略缓存，`pad`=OSS 房间号补零，city+room 都填=覆盖 PRT |
| `POST /api/run` `{action:upload\|verify\|verify_deep, ids[], force}` | 启动任务；有任务/扫描时 409；已全部上传时 400 |
| `GET /api/uploads?type&status&device&cap_date&operator&task&location&q&ps&page` | 上传记录（按采集时间倒序）：分页明细 + 汇总(数据集数/累计采集时长/体积，按 rel 去重) + `by_device`/`by_operator` 明细 + 下拉选项 facets。`status=ok` 表示成功+已存在；`cap_date` 是会话名日期 |
| `POST /api/open` `{id 或 rel, target?}` | 在本机打开该数据集的本地 PRT 文件夹；`target:"session"` 打开会话目录本身（仅 127.0.0.1；找不到目录返回 404） |
| `GET /api/move/plan` | 错放标定数据的移动预览：`plans[{id,name,source,target,reason,blocked}]`，只读 |
| `POST /api/move` `{ids[]}` | 把选中的错放数据 `os.rename` 到 `umi_calibration` 对应目录；返回 `moved[]`/`failed[]`；仅本机；扫描/任务中 409；未选 400 |
| `GET /api/uploads/export?<同 /api/uploads 的筛选>` | 按查询条件导出上传记录为 Excel（.xlsx，标准库生成；全部匹配记录，不分页）：上传记录 / 汇总(含查询条件) / 按设备 / 按人员 |
| `GET /api/export` | 导出官方客户端 `upload-history` JSON（浏览器下载，不落盘） |
| `POST /api/roots` `{add[],remove[]}` | 维护数据根目录 |
| `GET /api/browse?path=` | 浏览服务端目录（Windows 空路径列盘符） |
| `GET /api/videos?id=`、`GET /api/video?id=&f=` | 会话里的视频列表 / 流式返回一个视频（Range，仅本机，只限会话目录内的视频文件） |
| `POST /api/move/preview`、`POST /api/move`（`to`） | 手动移动（UMI_v3）：`{ids, to: calibration\|normal}` 预览/执行；不带 `to` 为 ISuit-V3 的自动判定移动 |
| `GET/POST /api/config`、`POST /api/config/test` | OSS/并发/方式配置；SK 只写不读（返回 `has_sk`）。含两个桶 `bucket`（ISuit-V3）/ `bucket_umi`（UMI_v3）、出口数 `wan`；GET 另含 `buckets`（格式→桶）与 `formats`；桶校验为小写字母/数字/`-`/`.`，留空=默认；任务或扫描进行中换桶返回 409；连接测试两个桶都测 |
| `GET /api/net/plan` | 多出口并发建议：`?wan=N&ds_jobs=&parallel=` → 建议的 ds_jobs/jobs/parallel 与估算连接数、当前设置的估算 |
| `POST /api/notice/ack` | 确认「上次任务被中断」提示 |
| `POST /api/shutdown` `{force}` | 退出服务；有任务时需 `force:true` |

## 8. 前端约定（web/）

- **事件**：HTML 里写 `data-act="名字"`（点击）/ `data-change="名字"`（change），处理函数登记在 `js/main.js` 的 `ACT` / `CHANGE`。不要写内联 `onclick`。
- **加一列**：数据列表改 `tables.js` 的 `DATA_COLS`，上传记录改 `REC_COLS`（表头+取值函数），不用改别处；后端需要新字段则同步 `state()`/`schema.sql`。
- **加一个过滤条件**：`index.html` 过滤条加控件 → `filters.js` 的 `readFilters`/`rows()`（数据列表，前端）→ `api.py` 的 `up_where()`（上传记录，SQL）。网格列宽在 `app.css` 的 `.filters`。
- **过滤条是「暂存—查询」**：控件里改的是暂存值，点「查询」/回车才写入 `S.F`；切换页签沿用 `S.F`。
- **防抖动规则**：控件高度固定(`--h`)；过滤条用固定列宽网格（窄屏横向滚动不换行）；切换页签不清空表格；下拉选项仅内容变化才重建（`setOptions`）；表格容器 `scrollbar-gutter:stable`。
- **样式**：颜色/圆角/高度只用 `app.css` 顶部变量；深色模式自动（`prefers-color-scheme`）。
- 轮询只在任务/扫描进行时每 0.6s 一次；空闲时不轮询（操作后会触发一次 `poll()`）。

## 9. 关闭安全（直接关 cmd 窗口）

- 入口调用 `lifecycle.install_handlers()`：SIGINT/SIGTERM/SIGBREAK、Windows 控制台事件（含关闭窗口 ✕）、`atexit` 都走 `cleanup()`：终止 ossutil 子进程 → 把未结束的 `runs` 标记为 `interrupted` → 删除临时目录。Windows 另有「关闭即杀」Job 对象兜底（进程怎么结束子进程都会被系统终止）。
- 数据安全由设计保证：远端没有 `manifest.json`/`_READY` 就等于没传完；重新上传会跳过已完成的数据集，未完成的 ossutil 上传走持久的 `--checkpoint-dir` 断点续传并用 `--update` 跳过已传文件，断网会自动等待恢复（SDK 方式整文件重传）。SQLite 用 WAL，断电/强杀不损坏。
- 下次启动：`init_db()` 把遗留的 `running` 标为 `interrupted`，页面顶部显示提示条（「知道了」后不再提示）。
- 页面右上角「退出」= 同样的清理后退出；有任务时二次确认。
- ⚠ Windows 的控制台事件/Job 对象代码**未在本环境（Linux）实测**，已用 try/except 包裹，失败不影响主流程；信号/atexit/子进程终止路径有测试。

## 10. 测试

```
python tests/smoke.py        # 全部通过 → exit 0；打印每项 ✔/✘
```
覆盖：建库与 v4→v5 迁移、**两种格式并存（§8：两个磁盘同时加载、各自的桶/标定/上传/校验、混合任务、桶隔离、ossutil 并发参数落到命令行、视频预览、真实样本）**、扫描（城市房间覆盖、日期/房间提示、防重复）、SDK 与 ossutil（逐分片/整会话）上传的顺序与单进程不
假 OSS 落盘在临时目录；假 ossutil 按 v1.7.19 的进度格式输出并真的拷贝文件。

**页面级检查**（可选）：用 Playwright 打开真实页面，截图并测量布局（切换页签前后 `.filters/.summary/#wr` 的 `getBoundingClientRect` 应完全一致）。做法：写一个脚本 patch `sdk.Oss`/`umi.uploader.put_file`（参考 tests/smoke.py 里的 FakeOss/fake_put）后 `app.run`，再用 Playwright 驱动。

## 11. 给 AI 的工作指南

**红线**
1. 本地数据**只读**：不得修改/重命名/删除原始数据。**唯一例外**：用户在页面确认的「移到标定」（`mover.py`，只做同盘 `os.rename`，见 §4.9）。
2. **不写日志文件**：日志只在内存（`JOBS[..]['log']`），页面显示；运行产生的文件只有 `umi_web.db*`、ossutil 断点目录 `umi_ckpt/`（持久，数据集提交成功后删除，见 §4.10）和临时 cfg/报告（`state.STATE_DIR`，退出即删）。
3. 校验对远端**只读**；上传的提交顺序**不可调换**：数据文件 → 复核 → `manifest.json` → `_READY`。
4. 上传逻辑复用 `upload_umi_sdk.py`（清单/哈希/提交），不要另写一套。
5. AK/SK 不下发前端、不进日志；命令行里密钥用临时 cfg 文件。
6. 模块分层：`state < util < db < lifecycle < ossutil_client < progress < scan < mover < uploader < verify < jobs < api`，下层不得导入上层（用 `python -c "import umi.api"` 验证）。
7. 共享可变状态只放 `state.py`，原地修改。

**改动流程**：读本文件对应章节 → 改代码 → `python tests/smoke.py` → 改了前端再跑页面级检查 → 更新本文件（业务规则变了必须改 §4，表结构变了改 `schema.sql` 与 §6，接口变了改 §7）→ 在 §13 加一行变更记录。

**常见改动**
- 新增配置项：`db.get_cfg` 默认值 → `api.cfg_set` 白名单 → `schema.sql` 的 settings 注释 → 前端（`dialogs.js` 的 `CFG_FIELDS`/`index.html` 或应用栏控件 + `panel.js` 的 `CFG_KEY`）。
- 改 OSS 路径规则：只改 `util.rel_of/full_uri` 与 `scan.run_scan`（路线 city/room），并同步 §4.1 与测试里的「用户示例路径」。
- 加数据集字段：`scan.inspect` → `schema.sql`(datasets+uploads) → `db.DS_COLS`/`jobs.add_upload` → 前端列。
- ossutil 行为：集中在 `uploader.ossutil_unit/ossutil_ordered`；进度正则在文件顶部 `PROG_RE/SPD_RE/DEAL_RE`。

## 12. 已知限制 / 待办

- ossutil 目录语义未真机验证（有保护，见 §4.4）。`ossutil ls` 的长格式解析按官方客户端的格式（`LastModified Size StorageClass ETag 对象`）实现，用模拟输出测过，未对真实 ossutil 输出实测；解析不出会报错并停止提交，不会误提交。
- 扫描在大盘符上依赖 3 层深度的目录查找；超大目录时「统计会话」阶段较慢（已并行 8 线程）。
- 深度校验不重算远端内容哈希；可加「按 ETag 比对」一档（小文件 ETag=MD5，分片需重算，MinIO/OSS 规则略有差异）。
- 尚未实现：操作员工作量汇总页、开/收工标定的每日闭环表（原始需求见 `agent.md` 末尾）、按数据集暂停/取消。
- 飞书确认流程：官方客户端仅在「其他(other)」类型上传时使用，本工具只处理 normal/calibration，故未实现。
- Windows 关闭安全相关代码未在本环境实测（见 §9）。

## 13. 变更记录

| 版本 | 要点 |
|---|---|
| v6.5 | 新增 UMI_v3 会话格式（官方客户端 v1.11.0 的 `prt-huawei`），与 ISuit-V3 **并存、互相独立**：目录名决定格式、格式决定桶（ISuit-V3 → hw-upload，UMI_v3 → prt-huawei，配置里可改），一次扫描同时识别，每个数据集上传/校验各用各的桶（每个桶一个 OSS 客户端）；ISuit-V3 自动判定标定，UMI_v3 点会话名看视频（Range 流式，H.265 视浏览器支持）后手动移到标定/移回常规；UMI_v3 采集时长取 recording.log（备用 frames.csv）、任务名取 `task_name / task_title`、`sub_task` 列表入库，本地层级与 ISuit-V3 一样（设备\日期\PRT\会话），UMI_v3 的「设备」列仅作参考、与设备SN 无关（不比对）；页面重新设计：格式列与分组按钮、标定整理分格式、配置分组；多出口负载均衡：「网络出口数」+「套用建议并发」+ 连接数估算，并发参数原样传给 ossutil；上传时 `record_location: Default` 给出提示；SDK 命令行加 `--bucket` / `--umi-bucket`；Excel 导出加「格式」「桶」列；无 DB 结构变更（仅注释，新增设置项 `bucket_umi`、`wan`） |
| v6.4 | 全面检查：轮询分级（轻量 `/api/state?light=1`，完整刷新每 5 秒，行提示缓存）；配置校验（并发数/net_wait/枚举，非法值 400，留空=默认）；扫描/上传/移动的「检查+登记」用 `START` 锁互斥；连接测试不再等待网络；LIKE 里的 `%` `_` 当普通字符；数据列表排序稳定（同键按 rel 定序）；保存配置失败时页面如实提示 |
| v6.3 | ossutil 断网自动等待恢复并续传（`net_wait` 设置，默认 30 分钟）；断点目录持久化到 `umi_ckpt/`、续传加 `--update` 跳过已传文件；数据列表/上传记录关键字查询覆盖会话目录名、本地全路径、设备，支持空格分隔多词、`\\` 与 `/` 通用；无 DB 结构变更 |
| v6.2 | 设备列改取设备文件夹名（≠ SN 后 4 位时橙色提示）；数据列表新增「会话目录」列（点击打开文件夹）；任务名悬浮显示本地全路径；错放判定改为 时长<3分钟 / 任务名含「标定」/ 配置声明；新增「移到标定」（预览→确认→os.rename，`mover.py`、`/api/move*`）；无 DB 结构变更 |
| v6.1 | ossutil 方式所有 OSS 操作走 ossutil（`OssutilOss`），不再需要 oss2/boto3；含排除项的数据集改 ossutil 逐文件上传；上传记录按查询条件导出 Excel（标准库 xlsx 写入器）；requirements 只保留 flask/pyyaml |
| v6 | 数据列表/上传记录新增 房间(PRT_WX_room002 格式，不规范橙色、点击打开文件夹)、sub_task、device_SN；过滤改为「采集日期」并去掉「提示」列；城市/房间覆盖改为显式且不再保存（修复改文件夹名重扫不生效）；快速校验补录上传记录(防重复)；累计采集时长明细(按设备/人员)；上传记录以采集时间为准；扫描提速(scandir+尾部读 yaml+签名缓存)；「完整/补零」扫描选项；SQLite v6 |
| v5 | 拆分为 `umi/` 包 + `web/` 静态前端；SQLite v5（datasets 逐列、runs 任务记录、WAL）；关闭安全（lifecycle）；中断提示；退出按钮；界面美化；README/测试完善 |
| v4 | 上传记录带数据列表字段与筛选/汇总；数据集并发；唯一进度条；扫描后台化；城市房间覆盖 PRT；日期不一致提示 |
| v3 | ossutil 单进程逐文件夹/整会话；AheadHasher；导出官方格式；OSS 全路径 |
| v1–v2 | 多目录扫描、SDK/ossutil 上传、校验、SQLite、进度与速度 |
