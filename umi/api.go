package umi

import (
	"bytes"
	"database/sql"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OpenFolderFunc opens a local directory in the OS file manager; swappable in tests.
var OpenFolderFunc = func(p string) error {
	return OpenInFileManager(p, true)
}

var (
	reBucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	reOSSBucket  = regexp.MustCompile(`^oss://([^/]+)/`)
)

var cfgNumBounds = map[string][2]int{
	"jobs":     {1, 256},
	"parallel": {1, 256},
	"ds_jobs":  {1, 32},
	"wan":      {1, 16},
	"net_wait": {0, 10080},
	"target_mbps": {0, 100000},
	"max_jobs": {1, 512},
	"max_parallel": {1, 64},
}

var cfgEnums = map[string][]string{
	"storage_type": {"aliyun", "s3"},
	"method":       {"sdk", "ossutil"},
	"order":        {"folder", "slice", "pipeline"},
	"ossutil_unit": {"slice", "session"},
	"auto_tune": {"true", "false"},
}

var cfgAllowedKeys = map[string]bool{
	"storage_type": true,
	"endpoint":     true,
	"bucket":       true,
	"bucket_umi":   true,
	"ak":           true,
	"sk":           true,
	"jobs":         true,
	"parallel":     true,
	"method":       true,
	"order":        true,
	"ds_jobs":      true,
	"wan":          true,
	"ossutil_unit": true,
	"ossutil_path": true,
	"ossutil_args": true,
	"net_wait":     true,
	"target_mbps":  true,
	"auto_tune":    true,
	"max_jobs":     true,
	"max_parallel": true,
}

func cfgCheck(k, v string) string {
	if v == "" {
		return ""
	}
	if (k == "bucket" || k == "bucket_umi") && !reBucketName.MatchString(v) {
		return "Bucket 只能含小写字母、数字、- 和 .（3~63 位），例如 hw-upload / prt-huawei"
	}
	if allowed, ok := cfgEnums[k]; ok {
		found := false
		for _, a := range allowed {
			if v == a {
				found = true
				break
			}
		}
		if !found {
			return fmt.Sprintf("%s 只能是 %s", k, strings.Join(allowed, " / "))
		}
	}
	if bounds, ok := cfgNumBounds[k]; ok {
		lo, hi := float64(bounds[0]), float64(bounds[1])
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return fmt.Sprintf("%s 必须是数字", k)
		}
		if n < lo || n > hi || (k != "net_wait" && k != "target_mbps" && n != math.Trunc(n)) {
			kind := "整数"
			if k == "net_wait" {
				kind = "数"
			}
			return fmt.Sprintf("%s 必须是 %d ~ %d 的%s", k, bounds[0], bounds[1], kind)
		}
	}
	return ""
}

func BucketsMap(cfg map[string]string) map[string]string {
	if cfg == nil {
		cfg = ActiveCfg()
	}
	return map[string]string{
		"isuit":  BucketFor("isuit", cfg),
		"umi_v3": BucketFor("umi_v3", cfg),
	}
}

func FormatsLabelMap() map[string]string {
	out := make(map[string]string, len(Formats))
	for k, v := range Formats {
		out[k] = v.Label
	}
	return out
}

