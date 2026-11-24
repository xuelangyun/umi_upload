package umi

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "github.com/ncruces/go-sqlite3/driver"
)

//go:embed schema.sql
var schemaSQL string

var (
	dbMu     sync.Mutex
	dbConn   *sql.DB
	dbOpened string
)

var uploadNewCols = []struct {
	Name string
	Type string
}{
	{"run_id", "INTEGER"},
	{"device_sn", "TEXT"},
	{"cap_date", "TEXT"},
	{"sub_task", "TEXT"},
	{"room", "TEXT"},
}

// OpenDB returns a shared WAL-enabled SQLite connection for the current DBPath.
func OpenDB() (*sql.DB, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	if dbConn != nil && dbOpened == DBPath {
		return dbConn, nil
	}
	if dbConn != nil {
		_ = dbConn.Close()
		dbConn = nil
		dbOpened = ""
	}
	if err := os.MkdirAll(filepath.Dir(DBPath), 0o755); err != nil {
		return nil, err
	}
	conn, err := sql.Open("sqlite3", DBPath)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec("PRAGMA busy_timeout=30000; PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	dbConn = conn
	dbOpened = DBPath
	return dbConn, nil
}

// CloseDB closes the current SQLite connection (useful in tests or shutdown).
func CloseDB() {
	dbMu.Lock()
	defer dbMu.Unlock()
	if dbConn != nil {
		_ = dbConn.Close()
		dbConn = nil
		dbOpened = ""
	}
}

func ensureCols(db *sql.DB, table string, cols []struct{ Name, Type string }) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
			have[name] = true
		}
	}
	rows.Close()
	if len(have) == 0 {
		return nil
	}
	for _, col := range cols {
		if !have[col.Name] {
			if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col.Name, col.Type)); err != nil {
				return err
			}
		}
	}
	return nil
}

