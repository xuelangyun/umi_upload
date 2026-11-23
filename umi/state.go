package umi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const DBVersion = 7

const (
	MultipartThreshold int64 = 10 * 1024 * 1024 // >= 10 MiB uses multipart resumable upload
	PartSize           int64 = 10 * 1024 * 1024 // 10 MiB part size
)

var TZ = time.FixedZone("CST", 8*3600)

var LocalDirs = map[string][]string{
	"normal":      {"umi_normal", "umi_noraml"}, // umi_noraml kept for legacy typo compatibility
	"calibration": {"umi_calibration"},
}

var TypeSubdir = map[string]string{
	"normal":      "umi_normal",
	"calibration": "umi_calibration",
}

var SkipDirs = map[string]bool{
	"windows":                   true,
	"program files":             true,
	"program files (x86)":       true,
	"programdata":               true,
	"system volume information": true,
	"node_modules":              true,
	"$recycle.bin":              true,
}

var Markers = map[string]bool{
	"manifest.json": true,
	"_READY":        true,
	"_SUCCESS.json": true,
}

var OKStates = map[string]bool{
	"成功":  true,
	"已存在": true,
}

var SDKDefaults = map[string]string{
	"endpoint":     "http://10.20.100.10:9000",
	"storage_type": "aliyun",
	"ak":           "minioadmin",
	"sk":           "minioadmin@123",
	"bucket":       "hw-upload",
	"bucket_umi":   "prt-huawei",
}

var DefaultSettings = map[string]string{
	"storage_type": "aliyun",
	"endpoint":     SDKDefaults["endpoint"],
	"ak":           SDKDefaults["ak"],
	"sk":           SDKDefaults["sk"],
	"bucket":       SDKDefaults["bucket"],
	"bucket_umi":   SDKDefaults["bucket_umi"],
	"jobs":         "16",
	"parallel":     "4",
	"method":       "sdk",
	"order":        "folder",
	"ds_jobs":      "2",
	"wan":          "1",
	"net_wait":     "30",
	"target_mbps":  "350",
	"auto_tune":    "true",
	"max_jobs":     "48",
	"max_parallel": "12",
	"ossutil_unit": "slice",
	"ossutil_path": "",
	"ossutil_args": "",
}

// Runtime paths resolved relative to the executable directory (or overridden via flags/tests).
var (
	Here    string
	DBPath  string
	CkptDir string
)

func init() {
	exe, err := os.Executable()
	if err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		dir := filepath.Dir(exe)
		base := strings.ToLower(filepath.Base(exe))
		if strings.Contains(dir, "go-build") || strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe") {
			if wd, werr := os.Getwd(); werr == nil {
				dir = wd
			}
		}
		Here = dir
	} else if wd, werr := os.Getwd(); werr == nil {
		Here = wd
	} else {
		Here = "."
	}
	DBPath = filepath.Join(Here, "umi_web.db")
	CkptDir = filepath.Join(Here, "umi_ckpt")
}

// Global synchronization and in-memory state.
var (
	PL       sync.Mutex // Protects Datasets, Problems, Scan, Jobs, CurJob
	StartMu  sync.Mutex // Serializes starting scan / upload / verify / move
	MovingMu sync.Mutex // Held while directory move is in progress

	Datasets = map[string]*DatasetRecord{} // id -> DatasetRecord
	Problems = []string{}
	Scan     = ScanState{}
	Jobs     = map[string]*Job{}
	CurJob   *JobContext
	Moving   bool

	AppCtx    context.Context
	AppCancel context.CancelFunc
)

func init() {
	ResetAppContext()
}

func ResetAppContext() {
	AppCtx, AppCancel = context.WithCancel(context.Background())
}

// DatasetRecord represents one local session stored in memory and cached in `datasets` table.
type DatasetRecord struct {
	ID        string  `json:"id"`
	Rel       string  `json:"rel"`
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Source    string  `json:"source"`
	DirDate   string  `json:"dir_date"`
	CapDate   string  `json:"cap_date"`
	Device    string  `json:"device"`
	DeviceSN  string  `json:"device_sn"`
	Task      string  `json:"task"`
	SubTask   string  `json:"sub_task"`
	Purpose   string  `json:"purpose"`
	Operator  string  `json:"operator"`
	Location  string  `json:"location"`
	Slices    int     `json:"slices"`
	Size      int64   `json:"size"`
	Duration  float64 `json:"duration"`
	Start     string  `json:"start"`
	End       string  `json:"end"`
	Misplaced string  `json:"misplaced"`
	Room      string  `json:"room"`
	PRTLocal  string  `json:"prt_local"`
	PRTDir    string  `json:"prt_dir"`
	Sig       string  `json:"sig"`
	Cached    bool    `json:"-"`
}

func (r *DatasetRecord) Clone() *DatasetRecord {
	if r == nil {
		return nil
	}
	cp := *r
	return &cp
}

type ScanResult struct {
	Count      int    `json:"count,omitempty"`
	Problems   int    `json:"problems,omitempty"`
	Cached     int    `json:"cached,omitempty"`
	Scanned    int    `json:"scanned,omitempty"`
	Overridden int    `json:"overridden,omitempty"`
	Error      string `json:"error,omitempty"`
}

type ScanState struct {
	ID      int         `json:"id"`
	Running bool        `json:"running"`
	Stage   string      `json:"stage"`
	Done    int         `json:"done"`
	Total   int         `json:"total"`
	T0      float64     `json:"t0"`
	T1      float64     `json:"t1"`
	Result  *ScanResult `json:"result"`
}

func AnyJobRunningLocked() bool {
	for _, j := range Jobs {
		if !j.Done {
			return true
		}
	}
	return false
}

func JobBusy() bool {
	PL.Lock()
	defer PL.Unlock()
	return AnyJobRunningLocked()
}

func ScanBusy() bool {
	PL.Lock()
	defer PL.Unlock()
	return Scan.Running
}

func IsMoving() bool {
	PL.Lock()
	defer PL.Unlock()
	return Moving
}

// ActiveCfg holds the applied OSS configuration snapshot.
var (
	cfgMu     sync.RWMutex
	activeCfg = copyStringMap(DefaultSettings)
)

func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func ActiveCfg() map[string]string {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return copyStringMap(activeCfg)
}

func setActiveCfg(c map[string]string) {
	cfgMu.Lock()
	activeCfg = copyStringMap(c)
	cfgMu.Unlock()
}
