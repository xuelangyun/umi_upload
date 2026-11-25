package umi

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const CalMaxSec = 180.0

var (
	reHeadTime  = regexp.MustCompile(`(?m)(?:^|\s)head_time:\s*([\d.]+)`)
	reTailTime  = regexp.MustCompile(`(?m)(?:^|\s)tail_time:\s*([\d.]+)`)
	reLogTS     = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})(\.\d+)? ([+-]\d{4})\b`)
	reLogDur    = regexp.MustCompile(`操作录制时长秒=([0-9.]+)`)
	reDirDate8  = regexp.MustCompile(`^\d{8}$`)
	localDirMap = func() map[string]string {
		m := map[string]string{}
		for typ, names := range LocalDirs {
			for _, n := range names {
				m[strings.ToLower(n)] = typ
			}
		}
		return m
	}()
)

func parseShardTimes(fp string) (float64, float64, bool) {
	f, err := os.Open(fp)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, 0, false
	}
	sz := st.Size()
	offset := sz - 600
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, sz-offset)
	if _, err := f.ReadAt(buf, offset); err == nil {
		t := string(buf)
		hm := reHeadTime.FindStringSubmatch(t)
		tm := reTailTime.FindStringSubmatch(t)
		if hm != nil && tm != nil {
			h, err1 := strconv.ParseFloat(hm[1], 64)
			e, err2 := strconv.ParseFloat(tm[1], 64)
			if err1 == nil && err2 == nil {
				return h, e, true
			}
		}
	}
	full, err := ReadText(fp)
	if err != nil {
		return 0, 0, false
	}
	hm := reHeadTime.FindStringSubmatch(full)
	tm := reTailTime.FindStringSubmatch(full)
	if hm != nil && tm != nil {
		h, err1 := strconv.ParseFloat(hm[1], 64)
		e, err2 := strconv.ParseFloat(tm[1], 64)
		if err1 == nil && err2 == nil {
			return h, e, true
		}
	}
	return 0, 0, false
}

// SessionSig computes a lightweight directory signature: root_mtime_ns:subdir_count:newest_subdir_mtime_ns.
func SessionSig(src string) string {
	st, err := os.Stat(src)
	if err != nil {
		return ""
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return ""
	}
	n := 0
	var newest int64
	for _, e := range entries {
		if e.IsDir() {
			n++
			if info, ierr := e.Info(); ierr == nil {
				if m := info.ModTime().UnixNano(); m > newest {
					newest = m
				}
			}
		}
	}
	return fmt.Sprintf("%d:%d:%d", st.ModTime().UnixNano(), n, newest)
}

type SessionStats struct {
	Size     int64
	Slices   int
	Duration float64
	Start    string
	End      string
}

func Stats(src, name string, shards bool) SessionStats {
	var totalSize int64
	slices := 0
	var minHead, maxTail float64
	hasHead := false
	dur := 0.0

	type stackItem struct {
		dir   string
		depth int
	}
	stack := []stackItem{{dir: src, depth: 0}}
	shardPre := name + "_"

	for len(stack) > 0 {
		if AppCtx.Err() != nil {
			return SessionStats{}
		}
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		entries, err := os.ReadDir(cur.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if AppCtx.Err() != nil {
				return SessionStats{}
			}
			en := e.Name()
			full := filepath.Join(cur.dir, en)
			if e.IsDir() {
				if en != "_pipeline" {
					stack = append(stack, stackItem{dir: full, depth: cur.depth + 1})
				}
				continue
			}
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			if Markers[en] {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			totalSize += info.Size()
			if shards && cur.depth == 1 && strings.HasSuffix(en, ".yaml") && strings.HasPrefix(en, shardPre) {
				slices++
				if h, t, ok := parseShardTimes(full); ok {
					if !hasHead || h < minHead {
						minHead = h
					}
					if !hasHead || t > maxTail {
						maxTail = t
					}
					hasHead = true
					if t > h {
						dur += t - h
					}
				}
			}
		}
	}

	var startStr, endStr string
	if hasHead {
		startStr = FmtTS(minHead)
		endStr = FmtTS(maxTail)
	}
	return SessionStats{
		Size:     totalSize,
		Slices:   slices,
		Duration: math.Round(dur*100) / 100,
		Start:    startStr,
		End:      endStr,
	}
}

func ReadConf(src, fmtName string) map[string]any {
	spec, ok := Formats[fmtName]
	if !ok {
		return map[string]any{}
	}
	p := filepath.Join(src, filepath.FromSlash(spec.Conf))
	txt, err := ReadText(p)
	if err != nil {
		return map[string]any{}
	}
	m := ParseSimpleYAML(txt)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// Txt converts a YAML config value into string: nil -> "", list -> joined by "、", scalar -> string.
func Txt(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, item := range x {
			if item == nil {
				continue
			}
			parts = append(parts, fmt.Sprint(item))
		}
		return strings.Join(parts, "、")
	case []string:
		return strings.Join(x, "、")
	default:
		return fmt.Sprint(x)
	}
}

func parseLogEpoch(line string) (float64, bool) {
	m := reLogTS.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	frac := m[2]
	if frac == "" {
		frac = ".0"
	}
	t, err := time.Parse("2006-01-02 15:04:05.999999999 -0700", m[1]+frac+" "+m[3])
	if err != nil {
		return 0, false
	}
	return float64(t.UnixNano()) / 1e9, true
}

func UMILogTimes(src string) (start *float64, end *float64, dur *float64) {
	if AppCtx.Err() != nil {
		return nil, nil, nil
	}
	p := filepath.Join(src, "metadata", "recording.log")
	text, err := ReadText(p)
	if err != nil {
		return nil, nil, nil
	}
	for _, line := range strings.Split(text, "\n") {
		if AppCtx.Err() != nil {
			return nil, nil, nil
		}
		line = strings.TrimRight(line, "\r")
		if start == nil && strings.Contains(line, " INFO 录制开始") {
			if ep, ok := parseLogEpoch(line); ok {
				v := ep
				start = &v
			}
		} else if strings.Contains(line, " INFO 停止录制") {
			if ep, ok := parseLogEpoch(line); ok {
				v := ep
				end = &v
			}
		}
		if dm := reLogDur.FindStringSubmatch(line); dm != nil {
			if d, err := strconv.ParseFloat(dm[1], 64); err == nil && d > 0 {
				v := d
				dur = &v
			}
		}
	}
	if (dur == nil || *dur <= 0) && start != nil && end != nil && *end > *start {
		d := *end - *start
		dur = &d
	}
	if dur != nil && *dur <= 0 {
		dur = nil
	}
	return start, end, dur
}

func csvFirstLast(fp string) (head, first, last string, ok bool) {
	f, err := os.Open(fp)
	if err != nil {
		return "", "", "", false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return "", "", "", false
	}
	headBuf := make([]byte, minInt64(st.Size(), 4096))
	n, _ := f.Read(headBuf)
	lines := bytes.Split(headBuf[:n], []byte{'\n'})
	if len(lines) < 2 {
		return "", "", "", false
	}
	head = strings.TrimSpace(string(lines[0]))
	first = strings.TrimSpace(string(lines[1]))

	tailOff := st.Size() - 4096
	if tailOff < 0 {
		tailOff = 0
	}
	tailBuf := make([]byte, st.Size()-tailOff)
	if _, err := f.ReadAt(tailBuf, tailOff); err != nil && err != io.EOF {
		return "", "", "", false
	}
	tailLines := bytes.Split(tailBuf, []byte{'\n'})
	for i := len(tailLines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(string(tailLines[i]))
		if s != "" {
			last = s
			break
		}
	}
	return head, first, last, true
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func UMIFrameTimes(src string) (*float64, *float64) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil, nil
	}
	var loMin, hiMax float64
	found := false
	for _, d := range entries {
		if AppCtx.Err() != nil {
			return nil, nil
		}
		if !d.IsDir() || d.Name() == "metadata" {
			continue
		}
		subPath := filepath.Join(src, d.Name())
		files, serr := os.ReadDir(subPath)
		if serr != nil {
			continue
		}
		for _, f := range files {
			if AppCtx.Err() != nil {
				return nil, nil
			}
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".frames.csv") {
				continue
			}
			head, first, last, ok := csvFirstLast(filepath.Join(subPath, f.Name()))
			if !ok || first == "" || last == "" || last == head {
				continue
			}
			cols := strings.Split(head, ",")
			expIdx := -1
			for idx, c := range cols {
				if strings.TrimSpace(c) == "exposure_ns" {
					expIdx = idx
					break
				}
			}
			if expIdx < 0 {
				continue
			}
			fCols := strings.Split(first, ",")
			lCols := strings.Split(last, ",")
			if expIdx >= len(fCols) || expIdx >= len(lCols) {
				continue
			}
			ns1, err1 := strconv.ParseInt(strings.TrimSpace(fCols[expIdx]), 10, 64)
			ns2, err2 := strconv.ParseInt(strings.TrimSpace(lCols[expIdx]), 10, 64)
			if err1 != nil || err2 != nil {
				continue
			}
			s1 := float64(ns1) / 1e9
			s2 := float64(ns2) / 1e9
			if !found || s1 < loMin {
				loMin = s1
			}
			if !found || s2 > hiMax {
				hiMax = s2
			}
			found = true
		}
	}
	if !found {
		return nil, nil
	}
	return &loMin, &hiMax
}

func UMITimes(src, name string) (duration float64, startStr string, endStr string) {
	ls, le, ld := UMILogTimes(src)
	var startSec, endSec float64
	var dur float64
	if ld != nil && *ld > 0 {
		dur = *ld
		if ls != nil {
			startSec = *ls
		}
		if le != nil {
			endSec = *le
		} else if ls != nil {
			endSec = *ls + dur
		}
	} else {
		fs, fe := UMIFrameTimes(src)
		if fs != nil && fe != nil && *fe >= *fs {
			startSec = *fs
			endSec = *fe
			dur = *fe - *fs
		}
	}
	if startSec <= 0 {
		if m := UmiV3Re.FindStringSubmatch(name); m != nil {
			if t, err := time.ParseInLocation("20060102150405", m[1]+m[2], TZ); err == nil {
				startSec = float64(t.Unix())
			}
		}
	}
	return math.Round(dur*100) / 100, FmtTS(startSec), FmtTS(endSec)
}

func LocalPRT(src string) (string, string) {
	prefix, parts := splitPathComponents(src)
	for i := len(parts) - 2; i >= 0; i-- {
		if PRTRe.MatchString(parts[i]) {
			return parts[i], joinPathComponents(prefix, parts[:i+1], src)
		}
	}
	if len(parts) >= 1 {
		return "", joinPathComponents(prefix, parts[:len(parts)-1], src)
	}
	return "", filepath.Dir(src)
}

// DeviceDir extracts the folder immediately under umi_normal / umi_calibration unless it is an 8-digit date or PRT_* room folder.
func DeviceDir(src string) string {
	_, parts := splitPathComponents(src)
	for i := len(parts) - 2; i >= 0; i-- {
		if _, isTypeDir := localDirMap[strings.ToLower(parts[i])]; isTypeDir {
			if i+1 < len(parts)-1 {
				cand := parts[i+1]
				if reDirDate8.MatchString(cand) || PRTRe.MatchString(cand) {
					return ""
				}
				return cand
			}
			return ""
		}
	}
	return ""
}

func mmss(sec float64) string {
	s := int(math.Round(sec))
	if s < 0 {
		s = 0
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

func CalibReasons(duration float64, slices int, task string, declared string) []string {
	out := []string{}
	if duration > 0 && duration < CalMaxSec {
		out = append(out, fmt.Sprintf("时长 %s < 3 分钟", mmss(duration)))
	}
	if strings.Contains(task, "标定") {
		out = append(out, "任务名含「标定」")
	}
	if declared != "" {
		out = append(out, declared)
	}
	return out
}

func relID(rel string) string {
	sum := sha1.Sum([]byte(rel))
	return hex.EncodeToString(sum[:])[:12]
}

// Inspect builds a DatasetRecord from a discovered DatasetMeta, reusing cached stats when SessionSig matches.
func Inspect(ds *DatasetMeta, prev *DatasetRecord, pad bool) *DatasetRecord {
	if AppCtx.Err() != nil {
		return nil
	}
	src := ds.Source
	sig := SessionSig(src)
	if AppCtx.Err() != nil {
		return nil
	}
	fmtName := ds.Fmt
	if fmtName == "" {
		fmtName = FmtOfName(ds.Name)
		if fmtName == "" {
			fmtName = "isuit"
		}
	}

	var rec DatasetRecord
	if prev != nil && sig != "" && prev.Sig == sig {
		rec = *prev
		rec.Cached = true
	} else {
		conf := ReadConf(src, fmtName)
		if AppCtx.Err() != nil {
			return nil
		}
		st := Stats(src, ds.Name, fmtName == "isuit")
		if AppCtx.Err() != nil {
			return nil
		}
		if fmtName == "umi_v3" {
			st.Duration, st.Start, st.End = UMITimes(src, ds.Name)
		}

		purpose := Txt(conf["record_purpose"])
		ltype := Txt(conf["location_type"])
		declared := ""
		if purpose == "Start" || purpose == "End" || purpose == "Calibration" {
			declared = "record_purpose=" + purpose
		} else if ltype == "标定" {
			declared = "位置类型=标定"
		}

		task := Txt(conf["task_name"])
		if task == "" {
			task = Txt(conf["sub_task"])
		}
		if fmtName == "umi_v3" {
			tn := strings.TrimSpace(Txt(conf["task_name"]))
			tt := strings.TrimSpace(Txt(conf["task_title"]))
			var both []string
			if tn != "" {
				both = append(both, tn)
			}
			if tt != "" {
				both = append(both, tt)
			}
			if len(both) > 0 {
				task = strings.Join(both, " / ")
			} else {
				task = Txt(conf["sub_task"])
			}
		}

		reasons := CalibReasons(st.Duration, st.Slices, task, declared)
		misplaced := ""
		if fmtName != "umi_v3" {
			if ds.Type == "normal" && len(reasons) > 0 {
				misplaced = "标定数据放在 umi_normal：" + strings.Join(reasons, "；")
			} else if ds.Type == "calibration" && len(reasons) == 0 {
				misplaced = "常规数据放在 umi_calibration"
			}
		}

		devSN := Txt(conf["device_SN"])
		rec = DatasetRecord{
			DeviceSN:  devSN,
			Task:      task,
			SubTask:   Txt(conf["sub_task"]),
			Purpose:   purpose,
			Operator:  Txt(conf["record_operator"]),
			Location:  Txt(conf["record_location"]),
			Slices:    st.Slices,
			Size:      st.Size,
			Duration:  st.Duration,
			Start:     st.Start,
			End:       st.End,
			Misplaced: misplaced,
			Cached:    false,
		}
	}

	rel := RelOf(ds.RawURI)
	if pad {
		rel = PadRoom(rel)
	}
	prtLocal, prtDir := LocalPRT(src)
	dev := DeviceDir(src)
	if dev == "" && fmtName != "umi_v3" {
		if len(rec.DeviceSN) >= 4 {
			dev = rec.DeviceSN[len(rec.DeviceSN)-4:]
		} else {
			dev = rec.DeviceSN
		}
	}

	dirDate := ""
	_, parts := splitPathComponents(src)
	for _, p := range parts {
		if reDirDate8.MatchString(p) {
			dirDate = p
			break
		}
	}

	room := ""
	relParts := strings.Split(rel, "/")
	if len(relParts) >= 2 {
		room = relParts[1]
	}

	rec.ID = relID(rel)
	rec.Rel = rel
	rec.Name = ds.Name
	rec.Type = ds.Type
	rec.Source = src
	rec.CapDate = ds.CaptureDate
	rec.DirDate = dirDate
	rec.Device = dev
	rec.Room = room
	rec.PRTLocal = prtLocal
	rec.PRTDir = prtDir
	rec.Sig = sig

	return &rec
}

type FoundTypeDir struct {
	Type string
	Path string
}

func FindDirs(root string, maxDepth int) []FoundTypeDir {
	base := strings.ToLower(filepath.Base(root))
	if typ, ok := localDirMap[base]; ok {
		return []FoundTypeDir{{Type: typ, Path: root}}
	}
	var out []FoundTypeDir
	var walk func(p string, d int)
	walk = func(p string, d int) {
		if AppCtx.Err() != nil {
			return
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return
		}
		for _, e := range entries {
			if AppCtx.Err() != nil {
				return
			}
			if !e.IsDir() {
				continue
			}
			en := e.Name()
			low := strings.ToLower(en)
			if typ, ok := localDirMap[low]; ok {
				out = append(out, FoundTypeDir{Type: typ, Path: filepath.Join(p, en)})
			} else if d < maxDepth && !strings.HasPrefix(en, ".") && !strings.HasPrefix(en, "$") && !SkipDirs[low] {
				walk(filepath.Join(p, en), d+1)
			}
		}
	}
	walk(root, 0)
	return out
}

type RowWarnings struct {
	DateWarn string `json:"date_warn"`
	RoomBad  string `json:"room_bad"`
	RoomDiff string `json:"room_diff"`
	PRTWarn  string `json:"prt_warn"`
	DevDiff  string `json:"dev_diff"`
}

func RowInfo(r *DatasetRecord) RowWarnings {
	capDate := r.CapDate
	dw := ""
	if r.DirDate != "" && r.DirDate != strings.ReplaceAll(capDate, "-", "") {
		yymmdd, _ := ToYYMMDD(capDate)
		dw = fmt.Sprintf("目录日期 %s 与 session 目录名日期 %s 不一致（上传路径按 session 日期 raw_data_%s）", r.DirDate, capDate, yymmdd)
	}
	bad := ""
	if !RoomOKRe.MatchString(r.Room) {
		bad = fmt.Sprintf("房间 %s 不是 3 位房间号格式，应为 %s（可点击打开文件夹改名后重新扫描，或扫描时勾选「补零」）", r.Room, PadRoom(r.Room))
	}
	diff := ""
	if r.PRTLocal != "" && r.PRTLocal != r.Room {
		diff = fmt.Sprintf("OSS 路径使用 %s，本地目录是 %s", r.Room, r.PRTLocal)
	}
	dd := ""
	sn4 := r.DeviceSN
	if len(sn4) > 4 {
		sn4 = sn4[len(sn4)-4:]
	}
	if sn4 != "" && !strings.EqualFold(r.Device, sn4) && FmtOfName(r.Name) != "umi_v3" {
		dd = fmt.Sprintf("设备文件夹 %s 与设备SN %s 的后 4 位 %s 不一致（文件夹名可能写错，设备列取的是文件夹名）", r.Device, r.DeviceSN, sn4)
	}
	return RowWarnings{
		DateWarn: dw,
		RoomBad:  bad,
		RoomDiff: diff,
		PRTWarn:  diff,
		DevDiff:  dd,
	}
}

func ScanView() map[string]any {
	PL.Lock()
	sc := Scan
	PL.Unlock()

	elapsed := 0.0
	if sc.T0 > 0 {
		endT := sc.T1
		if sc.Running {
			endT = nowSec()
		}
		if endT > sc.T0 {
			elapsed = math.Round((endT-sc.T0)*10) / 10
		}
	}
	return map[string]any{
		"id":      sc.ID,
		"running": sc.Running,
		"stage":   sc.Stage,
		"done":    sc.Done,
		"total":   sc.Total,
		"t0":      sc.T0,
		"t1":      sc.T1,
		"result":  sc.Result,
		"elapsed": elapsed,
	}
}

type ScanOpts struct {
	City string
	Room string
	Full bool
	Pad  bool
}

func uniqueScanRecords(recs []*DatasetRecord) (map[string]*DatasetRecord, []*DatasetRecord, []string) {
	seen := make(map[string]*DatasetRecord, len(recs))
	ordered := make([]*DatasetRecord, 0, len(recs))
	var problems []string
	for _, rec := range recs {
		if rec == nil {
			continue
		}
		if previous, exists := seen[rec.ID]; exists {
			problems = append(problems, fmt.Sprintf("重复数据集(同一 OSS 路径): %s 与 %s", previous.Source, rec.Source))
			continue
		}
		seen[rec.ID] = rec
		ordered = append(ordered, rec)
	}
	return seen, ordered, problems
}

func RunScan(opts ScanOpts, roots []string) {
	var res *ScanResult
	defer func() {
		PL.Lock()
		Scan.Running = false
		Scan.Stage = "完成"
		Scan.T1 = nowSec()
		Scan.Result = res
		PL.Unlock()
	}()

	cfg := ApplyCfg()
	city := strings.TrimSpace(opts.City)
	room := strings.TrimSpace(opts.Room)
	hasOv := city != "" && room != ""
	var problems []string
	if (city != "" || room != "") && !hasOv {
		problems = append(problems, "城市和房间号需同时填写才会覆盖本地路径里的 PRT 目录，已忽略")
	}

	var routes []RouteSpec
	for i, r := range roots {
		if AppCtx.Err() != nil {
			return
		}
		PL.Lock()
		Scan.Stage = fmt.Sprintf("查找目录 %d/%d", i+1, len(roots))
		PL.Unlock()

		var found []FoundTypeDir
		if st, err := os.Stat(r); err == nil && st.IsDir() {
			found = FindDirs(r, 3)
		}
		if len(found) == 0 {
			problems = append(problems, fmt.Sprintf("%s 下（3 层内）没有 umi_normal / umi_calibration", r))
			continue
		}
		for _, fd := range found {
			rs := RouteSpec{Type: fd.Type, Path: fd.Path}
			if hasOv {
				rs.City = city
				rs.Room = room
			}
			routes = append(routes, rs)
		}
	}

	PL.Lock()
	Scan.Stage = "识别数据集"
	PL.Unlock()

	if AppCtx.Err() != nil {
		return
	}
	var metas []*DatasetMeta
	var discProbs []string
	if len(routes) > 0 {
		metas, discProbs = Discover(routes, cfg)
	}
	if AppCtx.Err() != nil {
		return
	}

	PL.Lock()
	Scan.Stage = "统计会话"
	Scan.Total = len(metas)
	Scan.Done = 0
	prevBySrc := map[string]*DatasetRecord{}
	if !opts.Full {
		for _, d := range Datasets {
			prevBySrc[d.Source] = d
		}
	}
	PL.Unlock()

	workers := runtime.NumCPU() * 4
	if workers < 4 {
		workers = 4
	}
	if workers > 32 {
		workers = 32
	}
	recs := make([]*DatasetRecord, len(metas))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for idx, m := range metas {
		if AppCtx.Err() != nil {
			break
		}
		acquired := false
		select {
		case sem <- struct{}{}:
			acquired = true
		case <-AppCtx.Done():
		}
		if !acquired {
			break
		}
		wg.Add(1)
		go func(i int, meta *DatasetMeta) {
			defer wg.Done()
			defer func() { <-sem }()
			r := Inspect(meta, prevBySrc[meta.Source], opts.Pad)
			recs[i] = r
			PL.Lock()
			Scan.Done++
			PL.Unlock()
		}(idx, m)
	}
	wg.Wait()
	if AppCtx.Err() != nil {
		return
	}

	cached := 0
	for _, r := range recs {
		if r != nil && r.Cached {
			cached++
			r.Cached = false
		}
	}
	seen, ordered, duplicateProblems := uniqueScanRecords(recs)
	problems = append(problems, duplicateProblems...)
	over := 0
	for _, r := range ordered {
		if r.PRTLocal != "" && r.PRTLocal != r.Room {
			over++
		}
	}
	if over > 0 && hasOv {
		problems = append(problems, fmt.Sprintf("界面城市/房间覆盖了 %d 个数据集的 PRT（OSS 路径与本地目录名不同，列表里已标出）。要让路径跟随文件夹名，请清空城市/房间后重新扫描", over))
	}

	allProbs := append(append([]string{}, problems...), discProbs...)
	PL.Lock()
	Datasets = seen
	Problems = allProbs
	PL.Unlock()

	saveErr := SaveDatasets(ordered)

	overCount := 0
	if hasOv {
		overCount = over
	}
	res = &ScanResult{
		Count:      len(seen),
		Problems:   len(allProbs),
		Cached:     cached,
		Scanned:    len(recs) - cached,
		Overridden: overCount,
	}
	if saveErr != nil {
		msg := "扫描结果已计算完成，但保存到 SQLite 失败：" + saveErr.Error()
		PL.Lock()
		Problems = append(Problems, msg)
		PL.Unlock()
		res.Problems++
		res.Error = msg
	}
}