// InitDB initializes or migrates the SQLite schema, marks orphaned running runs as interrupted, and loads cached datasets.
func InitDB() error {
	db, err := OpenDB()
	if err != nil {
		return err
	}
	var ver int
	if err := db.QueryRow("PRAGMA user_version").Scan(&ver); err != nil {
		return err
	}

	if ver != 0 && ver != 4 && ver != 5 && ver != 6 && ver != DBVersion {
		rows, qerr := db.Query("SELECT name, type FROM sqlite_master WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%'")
		if qerr == nil {
			type obj struct{ name, typ string }
			var objs []obj
			for rows.Next() {
				var o obj
				if rows.Scan(&o.name, &o.typ) == nil {
					objs = append(objs, o)
				}
			}
			rows.Close()
			for _, o := range objs {
				if o.typ == "view" || strings.HasPrefix(o.name, "v_") {
					_, _ = db.Exec(fmt.Sprintf(`DROP VIEW IF EXISTS "%s"`, o.name))
				} else {
					_, _ = db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS "%s"`, o.name))
				}
			}
		}
	}

	if ver == 4 || ver == 5 {
		_, _ = db.Exec("DROP TABLE IF EXISTS datasets")
	}

	if err := ensureCols(db, "uploads", uploadNewCols); err != nil {
		return err
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		return err
	}
	if err := ensureInitialConfigProfile(db); err != nil {
		return err
	}

	_, _ = db.Exec("UPDATE uploads SET cap_date=substr(rel,-19,10) WHERE cap_date IS NULL OR cap_date=''")
	_, _ = db.Exec("UPDATE uploads SET room=substr(rel, instr(rel,'/')+1, instr(substr(rel, instr(rel,'/')+1),'/')-1) WHERE room IS NULL OR room=''")
	_, _ = db.Exec(fmt.Sprintf("PRAGMA user_version=%d", DBVersion))
	_, _ = db.Exec("UPDATE runs SET status='interrupted', finished_at=? WHERE status='running'", NowTS())

	_ = os.Chmod(DBPath, 0o600)

	loaded := LoadDatasets()
	PL.Lock()
	Datasets = loaded
	PL.Unlock()
	return nil
}

// GetCfg returns merged settings (defaults + env fallbacks + DB settings).
func GetCfg() map[string]string {
	c := copyStringMap(DefaultSettings)
	if v := os.Getenv("OSS_ENDPOINT"); v != "" {
		c["endpoint"] = v
	}
	if v := os.Getenv("OSS_ACCESS_KEY_ID"); v != "" {
		c["ak"] = v
	}
	if v := os.Getenv("OSS_ACCESS_KEY_SECRET"); v != "" {
		c["sk"] = v
	}
	db, err := OpenDB()
	if err == nil {
		rows, qerr := db.Query("SELECT k, v FROM settings")
		if qerr == nil {
			defer rows.Close()
			for rows.Next() {
				var k, v string
				if rows.Scan(&k, &v) == nil && cfgAllowedKeys[k] {
					c[k] = v
				}
			}
		}
	}
	return c
}

func ApplyCfg() map[string]string {
	c := GetCfg()
	ApplyActiveRuntimeConfig(c, false)
	return c
}

// ApplyUploadCfg starts a new upload from the saved baseline while publishing
// the active settings atomically with the runtime limiter reset.
func ApplyUploadCfg() map[string]string {
	c := GetCfg()
	ApplyActiveRuntimeConfig(c, true)
	return c
}

func SetSetting(k, v string) error {
	db, err := OpenDB()
	if err != nil {
		return err
	}
	_, err = db.Exec("INSERT OR REPLACE INTO settings(k, v) VALUES(?, ?)", k, v)
	return err
}

func DeleteSetting(k string) error {
	db, err := OpenDB()
	if err != nil {
		return err
	}
	_, err = db.Exec("DELETE FROM settings WHERE k=?", k)
	return err
}

// SaveDatasets replaces all rows in `datasets` inside a single transaction.
func SaveDatasets(recs []*DatasetRecord) error {
	invalidateDatasetRowsCache()
	db, err := OpenDB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM datasets"); err != nil {
		_ = tx.Rollback()
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO datasets(
		id, rel, name, type, source, dir_date, cap_date, device, device_sn,
		task, sub_task, purpose, operator, location, slices, size, duration,
		start_time, end_time, misplaced, room, prt_local, prt_dir, sig, scanned_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()

	now := NowTS()
	for _, r := range recs {
		_, err := stmt.Exec(
			r.ID, r.Rel, r.Name, r.Type, r.Source, r.DirDate, r.CapDate, r.Device, r.DeviceSN,
			r.Task, r.SubTask, r.Purpose, r.Operator, r.Location, r.Slices, r.Size, r.Duration,
			r.Start, r.End, r.Misplaced, r.Room, r.PRTLocal, r.PRTDir, r.Sig, now,
		)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func LoadDatasets() map[string]*DatasetRecord {
	out := map[string]*DatasetRecord{}
	db, err := OpenDB()
	if err != nil {
		return out
	}
	rows, err := db.Query(`SELECT
		COALESCE(id,''), COALESCE(rel,''), COALESCE(name,''), COALESCE(type,'normal'),
		COALESCE(source,''), COALESCE(dir_date,''), COALESCE(cap_date,''), COALESCE(device,''),
		COALESCE(device_sn,''), COALESCE(task,''), COALESCE(sub_task,''), COALESCE(purpose,''),
		COALESCE(operator,''), COALESCE(location,''), COALESCE(slices,0), COALESCE(size,0),
		COALESCE(duration,0), COALESCE(start_time,''), COALESCE(end_time,''), COALESCE(misplaced,''),
		COALESCE(room,''), COALESCE(prt_local,''), COALESCE(prt_dir,''), COALESCE(sig,'')
		FROM datasets`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var r DatasetRecord
		err := rows.Scan(
			&r.ID, &r.Rel, &r.Name, &r.Type,
			&r.Source, &r.DirDate, &r.CapDate, &r.Device,
			&r.DeviceSN, &r.Task, &r.SubTask, &r.Purpose,
			&r.Operator, &r.Location, &r.Slices, &r.Size,
			&r.Duration, &r.Start, &r.End, &r.Misplaced,
			&r.Room, &r.PRTLocal, &r.PRTDir, &r.Sig,
		)
		if err == nil && r.ID != "" {
			cp := r
			out[cp.ID] = &cp
		}
	}
	return out
}

func StartRun(action, method string, dsJobs, total int) int64 {
	db, err := OpenDB()
	if err != nil {
		return 0
	}
	res, err := db.Exec(
		"INSERT INTO runs(action, method, ds_jobs, total, ok, fail, status, started_at) VALUES(?,?,?,?,0,0,'running',?)",
		action, method, dsJobs, total, NowTS(),
	)
	if err != nil {
		return 0
	}
	id, _ := res.LastInsertId()
	return id
}

func FinishRun(runID int64, okCount, failCount int) {
	if runID <= 0 {
		return
	}
	db, err := OpenDB()
	if err != nil {
		return
	}
	_, _ = db.Exec(
		"UPDATE runs SET ok=?, fail=?, status='done', finished_at=? WHERE id=? AND status='running'",
		okCount, failCount, NowTS(), runID,
	)
}

func InterruptedNotice() map[string]any {
	db, err := OpenDB()
	if err != nil {
		return nil
	}
	var id, dsJobs, total, okCount, failCount int64
	var action, method, status, startedAt, finishedAt string
	err = db.QueryRow(`SELECT id, COALESCE(action,''), COALESCE(method,''), COALESCE(ds_jobs,0),
		COALESCE(total,0), COALESCE(ok,0), COALESCE(fail,0), COALESCE(status,''),
		COALESCE(started_at,''), COALESCE(finished_at,'')
		FROM runs
		WHERE status='interrupted'
		  AND id > CAST(COALESCE((SELECT v FROM settings WHERE k='ack_run'),'0') AS INTEGER)
		ORDER BY id DESC LIMIT 1`).Scan(&id, &action, &method, &dsJobs, &total, &okCount, &failCount, &status, &startedAt, &finishedAt)
	if err != nil {
		return nil
	}
	var uploadedOK int64
	_ = db.QueryRow("SELECT COUNT(*) FROM uploads WHERE run_id=? AND status IN ('成功','已存在')", id).Scan(&uploadedOK)
	return map[string]any{
		"id":          id,
		"action":      action,
		"method":      method,
		"ds_jobs":     dsJobs,
		"total":       total,
		"ok":          uploadedOK,
		"fail":        failCount,
		"status":      status,
		"started_at":  startedAt,
		"finished_at": finishedAt,
	}
}
