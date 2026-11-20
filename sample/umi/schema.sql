-- ============================================================================
-- UMI 数据中心 SQLite 表结构（DB_VERSION=6）
--   * 由 umi/db.py 的 init_db() 在启动时执行（全部 IF NOT EXISTS，可重复执行）
--   * 改表结构：①改这里 ②DB_VERSION 加 1 ③在 db.py 的 init_db() 里补迁移（ALTER/数据搬运）
--   * 数据库使用 WAL 模式：直接关闭 cmd 窗口 / 断电也不会损坏，下次启动自动恢复
-- ============================================================================

-- 配置表（键值对）
--   endpoint/bucket/ak/sk   OSS 连接（页面「配置」维护；sk 只写不读，接口只返回 has_sk）。bucket=ISuit-V3 格式数据的桶（默认 hw-upload），bucket_umi=UMI_v3 格式数据的桶（默认 prt-huawei）；会话目录名决定格式，格式决定桶（upload_umi_sdk.bucket_for）
--   jobs / parallel         同时上传文件数 / 单文件分片并发（SDK 与 ossutil 共用）
--   method                  上传方式 sdk | ossutil
--   order                   SDK 上传顺序 folder 逐文件夹 | pipeline 顺序流水线
--   ossutil_unit            ossutil 单位 slice 逐分片 | session 整会话
--   ds_jobs                 同时上传的数据集个数
--   ossutil_path/args       ossutil 路径（空=umi_web.py 同级的 ossutil.exe）/ 额外参数
--   net_wait                ossutil 方式断网后最长等待分钟数（默认 30；0=不等待，只做 3 次快速重试）
--   ack_run                 已确认（不再提示）的最大「中断任务」编号
CREATE TABLE IF NOT EXISTS settings(k TEXT PRIMARY KEY, v TEXT);

-- 数据根目录：页面里添加的目录/盘符，扫描时在每个根（3 层内）查找 umi_normal / umi_calibration
CREATE TABLE IF NOT EXISTS roots(path TEXT PRIMARY KEY);

-- 扫描结果：最近一次扫描整体替换。一行 = 一个 session（一次采集会话）
CREATE TABLE IF NOT EXISTS datasets(
  id TEXT,                 -- rel 的 sha1 前 12 位（前端勾选用的稳定 id）
  rel TEXT PRIMARY KEY,    -- OSS 相对路径 umi_normal|umi_calibration/PRT_城市_room房间/会话名（不含桶与 raw_data/ 前缀）
  name TEXT,               -- 会话目录名：ISuit-V3_YYYY-MM-DD-HH-MM-SS（桶 hw-upload）/ UMI_v3_YYYYMMDD_HHMMSS（桶 prt-huawei）
  type TEXT,               -- normal 常规 / calibration 标定
  source TEXT,             -- 本地会话目录
  dir_date TEXT,           -- 所在日期文件夹（归档日期，如 20261104），可能为空
  cap_date TEXT,           -- 采集日期：取自会话目录名，OSS 路径 raw_data_YYMMDD 由它得出
  device TEXT,             -- 设备编号（device_SN 后 4 位）
  device_sn TEXT,          -- 设备完整序列号 device_SN（如 01351CA8）
  task TEXT,               -- 任务名（采集配置的 task_name：ISuit-V3 为 datacollect_conf.yaml，UMI_v3 为 metadata/conf.yaml）
  sub_task TEXT,           -- 子任务（采集配置的 sub_task）
  purpose TEXT,            -- 录制用途 Normal/Start/End
  operator TEXT,           -- 采集人员（record_operator）
  location TEXT,           -- 采集地点（record_location）
  slices INTEGER,          -- 分片数（UMI_v3 没有分片，恒为 0）
  size INTEGER,            -- 体积（字节，已排除 manifest/_READY/_pipeline 等）
  duration REAL,           -- 采集时长（秒）。ISuit-V3：各分片 tail_time-head_time 之和；UMI_v3：metadata/conf.yaml 里的起止时间之差或 duration，读不到记 0
  start_time TEXT,         -- 采集开始（北京时间）。ISuit-V3：分片 head_time 最小值；UMI_v3：conf.yaml 里的开始时间，没有则取会话目录名里的时间
  end_time TEXT,           -- 采集结束（ISuit-V3：分片 tail_time 最大值；UMI_v3：conf.yaml 里的结束时间或 开始+duration，读不到为空）
  misplaced TEXT,          -- 错放原因（常规放进标定目录等），空=正常
  room TEXT,               -- OSS 路径里的房间目录名，规范格式 PRT_WX_room002（rel 的第二段）
  prt_local TEXT,          -- 本地路径里的 PRT 目录名（可能是 PRT_WX_room2 这类不规范写法），无则空
  prt_dir TEXT,            -- 本地 PRT 目录完整路径（点击「房间」打开它）；无 PRT 目录时为会话的上一级目录
  sig TEXT,                -- 会话签名（目录 mtime:分片目录数:最新分片 mtime）。重复扫描时签名没变就复用统计，不再遍历文件
  scanned_at TEXT);        -- 扫描时间