func isLocalRequest(r *http.Request) bool {
	addr := strings.TrimSpace(r.RemoteAddr)
	if addr == "" {
		return true
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
}

// Row warnings cache keyed by pointer identity (invalidated automatically when scan/move creates a new *DatasetRecord).
var (
	infoCacheMu sync.Mutex
	infoCache   = map[string]struct {
		ptr  *DatasetRecord
		info RowWarnings
	}{}
)

func infoOf(r *DatasetRecord) RowWarnings {
	infoCacheMu.Lock()
	defer infoCacheMu.Unlock()
	if hit, ok := infoCache[r.ID]; ok && hit.ptr == r {
		return hit.info
	}
	if len(infoCache) > 2*len(Datasets)+100 {
		infoCache = map[string]struct {
			ptr  *DatasetRecord
			info RowWarnings
		}{}
	}
	inf := RowInfo(r)
	infoCache[r.ID] = struct {
		ptr  *DatasetRecord
		info RowWarnings
	}{ptr: r, info: inf}
	return inf
}

type datasetRowsCacheState struct {
	mu           sync.Mutex
	version      uint64
	builtVersion uint64
	configKey    string
	rows         []map[string]any
	body         []byte
	etag         string
}

var datasetRowsCache = datasetRowsCacheState{version: 1}

// invalidateDatasetRowsCache marks the materialized dataset list stale after
// scans, uploads, checks, or moves. Bucket changes are detected by configKey.
func invalidateDatasetRowsCache() {
	datasetRowsCache.mu.Lock()
	datasetRowsCache.version++
	datasetRowsCache.rows = nil
	datasetRowsCache.body = nil
	datasetRowsCache.etag = ""
	datasetRowsCache.mu.Unlock()
}

func datasetRowsConfigKey(cfg map[string]string) string {
	return DBPath + "\x00" + BucketFor("isuit", cfg) + "\x00" + BucketFor("umi_v3", cfg)
}

// cachedDatasetRows serializes SQLite summaries, warning calculations, sorting,
// and JSON encoding once per data version. Callers treat rows/body as immutable.
func cachedDatasetRows(cfg map[string]string) ([]map[string]any, []byte, string, uint64, error) {
	key := datasetRowsConfigKey(cfg)
	datasetRowsCache.mu.Lock()
	defer datasetRowsCache.mu.Unlock()

	if datasetRowsCache.rows != nil &&
		datasetRowsCache.builtVersion == datasetRowsCache.version &&
		datasetRowsCache.configKey == key {
		return datasetRowsCache.rows, datasetRowsCache.body, datasetRowsCache.etag, datasetRowsCache.builtVersion, nil
	}

	rows := buildDatasetRows(cfg)
	body, err := jsonMarshalNoEscape(rows, "")
	if err != nil {
		return nil, nil, "", 0, err
	}
	sum := sha256.Sum256(body)
	etag := fmt.Sprintf("\"%x\"", sum[:])

	datasetRowsCache.rows = rows
	datasetRowsCache.body = body
	datasetRowsCache.etag = etag
	datasetRowsCache.configKey = key
	datasetRowsCache.builtVersion = datasetRowsCache.version
	return rows, body, etag, datasetRowsCache.builtVersion, nil
}

func datasetRowsVersion() uint64 {
	datasetRowsCache.mu.Lock()
	version := datasetRowsCache.version
	datasetRowsCache.mu.Unlock()
	return version
}
func DatasetRows() []map[string]any {
	rows, _, _, _, err := cachedDatasetRows(ActiveCfg())
	if err != nil {
		return []map[string]any{}
	}
	return rows
}

func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}
func buildDatasetRows(cfg map[string]string) []map[string]any {
	db, _ := OpenDB()

	type upSummary struct {
		status    string
		createdAt string
	}
	upMap := map[string]upSummary{}
	if db != nil {
		rows, err := db.Query("SELECT rel, COALESCE(status,''), COALESCE(created_at,'') FROM uploads WHERE id IN (SELECT MAX(id) FROM uploads GROUP BY rel)")
		if err == nil {
			for rows.Next() {
				var rel, st, ca string
				if rows.Scan(&rel, &st, &ca) == nil {
					upMap[rel] = upSummary{status: st, createdAt: ca}
				}
			}
			rows.Close()
		}
	}

	type ckSummary struct {
		ready     int
		detail    string
		checkedAt string
	}
	ckMap := map[string]ckSummary{}
	if db != nil {
		rows, err := db.Query("SELECT rel, COALESCE(ready,0), COALESCE(detail,''), COALESCE(checked_at,'') FROM checks")
		if err == nil {
			for rows.Next() {
				var rel, det, ca string
				var rdy int
				if rows.Scan(&rel, &rdy, &det, &ca) == nil {
					ckMap[rel] = ckSummary{ready: rdy, detail: det, checkedAt: ca}
				}
			}
			rows.Close()
		}
	}

	PL.Lock()
	all := make([]*DatasetRecord, 0, len(Datasets))
	for _, r := range Datasets {
		all = append(all, r)
	}
	PL.Unlock()

	sort.Slice(all, func(i, j int) bool {
		return all[i].Rel < all[j].Rel
	})

	rowsOut := make([]map[string]any, 0, len(all))
	for _, r := range all {
		u, hasU := upMap[r.Rel]
		k, hasK := ckMap[r.Rel]
		var rm map[string]any
		if hasK && k.detail != "" {
			_ = json.Unmarshal([]byte(k.detail), &rm)
		}

		st := "未上传"
		at := ""
		if hasU && OKStates[u.status] {
			st = "已上传"
			at = u.createdAt
		} else if hasK && k.ready != 0 {
			st = "已上传"
			at = k.checkedAt
		} else if hasU {
			st = "上传失败"
			at = u.createdAt
		}
		if st == "已上传" && rm != nil && strOr(rm["status"], "") == "未上传" {
			st = "远端缺失"
		}

		ossURL, err := FullURI(r.Rel, cfg)
		if err != nil {
			ossURL = ""
		}
		fm := FmtOfName(r.Name)
		if fm == "" {
			fm = "isuit"
		}
		inf := infoOf(r)

		var remoteVal any
		if rm != nil {
			remoteVal = rm
		}

		rowsOut = append(rowsOut, map[string]any{
			"id":        r.ID,
			"rel":       r.Rel,
			"name":      r.Name,
			"type":      r.Type,
			"source":    r.Source,
			"dir_date":  r.DirDate,
			"cap_date":  r.CapDate,
			"device":    r.Device,
			"device_sn": r.DeviceSN,
			"task":      r.Task,
			"sub_task":  r.SubTask,
			"purpose":   r.Purpose,
			"operator":  r.Operator,
			"location":  r.Location,
			"slices":    r.Slices,
			"size":      r.Size,
			"duration":  r.Duration,
			"start":     r.Start,
			"end":       r.End,
			"misplaced": r.Misplaced,
			"room":      r.Room,
			"prt_local": r.PRTLocal,
			"prt_dir":   r.PRTDir,
			"sig":       r.Sig,
			"up_state":  st,
			"up_at":     at,
			"remote":    remoteVal,
			"oss_url":   ossURL,
			"fmt":       fm,
			"bucket":    BucketFor(fm, cfg),
			"date_warn": inf.DateWarn,
			"room_bad":  inf.RoomBad,
			"room_diff": inf.RoomDiff,
			"prt_warn":  inf.PRTWarn,
			"dev_diff":  inf.DevDiff,
		})
	}
	return rowsOut
}

func likePattern(t string) string {
	esc := strings.ReplaceAll(t, "!", "!!")
	esc = strings.ReplaceAll(esc, "%", "!%")
	esc = strings.ReplaceAll(esc, "_", "!_")
	return "%" + esc + "%"
}

func upWhere(q url.Values) (string, []any) {
	var w []string
	var p []any
	for _, k := range []string{"type", "device", "cap_date", "operator"} {
		if v := q.Get(k); v != "" {
			w = append(w, k+"=?")
			p = append(p, v)
		}
	}
	switch q.Get("fmt") {
	case "umi_v3":
		w = append(w, "rel LIKE '%/UMI!_v3!_%' ESCAPE '!'")
	case "isuit":
		w = append(w, "rel NOT LIKE '%/UMI!_v3!_%' ESCAPE '!'")
	}
	for _, k := range []string{"task", "location"} {
		if v := q.Get(k); v != "" {
			w = append(w, k+" LIKE ? ESCAPE '!'")
			p = append(p, likePattern(v))
		}
	}
	if kw := q.Get("q"); kw != "" {
		cols := []string{"REPLACE(source,'\\','/')", "rel", "task", "sub_task", "operator", "location", "device", "device_sn"}
		for _, tok := range strings.Fields(strings.ReplaceAll(kw, "\\", "/")) {
			var orParts []string
			for _, col := range cols {
				orParts = append(orParts, col+" LIKE ? ESCAPE '!'")
				p = append(p, likePattern(tok))
			}
			w = append(w, "("+strings.Join(orParts, " OR ")+")")
		}
	}
	st := q.Get("status")
	if st == "ok" {
		w = append(w, "status IN ('成功','已存在')")
	} else if st != "" {
		w = append(w, "status=?")
		p = append(p, st)
	}
	if len(w) == 0 {
		return "", p
	}
	return " WHERE " + strings.Join(w, " AND "), p
}

