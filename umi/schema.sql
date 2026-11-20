-- ============================================================================
-- UMI 数据中心 SQLite 表结构（DB_VERSION=7）
--   * 与原版 umi_web.db 100% 兼容，可直接无缝复用已有数据库
--   * 数据库使用 WAL 模式：直接关闭 cmd 窗口 / 断电也不会损坏，下次启动自动恢复
-- ============================================================================

CREATE TABLE IF NOT EXISTS settings(k TEXT PRIMARY KEY, v TEXT);

-- Named connection profiles. The active config is mirrored into settings for runtime compatibility.
CREATE TABLE IF NOT EXISTS config_profiles(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  config_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS roots(path TEXT PRIMARY KEY);

CREATE TABLE IF NOT EXISTS datasets(
  id TEXT,
  rel TEXT PRIMARY KEY,
  name TEXT,
  type TEXT,
  source TEXT,
  dir_date TEXT,
  cap_date TEXT,
  device TEXT,
  device_sn TEXT,
  task TEXT,
  sub_task TEXT,
  purpose TEXT,
  operator TEXT,
  location TEXT,
  slices INTEGER,
  size INTEGER,
  duration REAL,
  start_time TEXT,
  end_time TEXT,
  misplaced TEXT,
  room TEXT,
  prt_local TEXT,
  prt_dir TEXT,
  sig TEXT,
  scanned_at TEXT);

CREATE TABLE IF NOT EXISTS runs(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  action TEXT,
  method TEXT,
  ds_jobs INTEGER,
  total INTEGER,
  ok INTEGER,
  fail INTEGER,
  status TEXT,
  started_at TEXT,
  finished_at TEXT);

CREATE TABLE IF NOT EXISTS uploads(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id INTEGER,
  rel TEXT,
  oss_url TEXT,
  source TEXT,
  device TEXT, device_sn TEXT, dir_date TEXT,
  cap_date TEXT,
  task TEXT, sub_task TEXT, operator TEXT, location TEXT, purpose TEXT, type TEXT,
  room TEXT,
  start_time TEXT, end_time TEXT,
  slices INTEGER,
  size INTEGER,
  duration REAL,
  files INTEGER,
  status TEXT,
  method TEXT,
  revision TEXT,
  elapsed INTEGER,
  avg_speed REAL,
  error TEXT,
  created_at TEXT);
CREATE INDEX IF NOT EXISTS ix_uploads_rel ON uploads(rel);
CREATE INDEX IF NOT EXISTS ix_uploads_cap ON uploads(cap_date);
CREATE INDEX IF NOT EXISTS ix_uploads_start ON uploads(start_time);
CREATE UNIQUE INDEX IF NOT EXISTS ux_uploads_backfill ON uploads(rel) WHERE method='校验补录';

CREATE TABLE IF NOT EXISTS checks(rel TEXT PRIMARY KEY, status TEXT, ready INTEGER, deep INTEGER, detail TEXT, checked_at TEXT);

CREATE VIEW IF NOT EXISTS v_latest_upload AS
  SELECT * FROM uploads WHERE id IN (SELECT MAX(id) FROM uploads GROUP BY rel);