-- 任务运行记录（每次点「上传/校验」一行）：用于发现「被直接关掉 cmd 而中断」的任务
CREATE TABLE IF NOT EXISTS runs(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  action TEXT,             -- upload / verify / verify_deep
  method TEXT,             -- 上传方式标签（sdk·逐文件夹 / ossutil·整会话 …）
  ds_jobs INTEGER,         -- 数据集并发数
  total INTEGER,           -- 本次数据集个数
  ok INTEGER,              -- 完成个数
  fail INTEGER,            -- 失败个数
  status TEXT,             -- running 进行中 / done 已结束 / interrupted 被中断（进程退出时仍为 running）
  started_at TEXT,
  finished_at TEXT);

-- 上传记录（每次上传一行）。保留数据列表字段的「快照」：重新扫描或删除本地数据后，记录仍然完整、可筛选、可汇总采集时长
CREATE TABLE IF NOT EXISTS uploads(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id INTEGER,          -- 所属任务（runs.id）
  rel TEXT,                -- OSS 相对路径（同 datasets.rel），判断是否已上传、汇总时按它去重
  oss_url TEXT,            -- OSS 全路径 oss://桶/raw_data/raw_data_YYMMDD/<rel>/
  source TEXT,             -- 本地会话目录（导出官方客户端 upload-history 用）
  device TEXT, device_sn TEXT, dir_date TEXT,
  cap_date TEXT,           -- 采集日期（会话目录名里的日期，如 2026-11-08）
  task TEXT, sub_task TEXT, operator TEXT, location TEXT, purpose TEXT, type TEXT,
  room TEXT,               -- OSS 路径里的房间目录名 PRT_WX_room002
  start_time TEXT, end_time TEXT,   -- 采集开始/结束（北京时间）
  slices INTEGER,
  size INTEGER,            -- 体积（字节）
  duration REAL,           -- 采集时长（秒）
  files INTEGER,           -- 文件数
  status TEXT,             -- 成功 / 已存在(远端早已提交，未重复上传，或校验补录) / 失败
  method TEXT,             -- 上传方式标签；「校验补录」= 校验发现远端已提交但本地原无记录，补上的一条（不是真实上传）
  revision TEXT,           -- 远端清单版本号
  elapsed INTEGER,         -- 本次耗时（秒）
  avg_speed REAL,          -- 平均上传速度（字节/秒，仅「成功」）
  error TEXT,              -- 失败原因
  created_at TEXT);        -- 登记时间（北京时间；页面以「采集时间」start_time 为准显示和排序，这里仅作审计）
CREATE INDEX IF NOT EXISTS ix_uploads_rel ON uploads(rel);
CREATE INDEX IF NOT EXISTS ix_uploads_cap ON uploads(cap_date);
CREATE INDEX IF NOT EXISTS ix_uploads_start ON uploads(start_time);
-- 每个数据集最多一条「校验补录」，防止重复补录
CREATE UNIQUE INDEX IF NOT EXISTS ux_uploads_backfill ON uploads(rel) WHERE method='校验补录';

-- 远端校验结果（每个 rel 保留最近一次）
--   status 一致 / 不一致 / 缺_READY / 未上传 / 出错；ready=远端是否有 _READY；deep=是否深度校验；detail=完整结果 JSON
CREATE TABLE IF NOT EXISTS checks(rel TEXT PRIMARY KEY, status TEXT, ready INTEGER, deep INTEGER, detail TEXT, checked_at TEXT);

-- 每个 rel 的最近一条上传记录
CREATE VIEW IF NOT EXISTS v_latest_upload AS
  SELECT * FROM uploads WHERE id IN (SELECT MAX(id) FROM uploads GROUP BY rel);