func recFmtAndBucket(rel, ossURL string) (string, string) {
	parts := strings.Split(rel, "/")
	name := parts[len(parts)-1]
	fm := FmtOfName(name)
	if fm == "" {
		fm = "isuit"
	}
	bkt := ""
	if m := reOSSBucket.FindStringSubmatch(ossURL); m != nil {
		bkt = m[1]
	}
	return fm, bkt
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUploadRow(rs rowScanner) map[string]any {
	var (
		id, runID, slices, size, files, elapsed                      int64
		duration, avgSpeed                                           float64
		rel, ossURL, source, device, deviceSN, dirDate, capDate      string
		task, subTask, operator, location, purpose, dsType, room     string
		startTime, endTime, status, method, revision, errStr, create string
	)
	err := rs.Scan(
		&id, &runID, &rel, &ossURL, &source,
		&device, &deviceSN, &dirDate, &capDate,
		&task, &subTask, &operator, &location,
		&purpose, &dsType, &room,
		&startTime, &endTime,
		&slices, &size, &duration, &files,
		&status, &method, &revision, &elapsed, &avgSpeed, &errStr, &create,
	)
	if err != nil {
		return nil
	}
	var runVal any
	if runID > 0 {
		runVal = runID
	}
	fm, bkt := recFmtAndBucket(rel, ossURL)
	return map[string]any{
		"id":         id,
		"run_id":     runVal,
		"rel":        rel,
		"oss_url":    ossURL,
		"source":     source,
		"device":     device,
		"device_sn":  deviceSN,
		"dir_date":   dirDate,
		"cap_date":   capDate,
		"task":       task,
		"sub_task":   subTask,
		"operator":   operator,
		"location":   location,
		"purpose":    purpose,
		"type":       dsType,
		"room":       room,
		"start_time": startTime,
		"end_time":   endTime,
		"slices":     slices,
		"size":       size,
		"duration":   duration,
		"files":      files,
		"status":     status,
		"method":     method,
		"revision":   revision,
		"elapsed":    elapsed,
		"avg_speed":  avgSpeed,
		"error":      errStr,
		"created_at": create,
		"fmt":        fm,
		"bucket":     bkt,
	}
}

const uploadSelectCols = `id, COALESCE(run_id,0), COALESCE(rel,''), COALESCE(oss_url,''), COALESCE(source,''),
	COALESCE(device,''), COALESCE(device_sn,''), COALESCE(dir_date,''), COALESCE(cap_date,''),
	COALESCE(task,''), COALESCE(sub_task,''), COALESCE(operator,''), COALESCE(location,''),
	COALESCE(purpose,''), COALESCE(type,''), COALESCE(room,''),
	COALESCE(start_time,''), COALESCE(end_time,''),
	COALESCE(slices,0), COALESCE(size,0), COALESCE(duration,0), COALESCE(files,0),
	COALESCE(status,''), COALESCE(method,''), COALESCE(revision,''), COALESCE(elapsed,0), COALESCE(avg_speed,0), COALESCE(error,''), COALESCE(created_at,'')`

func uploadsGroups(whereClause string, args []any, col string) []map[string]any {
	out := []map[string]any{}
	if col != "device" && col != "operator" {
		return out
	}
	db, err := OpenDB()
	if err != nil {
		return out
	}
	q := fmt.Sprintf(`SELECT COALESCE(%s,'') AS k, COALESCE(MAX(device_sn),'') AS sn, COUNT(*) AS n, COALESCE(SUM(duration),0) AS duration, COALESCE(SUM(size),0) AS size
		FROM (SELECT rel, MAX(device) device, MAX(device_sn) device_sn, MAX(operator) operator, MAX(duration) duration, MAX(size) size FROM uploads%s GROUP BY rel)
		GROUP BY %s ORDER BY duration DESC`, col, whereClause, col)
	rows, err := db.Query(q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, sn string
		var n, sz int64
		var dur float64
		if rows.Scan(&k, &sn, &n, &dur, &sz) == nil {
			out = append(out, map[string]any{
				"k":        k,
				"sn":       sn,
				"n":        n,
				"duration": dur,
				"size":     sz,
			})
		}
	}
	return out
}

func formatHMS(sec float64) string {
	s := int64(math.Round(sec))
	if s < 0 {
		s = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, (s/60)%60, s%60)
}

var filterLabels = []struct{ Key, Label string }{
	{"fmt", "格式"},
	{"type", "类型"},
	{"status", "状态"},
	{"device", "设备"},
	{"cap_date", "采集日期"},
	{"operator", "人员"},
	{"task", "任务名"},
	{"location", "地点"},
	{"q", "关键字"},
}

func BuildUploadsExportXLSX(q url.Values) ([]byte, error) {
	w, p := upWhere(q)
	db, err := OpenDB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query("SELECT "+uploadSelectCols+" FROM uploads"+w+" ORDER BY start_time DESC, id DESC LIMIT 200000", p...)
	if err != nil {
		return nil, err
	}
	var recs []map[string]any
	for rows.Next() {
		if m := scanUploadRow(rows); m != nil {
			recs = append(recs, m)
		}
	}
	rows.Close()

	colHeaders := []any{
		"采集时间", "状态", "类型", "格式", "桶", "设备", "设备SN", "房间", "目录日期",
		"采集日期", "任务名", "子任务", "操作员", "地点", "采集结束", "分片",
		"采集时长", "采集时长(秒)", "大小(MB)", "文件数", "方式", "耗时(s)",
		"平均速度(MB/s)", "登记时间", "OSS 相对路径", "OSS 全路径",
	}
	colWidths := []int{19, 8, 6, 9, 12, 7, 11, 16, 10, 11, 26, 20, 14, 18, 19, 6, 10, 12, 10, 7, 14, 8, 14, 19, 60, 90}

	detail := make([][]any, 0, len(recs)+1)
	detail = append(detail, colHeaders)
	for _, r := range recs {
		typCN := "标定"
		if strOr(r["type"], "") == "normal" {
			typCN = "常规"
		}
		fm := strOr(r["fmt"], "isuit")
		dur := toFloat(r["duration"])
		sz := toInt64(r["size"])
		spd := toFloat(r["avg_speed"])
		detail = append(detail, []any{
			strOr(r["start_time"], ""),
			strOr(r["status"], ""),
			typCN,
			Formats[fm].Label,
			strOr(r["bucket"], ""),
			strOr(r["device"], ""),
			strOr(r["device_sn"], ""),
			strOr(r["room"], ""),
			strOr(r["dir_date"], ""),
			strOr(r["cap_date"], ""),
			strOr(r["task"], ""),
			strOr(r["sub_task"], ""),
			strOr(r["operator"], ""),
			strOr(r["location"], ""),
			strOr(r["end_time"], ""),
			toInt64(r["slices"]),
			formatHMS(dur),
			math.Round(dur*100) / 100,
			math.Round(float64(sz)/1048576.0*100) / 100,
			toInt64(r["files"]),
			strOr(r["method"], ""),
			toInt64(r["elapsed"]),
			math.Round(spd/1048576.0*100) / 100,
			strOr(r["created_at"], ""),
			strOr(r["rel"], ""),
			strOr(r["oss_url"], ""),
		})
	}

	var aggCount int64
	var aggDur float64
	var aggSize int64
	_ = db.QueryRow("SELECT COUNT(*), COALESCE(SUM(duration),0), COALESCE(SUM(size),0) FROM (SELECT rel, MAX(duration) duration, MAX(size) size FROM uploads"+w+" GROUP BY rel)", p...).
		Scan(&aggCount, &aggDur, &aggSize)

	okw := " WHERE status IN ('成功','已存在')"
	if w != "" {
		okw = w + " AND status IN ('成功','已存在')"
	}
	var okCount int64
	var okDur float64
	_ = db.QueryRow("SELECT COUNT(*), COALESCE(SUM(duration),0) FROM (SELECT rel, MAX(duration) duration FROM uploads"+okw+" GROUP BY rel)", p...).
		Scan(&okCount, &okDur)

	var condParts []string
	for _, fl := range filterLabels {
		if v := q.Get(fl.Key); v != "" {
			disp := v
			if v == "normal" {
				disp = "常规"
			} else if v == "calibration" {
				disp = "标定"
			}
			condParts = append(condParts, fmt.Sprintf("%s=%s", fl.Label, disp))
		}
	}
	cond := "（无，导出全部）"
	if len(condParts) > 0 {
		cond = strings.Join(condParts, "；")
	}

	summary := [][]any{
		{"项目", "值"},
		{"导出时间", NowTS()},
		{"查询条件", cond},
		{"记录数", len(recs)},
		{"数据集数（去重）", aggCount},
		{"累计采集时长", formatHMS(aggDur)},
		{"累计采集时长(小时)", math.Round(aggDur/3600.0*100) / 100},
		{"体积(GB)", math.Round(float64(aggSize)/(1<<30)*100) / 100},
		{"其中已上传数据集数", okCount},
		{"其中已上传累计采集时长", formatHMS(okDur)},
		{"说明", "累计采集时长按数据集去重（同一数据集多次上传只算一次）；记录按采集时间倒序"},
	}

	buildGrp := func(col string, withSN bool) [][]any {
		g := uploadsGroups(w, p, col)
		firstHead := "人员"
		if col == "device" {
			firstHead = "设备"
		}
		var head []any
		head = append(head, firstHead)
		if withSN {
			head = append(head, "设备SN")
		}
		head = append(head, "数据集数", "累计采集时长", "累计采集时长(小时)", "占比", "体积(GB)")
		out := [][]any{head}
		for _, x := range g {
			d := toFloat(x["duration"])
			sz := toInt64(x["size"])
			pct := 0.0
			if aggDur > 0 {
				pct = math.Round(100.0*d/aggDur*10) / 10
			}
			var row []any
			row = append(row, strOr(x["k"], ""))
			if withSN {
				row = append(row, strOr(x["sn"], ""))
			}
			row = append(row, toInt64(x["n"]), formatHMS(d), math.Round(d/3600.0*100)/100, pct, math.Round(float64(sz)/(1<<30)*100)/100)
			out = append(out, row)
		}
		return out
	}

	return BuildXLSX([]XLSXSheet{
		{Name: "上传记录", Rows: detail, Widths: colWidths, Filter: true, Freeze: true},
		{Name: "汇总", Rows: summary, Widths: []int{24, 70}, Filter: false, Freeze: false},
		{Name: "按设备", Rows: buildGrp("device", true), Widths: []int{10, 12, 10, 14, 16, 8, 10}, Filter: true, Freeze: true},
		{Name: "按人员", Rows: buildGrp("operator", false), Widths: []int{16, 10, 14, 16, 8, 10}, Filter: true, Freeze: true},
	})
}

func buildOfficialExportJSON() ([]byte, error) {
	db, err := OpenDB()
	if err != nil {
		return []byte("[]"), nil
	}
	rows, err := db.Query("SELECT " + uploadSelectCols + " FROM uploads ORDER BY id")
	if err != nil {
		return []byte("[]"), nil
	}
	latest := map[string]map[string]any{}
	var orderRels []string
	for rows.Next() {
		if m := scanUploadRow(rows); m != nil {
			rel := strOr(m["rel"], "")
			if _, seen := latest[rel]; !seen {
				orderRels = append(orderRels, rel)
			}
			latest[rel] = m
		}
	}
	rows.Close()

	type grpKey struct {
		typ    string
		cdate  string
		prefix string
	}
	groups := map[grpKey][]map[string]any{}
	var grpOrder []grpKey

	for _, rel := range orderRels {
		r := latest[rel]
		parts := strings.Split(rel, "/")
		name := parts[len(parts)-1]
		cdate := strOr(r["cap_date"], "")
		if cdate == "" {
			fm := FmtOfName(name)
			if fm == "" {
				fm = "isuit"
			}
			cdate = SessionDate(name, fm)
		}
		ossURL := strOr(r["oss_url"], "")
		prefix := ossURL
		suffix := "/" + name + "/"
		if idx := strings.LastIndex(ossURL, suffix); idx >= 0 {
			prefix = ossURL[:idx]
		}
		gk := grpKey{typ: strOr(r["type"], ""), cdate: cdate, prefix: prefix}
		if _, exists := groups[gk]; !exists {
			grpOrder = append(grpOrder, gk)
		}
		groups[gk] = append(groups[gk], r)
	}

	genAt := fmt.Sprintf("%s.%03dZ", time.Now().UTC().Format("2006-01-02T15:04:05"), time.Now().UTC().Nanosecond()/1e6)
	out := make([]map[string]any, 0, len(grpOrder))
	for _, gk := range grpOrder {
		rs := groups[gk]
		items := make([]map[string]any, 0, len(rs))
		sub, ar, fl := 0, 0, 0
		for _, r := range rs {
			st := strOr(r["status"], "")
			if st == "失败" {
				fl++
				items = append(items, map[string]any{
					"kind":   "dataset",
					"source": strOr(r["source"], ""),
					"uri":    nil,
					"status": "failed",
					"error":  strOr(r["error"], ""),
				})
			} else {
				itemSt := "already-ready"
				if st == "成功" {
					itemSt = "submitted"
					sub++
				} else {
					ar++
				}
				items = append(items, map[string]any{
					"kind":      "dataset",
					"source":    strOr(r["source"], ""),
					"uri":       strOr(r["oss_url"], ""),
					"status":    itemSt,
					"revision":  strOr(r["revision"], ""),
					"fileCount": toInt64(r["files"]),
				})
			}
		}
		out = append(out, map[string]any{
			"generatedAt": genAt,
			"type":        gk.typ,
			"date":        gk.cdate,
			"prefix":      gk.prefix,
			"managed":     true,
			"counts": map[string]any{
				"uploaded":     sub + ar,
				"submitted":    sub,
				"alreadyReady": ar,
				"failed":       fl,
			},
			"items": items,
		})
	}
	if len(out) == 1 {
		return jsonMarshalNoEscape(out[0], "  ")
	}
	return jsonMarshalNoEscape(out, "  ")
}

// NewHandler creates the HTTP handler serving `/api/*`, `/static/*`, and `/`.
func NewHandler(webFS fs.FS) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			c := GetCfg()
			hasSK := c["sk"] != ""
			out := make(map[string]any, len(c)+4)
			for k, v := range c {
				if k == "sk" {
					continue
				}
				out[k] = v
			}
			out["method"] = "sdk"
			out["order"] = EffectiveOrder(c)
			out["has_sk"] = hasSK
			out["buckets"] = BucketsMap(c)
			out["formats"] = FormatsLabelMap()
			out["active_profile_id"] = ActiveConfigProfileID()
			writeJSON(w, http.StatusOK, out)

		case http.MethodPost:
			body, err := readBodyMap(r)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			// Serialize config writes with scan/upload/verify admission. This
			// closes the gap between the busy check and a new job starting.
			StartMu.Lock()
			defer StartMu.Unlock()
			if !activeWork.Begin() {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "程序正在退出，不能修改配置"})
				return
			}
			defer activeWork.End()
			cur := GetCfg()
			merged, validationError := mergeConfigValues(cur, body)
			if validationError != "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": validationError})
				return
			}
			remoteChanged := merged["storage_type"] != cur["storage_type"] ||
				merged["endpoint"] != cur["endpoint"] ||
				merged["bucket"] != cur["bucket"] ||
				merged["bucket_umi"] != cur["bucket_umi"]
			if remoteChanged {
				PL.Lock()
				busy := Scan.Running || AnyJobRunningLocked()
				PL.Unlock()
				if busy {
					writeJSON(w, http.StatusConflict, map[string]any{"error": "扫描/上传/校验进行中，暂时不能更换存储类型、Endpoint 或 Bucket"})
					return
				}
			}
			if err := SaveActiveConfigProfile(merged); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存配置失败: " + err.Error()})
				return
			}
			c := ApplyCfg()
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "buckets": BucketsMap(c), "active_profile_id": ActiveConfigProfileID(),
			})

		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
		}
	})

	mux.HandleFunc("/api/config/profiles", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, ListConfigProfiles())
		case http.MethodPost:
			body, err := readBodyMap(r)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			// Serialize profile activation/deletion with task admission so an
			// operation cannot start between the busy check and profile switch.
			StartMu.Lock()
			defer StartMu.Unlock()
			if !activeWork.Begin() {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "程序正在退出，不能修改配置档案"})
				return
			}
			defer activeWork.End()
			action := strOr(body["action"], "")
			id := strings.TrimSpace(strOr(body["id"], ""))
			PL.Lock()
			busy := Scan.Running || AnyJobRunningLocked()
			PL.Unlock()
			switch action {
			case "create":
				if busy {
					writeJSON(w, http.StatusConflict, map[string]any{"error": "扫描/上传/校验进行中，暂时不能另存并切换配置"})
					return
				}
				name := strings.TrimSpace(strOr(body["name"], ""))
				raw, ok := body["config"].(map[string]any)
				if !ok {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少配置内容"})
					return
				}
				cfg, validationError := mergeConfigValues(GetCfg(), raw)
				if validationError != "" {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": validationError})
					return
				}
				newID, err := CreateConfigProfile(name, cfg)
				if err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
					return
				}
				_ = ApplyCfg()
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "active_profile_id": newID, "profiles": ListConfigProfiles()["profiles"]})
			case "activate":
				if busy {
					writeJSON(w, http.StatusConflict, map[string]any{"error": "扫描/上传/校验进行中，暂时不能切换配置"})
					return
				}
				if err := ActivateConfigProfile(id); err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
					return
				}
				_ = ApplyCfg()
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "active_profile_id": id, "profiles": ListConfigProfiles()["profiles"]})
			case "delete":
				if busy && id == ActiveConfigProfileID() {
					writeJSON(w, http.StatusConflict, map[string]any{"error": "扫描/上传/校验进行中，不能删除当前生效配置"})
					return
				}
				if err := DeleteConfigProfile(id); err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
					return
				}
				_ = ApplyCfg()
				writeJSON(w, http.StatusOK, ListConfigProfiles())
			default:
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "action 只能是 create / activate / delete"})
			}
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
		}
	})

	mux.HandleFunc("/api/bandwidth", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		writeJSON(w, http.StatusOK, CurrentBandwidthTuneStatus())
	})

	mux.HandleFunc("/api/net/plan", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		c := GetCfg()
		q := r.URL.Query()
		parseIntParam := func(key, fallback string) (int, error) {
			s := strings.TrimSpace(q.Get(key))
			if s == "" {
				s = fallback
			}
			return strconv.Atoi(s)
		}
		wan, err1 := parseIntParam("wan", c["wan"])
		ds, err2 := parseIntParam("ds_jobs", c["ds_jobs"])
		par, err3 := parseIntParam("parallel", c["parallel"])
		if err1 != nil || err2 != nil || err3 != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数必须是整数"})
			return
		}
		if wan < 1 {
			wan = 1
		} else if wan > 16 {
			wan = 16
		}
		est := Estimate(ds, par, wan)
		writeJSON(w, http.StatusOK, map[string]any{
			"wan":       wan,
			"recommend": Recommend(wan),
			"current": map[string]any{
				"conns":    est.Conns,
				"per_wan":  est.PerWAN,
				"enough":   est.Enough,
				"ds_jobs":  ds,
				"parallel": par,
			},
		})
	})

	mux.HandleFunc("/api/config/test", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		// Bucket probes perform external network I/O. Track them so shutdown
		// cannot close SQLite or exit while a probe is still using app state.
		if !activeWork.Begin() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "程序正在退出，不能测试存储连接"})
			return
		}
		defer activeWork.End()
		c := ApplyCfg()
		bkts := BucketsMap(c)
		var out []string
		allOK := true
		for _, f := range FormatOrder {
			b := bkts[f]
			label := Formats[f].Label
			cli, err := NewOSSClientFunc(BuildOSSParams(c, b))
			if err != nil {
				allOK = false
				out = append(out, fmt.Sprintf("%s → %s：失败 %s", label, b, truncateStr(err.Error(), 160)))
				continue
			}
			if err := cli.TestBucket(); err != nil {
				allOK = false
				out = append(out, fmt.Sprintf("%s → %s：失败 %s", label, b, truncateStr(err.Error(), 160)))
			} else {
				out = append(out, fmt.Sprintf("%s → %s：成功", label, b))
			}
		}
		prefix := "连接成功"
		if !allOK {
			prefix = "有桶连接失败"
		}
		provider := "阿里云 OSS Go SDK v2"
		if c["storage_type"] == "s3" {
			provider = "MinIO/S3（AWS Signature V4）"
		}
		msg := fmt.Sprintf("%s（%s，%s）：%s", prefix, provider, c["endpoint"], strings.Join(out, "；"))
		writeJSON(w, http.StatusOK, map[string]any{"ok": allOK, "msg": msg})
	})

	mux.HandleFunc("/api/browse", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		p := r.URL.Query().Get("path")
		if p == "" && runtime.GOOS == "windows" {
			dirs := []map[string]string{}
			for ch := byte('A'); ch <= 'Z'; ch++ {
				drv := string(ch) + `:\`
				if st, err := os.Stat(drv); err == nil && st.IsDir() {
					dirs = append(dirs, map[string]string{"name": drv, "full": drv})
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"path":   "",
				"parent": nil,
				"hint":   []string{},
				"dirs":   dirs,
			})
			return
		}
		if p == "" {
			p = "/"
		}
		absP, err := filepath.Abs(p)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		entries, err := os.ReadDir(absP)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		sort.Slice(entries, func(i, j int) bool {
			return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
		})
		dirs := []map[string]string{}
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				full := filepath.Join(absP, e.Name())
				dirs = append(dirs, map[string]string{"name": e.Name(), "full": full})
			}
		}
		hints := []string{}
		for _, typ := range []string{"normal", "calibration"} {
			for _, n := range LocalDirs[typ] {
				if st, herr := os.Stat(filepath.Join(absP, n)); herr == nil && st.IsDir() {
					hints = append(hints, n)
				}
			}
		}
		par := filepath.Dir(absP)
		var parVal any
		if par != absP {
			parVal = par
		} else if runtime.GOOS == "windows" {
			parVal = ""
		} else {
			parVal = nil
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"path":   absP,
			"parent": parVal,
			"dirs":   dirs,
			"hint":   hints,
		})
	})

	mux.HandleFunc("/api/roots", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		body, err := readBodyMap(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		// Serialize root changes with scan admission so each scan uses one
		// consistent root snapshot. The UI already disables these changes while scanning.
		StartMu.Lock()
		defer StartMu.Unlock()
		if !activeWork.Begin() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "程序正在退出，不能修改数据根目录"})
			return
		}
		defer activeWork.End()
		PL.Lock()
		scanning := Scan.Running
		PL.Unlock()
		if scanning {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "正在扫描，暂不能修改目录"})
			return
		}
		db, err := OpenDB()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		for _, addP := range toStringSlice(body["add"]) {
			if addP != "" {
				_, _ = db.Exec("INSERT OR IGNORE INTO roots(path) VALUES(?)", addP)
			}
		}
		for _, rmP := range toStringSlice(body["remove"]) {
			if rmP != "" {
				_, _ = db.Exec("DELETE FROM roots WHERE path=?", rmP)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("/api/scan", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		body, err := readBodyMap(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		StartMu.Lock()
		roots := listRootPaths()
		if len(roots) == 0 {
			StartMu.Unlock()
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请先添加数据根目录"})
			return
		}
		PL.Lock()
		if Scan.Running {
			PL.Unlock()
			StartMu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{"error": "正在扫描，请勿重复点击"})
			return
		}
		if Moving {
			PL.Unlock()
			StartMu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{"error": "正在移动数据，请稍候"})
			return
		}
		if AnyJobRunningLocked() {
			PL.Unlock()
			StartMu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{"error": "有上传/校验任务在运行，暂时不能扫描"})
			return
		}
		if !activeWork.Begin() {
			PL.Unlock()
			StartMu.Unlock()
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "程序正在退出，不能启动新任务"})
			return
		}
		Scan = ScanState{
			ID:      Scan.ID + 1,
			Running: true,
			Stage:   "准备",
			Done:    0,
			Total:   0,
			T0:      nowSec(),
			T1:      0,
			Result:  nil,
		}
		PL.Unlock()
		StartMu.Unlock()

		opts := ScanOpts{
			City: strOr(body["city"], ""),
			Room: strOr(body["room"], ""),
			Full: truthy(body["full"]),
			Pad:  truthy(body["pad"]),
		}
		go func() {
			defer activeWork.End()
			RunScan(opts, roots)
		}()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("/api/run", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		body, err := readBodyMap(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		action := strOr(body["action"], "")
		ids := toStringSlice(body["ids"])
		force := truthy(body["force"])

		PL.Lock()
		var recs []*DatasetRecord
		for _, id := range ids {
			if rec, ok := Datasets[id]; ok {
				recs = append(recs, rec)
			}
		}
		PL.Unlock()

		if (action != "upload" && action != "verify" && action != "verify_deep") || len(recs) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数无效或未选择数据"})
			return
		}

		StartMu.Lock()
		defer StartMu.Unlock()

		PL.Lock()
		if Scan.Running {
			PL.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{"error": "正在扫描，请稍候"})
			return
		}
		if Moving {
			PL.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{"error": "正在移动数据，请稍候"})
			return
		}
		if AnyJobRunningLocked() {
			PL.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{"error": "已有任务在运行"})
			return
		}
		PL.Unlock()

		skipped := 0
		if action == "upload" && !force {
			doneRels := map[string]bool{}
			if db, derr := OpenDB(); derr == nil {
				if rows, qerr := db.Query("SELECT rel FROM uploads WHERE status IN ('成功','已存在')"); qerr == nil {
					for rows.Next() {
						var rel string
						if rows.Scan(&rel) == nil {
							doneRels[rel] = true
						}
					}
					rows.Close()
				}
				if rows, qerr := db.Query("SELECT rel FROM checks WHERE ready=1"); qerr == nil {
					for rows.Next() {
						var rel string
						if rows.Scan(&rel) == nil {
							doneRels[rel] = true
						}
					}
					rows.Close()
				}
			}
			var kept []*DatasetRecord
			for _, rec := range recs {
				if !doneRels[rec.Rel] {
					kept = append(kept, rec)
				}
			}
			skipped = len(recs) - len(kept)
			recs = kept
			if len(recs) == 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "所选数据均已上传（勾选「含已上传」可重新核对）"})
				return
			}
		}

		sort.Slice(recs, func(i, j int) bool {
			if recs[i].Start != recs[j].Start {
				return recs[i].Start < recs[j].Start
			}
			return recs[i].Rel < recs[j].Rel
		})

		deep := action == "verify_deep"
		c := GetCfg()
		units := make([]int64, len(recs))
		items := make([]JobItem, len(recs))
		var totalUnits int64
		for i, r := range recs {
			var u int64 = 1
			if action == "upload" {
				if 2*r.Size > 1 {
					u = 2 * r.Size
				}
			} else if deep {
				u = r.Size + 1
			}
			units[i] = u
			totalUnits += u
			items[i] = JobItem{Rel: r.Rel, State: "queued", Msg: ""}
		}

		dsJobsNum := 1
		if !deep {
			if n, nerr := strconv.Atoi(c["ds_jobs"]); nerr == nil && n > 1 {
				dsJobsNum = n
			}
		}
		methodStr := "SDK"
		if action == "upload" {
			methodStr = MLabel(c)
		}

		jobID := fmt.Sprintf("%.6f", nowSec())
		job := &Job{
			ID:         jobID,
			Action:     action,
			Count:      len(recs),
			Units:      units,
			TotalUnits: totalUnits,
			Items:      items,
			T0:         nowSec(),
			Log:        []string{},
			Actives:    []*JobContext{},
			M:          NewMeter(),
		}
		if skipped > 0 {
			JobLog(job, "", fmt.Sprintf("已跳过 %d 个已上传的数据集", skipped))
		}
		for _, rec := range recs {
			inf := RowInfo(rec)
			for _, m := range []string{inf.DateWarn, inf.RoomBad, inf.RoomDiff, inf.DevDiff} {
				if m != "" {
					JobLog(job, "", fmt.Sprintf("⚠ %s：%s", rec.Name, m))
				}
			}
		}
		JobLog(job, "", fmt.Sprintf("开始 %s，共 %d 个数据集，方式: %s，数据集并发: %d", action, len(recs), methodStr, dsJobsNum))
		if !activeWork.Begin() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "程序正在退出，不能启动新任务"})
			return
		}
		var runErr error
		job.RunID, runErr = StartRun(action, methodStr, dsJobsNum, len(recs))
		if runErr != nil {
			JobLog(job, "", "⚠ 无法在 SQLite 中创建任务记录；本次任务仍会继续，但不会出现在历史运行记录中："+truncateStr(runErr.Error(), 400))
		}

		PL.Lock()
		Jobs = map[string]*Job{job.ID: job}
		PL.Unlock()

		go func() {
			defer activeWork.End()
			RunJob(job, action, recs, c)
		}()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("/api/export", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		data, err := buildOfficialExportJSON()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=umi-upload-history.json")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})

	mux.HandleFunc("/api/uploads", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		q := r.URL.Query()
		whereClause, args := upWhere(q)
		allRows := q.Get("ps") == "all"
		ps := 100
		if !allRows {
			if v, err := strconv.Atoi(q.Get("ps")); err == nil {
				ps = v
			}
			if ps < 1 {
				ps = 1
			} else if ps > 1000 {
				ps = 1000
			}
		}
		pg := 0
		if v, err := strconv.Atoi(q.Get("page")); err == nil && v > 0 {
			pg = v
		}

		db, err := OpenDB()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}

		rowsOut := []map[string]any{}
		queryRows := func(rows *sql.Rows) {
			for rows.Next() {
				if m := scanUploadRow(rows); m != nil {
					rowsOut = append(rowsOut, m)
				}
			}
			rows.Close()
		}
		if allRows {
			if rows, qerr := db.Query("SELECT "+uploadSelectCols+" FROM uploads"+whereClause+" ORDER BY start_time DESC, id DESC", args...); qerr == nil {
				queryRows(rows)
			}
		} else {
			pageArgs := append(append([]any{}, args...), ps, pg*ps)
			if rows, qerr := db.Query("SELECT "+uploadSelectCols+" FROM uploads"+whereClause+" ORDER BY start_time DESC, id DESC LIMIT ? OFFSET ?", pageArgs...); qerr == nil {
				queryRows(rows)
			}
		}

		var total int64
		_ = db.QueryRow("SELECT COUNT(*) FROM uploads"+whereClause, args...).Scan(&total)

		var dsCount int64
		var totDur float64
		var totSize int64
		_ = db.QueryRow("SELECT COUNT(*), COALESCE(SUM(duration),0), COALESCE(SUM(size),0) FROM (SELECT rel, MAX(duration) duration, MAX(size) size FROM uploads"+whereClause+" GROUP BY rel)", args...).
			Scan(&dsCount, &totDur, &totSize)

		okw := " WHERE status IN ('成功','已存在')"
		if whereClause != "" {
			okw = whereClause + " AND status IN ('成功','已存在')"
		}
		var okCount int64
		var okDur float64
		var okSize int64
		_ = db.QueryRow("SELECT COUNT(*), COALESCE(SUM(duration),0), COALESCE(SUM(size),0) FROM (SELECT rel, MAX(duration) duration, MAX(size) size FROM uploads"+okw+" GROUP BY rel)", args...).
			Scan(&okCount, &okDur, &okSize)

		facets := map[string][]string{}
		for _, fk := range []string{"cap_date", "operator", "device"} {
			vals := []string{}
			if frows, ferr := db.Query(fmt.Sprintf("SELECT DISTINCT %s FROM uploads WHERE %s IS NOT NULL AND %s<>'' ORDER BY 1", fk, fk, fk)); ferr == nil {
				for frows.Next() {
					var s string
					if frows.Scan(&s) == nil {
						vals = append(vals, s)
					}
				}
				frows.Close()
			}
			facets[fk] = vals
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"rows":        rowsOut,
			"total":       total,
			"datasets":    dsCount,
			"duration":    totDur,
			"size":        totSize,
			"ok_datasets": okCount,
			"ok_duration": okDur,
			"ok_size":     okSize,
			"facets":      facets,
			"by_device":   uploadsGroups(whereClause, args, "device"),
			"by_operator": uploadsGroups(whereClause, args, "operator"),
		})
	})

	mux.HandleFunc("/api/uploads/export", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		data, err := BuildUploadsExportXLSX(r.URL.Query())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		fnCN := fmt.Sprintf("上传记录_%s.xlsx", time.Now().In(TZ).Format("20060102-150405"))
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=upload_records.xlsx; filename*=UTF-8''%s", url.PathEscape(fnCN)))
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})

	mux.HandleFunc("/api/open", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		if !isLocalRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "只能在本机使用"})
			return
		}
		body, err := readBodyMap(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		id := strOr(body["id"], "")
		rel := strOr(body["rel"], "")
		target := strOr(body["target"], "")

		PL.Lock()
		rec := Datasets[id]
		PL.Unlock()

		src := ""
		if rec != nil {
			src = rec.Source
		} else if rel != "" {
			if db, derr := OpenDB(); derr == nil {
				_ = db.QueryRow("SELECT source FROM uploads WHERE rel=? AND source IS NOT NULL AND source<>'' ORDER BY id DESC LIMIT 1", rel).Scan(&src)
			}
		}
		if src == "" {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "找不到对应的本地目录"})
			return
		}
		openP := src
		if target != "session" {
			_, openP = LocalPRT(src)
		}
		if st, serr := os.Stat(openP); serr != nil || !st.IsDir() {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("目录不存在（可能已改名，请重新扫描）：%s", openP)})
			return
		}
		if err := OpenFolderFunc(openP); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("无法打开：%v", err)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": openP})
	})

	mux.HandleFunc("/api/move/plan", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"plans": Plans(nil, "")})
	})

	mux.HandleFunc("/api/move/preview", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		body, err := readBodyMap(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		to := strOr(body["to"], "")
		if to != "normal" && to != "calibration" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "to 必须是 normal 或 calibration"})
			return
		}
		ids := toStringSlice(body["ids"])
		writeJSON(w, http.StatusOK, map[string]any{"plans": Plans(ids, to)})
	})

	mux.HandleFunc("/api/move", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		if !isLocalRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "只能在本机使用"})
			return
		}
		body, err := readBodyMap(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		ids := toStringSlice(body["ids"])
		to := strOr(body["to"], "")
		if len(ids) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未选择数据"})
			return
		}
		if to != "" && to != "normal" && to != "calibration" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "to 必须是 normal 或 calibration"})
			return
		}

		StartMu.Lock()
		PL.Lock()
		if Scan.Running {
			PL.Unlock()
			StartMu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{"error": "正在扫描，暂时不能移动"})
			return
		}
		if AnyJobRunningLocked() {
			PL.Unlock()
			StartMu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{"error": "有上传/校验任务在运行，暂时不能移动"})
			return
		}
		if Moving || !MovingMu.TryLock() {
			PL.Unlock()
			StartMu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{"error": "正在移动数据，请稍候"})
			return
		}
		if !activeWork.Begin() {
			PL.Unlock()
			MovingMu.Unlock()
			StartMu.Unlock()
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "程序正在退出，不能启动新任务"})
			return
		}
		Moving = true
		PL.Unlock()
		StartMu.Unlock()

		defer func() {
			PL.Lock()
			Moving = false
			PL.Unlock()
			MovingMu.Unlock()
			activeWork.End()
		}()

		moved, failed := MoveIDs(ids, to)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     len(failed) == 0,
			"moved":  moved,
			"failed": failed,
		})
	})

	mux.HandleFunc("/api/videos", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		if !isLocalRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "只能在本机使用"})
			return
		}
		id := r.URL.Query().Get("id")
		PL.Lock()
		rec := Datasets[id]
		PL.Unlock()
		if rec == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "数据集不存在，请重新扫描"})
			return
		}
		if st, err := os.Stat(rec.Source); err != nil || !st.IsDir() {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "本地目录不存在（可能已被移动/改名，请重新扫描）"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"name":  rec.Name,
			"type":  rec.Type,
			"files": ListVideos(rec.Source),
		})
	})

	mux.HandleFunc("/api/video", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		if !isLocalRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "只能在本机使用"})
			return
		}
		id := r.URL.Query().Get("id")
		fParam := r.URL.Query().Get("f")
		PL.Lock()
		rec := Datasets[id]
		PL.Unlock()
		vp := ""
		if rec != nil {
			vp = ResolveVideo(rec.Source, fParam)
		}
		if vp == "" {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "视频不存在"})
			return
		}
		f, err := os.Open(vp)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "视频不存在"})
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "视频不存在"})
			return
		}
		ct := "video/mp4"
		if strings.HasSuffix(strings.ToLower(vp), ".webm") {
			ct = "video/webm"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-cache, max-age=0")
		http.ServeContent(w, r, filepath.Base(vp), st.ModTime(), f)
	})

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		if r.URL.Query().Get("realtime") == "1" {
			writeJSON(w, http.StatusOK, liveHTTPStateSnapshot())
			return
		}
		c := ActiveCfg()
		PL.Lock()
		var curJ *Job
		for _, j := range Jobs {
			curJ = j
			break
		}
		probs := make([]string, len(Problems))
		copy(probs, Problems)
		PL.Unlock()

		var jobVal any
		if curJ != nil {
			PL.Lock()
			jID := curJ.ID
			jAct := curJ.Action
			jDone := curJ.Done
			logLines := curJ.Log
			if len(logLines) > 250 {
				logLines = logLines[len(logLines)-250:]
			}
			logCopy := make([]string, len(logLines))
			copy(logCopy, logLines)
			PL.Unlock()
			jobVal = map[string]any{
				"id":     jID,
				"action": jAct,
				"done":   jDone,
				"log":    logCopy,
				"prog":   ProgView(curJ),
			}
		}

		out := map[string]any{
			"problems": probs,
			"db":       DBPath,
			"notice":   InterruptedNotice(),
			"roots":    listRootPaths(),
			"method":   "sdk",
			"order":    EffectiveOrder(c),
			"scan":     ScanView(),
			"unit":     c["ossutil_unit"],
			"ds_jobs":  c["ds_jobs"],
			"buckets":  BucketsMap(c),
			"formats":  FormatsLabelMap(),
			"job":      jobVal,
		}
		if r.URL.Query().Get("light") != "1" {
			out["datasets"] = DatasetRows()
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("/api/datasets", func(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
		return
	}
	_, body, etag, version, err := cachedDatasetRows(ActiveCfg())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "生成数据集快照失败: " + err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Dataset-Version", strconv.FormatUint(version, 10))
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
})

	mux.HandleFunc("/api/events", serveEvents)

	mux.HandleFunc("/api/notice/ack", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		if !activeWork.Begin() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "程序正在退出，不能修改运行记录"})
			return
		}
		defer activeWork.End()
		n := InterruptedNotice()
		if n != nil && n["id"] != nil {
			_ = SetSetting("ack_run", fmt.Sprint(n["id"]))
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "Method Not Allowed"})
			return
		}
		body, _ := readBodyMap(r)
		// Serialize shutdown admission with scan/run/move starts. Once stopped,
		// no new long-running work can enter while Cleanup is preparing to exit.
		StartMu.Lock()
		PL.Lock()
		busy := Scan.Running || Moving || AnyJobRunningLocked()
		PL.Unlock()
		if busy && !truthy(body["force"]) {
			StartMu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   "有任务正在运行，退出会中断它（已传完的数据不受影响，下次可继续）",
				"running": true,
			})
			return
		}
		activeWork.Stop()
		StartMu.Unlock()
		ShutdownLaterFunc()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// Static files (`/static/...` and `/` -> `index.html`)
	serveStatic := func(w http.ResponseWriter, r *http.Request, relPath string) {
		clean := strings.TrimPrefix(path.Clean("/"+relPath), "/")
		if clean == "" || clean == "." {
			clean = "index.html"
		}
		data, err := fs.ReadFile(webFS, clean)
		if err != nil {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		ext := strings.ToLower(path.Ext(clean))
		ct := mime.TypeByExtension(ext)
		if ct == "" {
			switch ext {
			case ".html":
				ct = "text/html; charset=utf-8"
			case ".css":
				ct = "text/css; charset=utf-8"
			case ".js":
				ct = "application/javascript; charset=utf-8"
			default:
				ct = "application/octet-stream"
			}
		}
		sum := sha256.Sum256(data)
		etag := fmt.Sprintf("\"%x\"", sum[:])
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", etag)
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	}

	mux.HandleFunc("/static/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		serveStatic(w, r, strings.TrimPrefix(r.URL.Path, "/static/"))
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		serveStatic(w, r, "index.html")
	})

	return mux
}

func listRootPaths() []string {
	out := []string{}
	db, err := OpenDB()
	if err != nil {
		return out
	}
	rows, err := db.Query("SELECT path FROM roots ORDER BY path")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	b, err := jsonMarshalNoEscape(payload, "")
	if err != nil {
		b = []byte(`{"error":"internal marshal error"}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func readBodyMap(r *http.Request) (map[string]any, error) {
	if r.Body == nil {
		return map[string]any{}, nil
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8*1024*1024))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func strOr(v any, def string) string {
	if v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	default:
		return 0
	}
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	default:
		return 0
	}
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case int:
		return x != 0
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		return s != "" && s != "0" && s != "false" && s != "no"
	default:
		return v != nil
	}
}

func toStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if x != nil {
			out = append(out, fmt.Sprint(x))
		}
	}
	return out
}
