package umi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type meterPoint struct {
	t     float64
	total int64
}

// Meter computes sliding-window throughput (bytes/sec) over a 5-second window.
type Meter struct {
	mu    sync.Mutex
	win   float64
	q     []meterPoint
	total int64
}

func NewMeter() *Meter {
	return &Meter{win: 5.0}
}

func nowSec() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

func (m *Meter) Add(n int64) {
	now := nowSec()
	m.mu.Lock()
	m.total += n
	if m.total < 0 {
		m.total = 0
	}
	m.q = append(m.q, meterPoint{t: now, total: m.total})
	for len(m.q) > 2 && now-m.q[0].t > m.win*2 {
		m.q = m.q[1:]
	}
	m.mu.Unlock()
}

func (m *Meter) Total() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}

func (m *Meter) Speed() float64 {
	now := nowSec()
	m.mu.Lock()
	defer m.mu.Unlock()
	for len(m.q) > 1 && now-m.q[0].t > m.win {
		m.q = m.q[1:]
	}
	if len(m.q) == 0 || now-m.q[0].t < 0.3 {
		return 0.0
	}
	dt := now - m.q[0].t
	diff := m.q[len(m.q)-1].total - m.q[0].total
	if diff <= 0 || dt <= 0 {
		return 0.0
	}
	return float64(diff) / dt
}

// HasRecentActivity reports whether the SDK observed outbound upload
// activity within the supplied interval. Depending on the storage SDK this may
// represent request-body bytes read or successfully completed multipart parts;
// it is an activity signal, not a server-side or WAN-interface counter.
func (m *Meter) HasRecentActivity(within time.Duration) bool {
	if m == nil || within <= 0 {
		return false
	}
	now := nowSec()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.q) == 0 {
		return false
	}
	age := now - m.q[len(m.q)-1].t
	return age >= 0 && age <= within.Seconds()
}

// JobItem tracks one dataset inside a batch Job.
type JobItem struct {
	Rel   string `json:"rel"`
	State string `json:"state"` // queued | running | done | fail
	Msg   string `json:"msg"`
}

// ProgState holds per-dataset live progress inside an active JobContext.
type ProgState struct {
	Phase      string   `json:"phase"`
	Name       string   `json:"name"`
	URL        string   `json:"url"`
	Sent       int64    `json:"sent"`
	Hashed     int64    `json:"hashed"`
	Total      int64    `json:"total"`
	Files      int      `json:"files"`
	TotalFiles int      `json:"total_files"`
	Raw        string   `json:"raw"`
	RepSpeed   *float64 `json:"rep_speed"`
	File       string   `json:"file"`
	FDone      int      `json:"fdone"`
	FTotal     int      `json:"ftotal"`
}

// JobContext represents one active dataset worker inside a batch Job.
type JobContext struct {
	Parent    *Job
	RunID     int64
	LastSpeed float64
	Tag       string
	Prog      ProgState
}

// Job represents an entire batch upload / verify / verify_deep run.
type Job struct {
	UploadLimiter *DynamicLimiter
	DatasetLimiter *DynamicLimiter
	ID         string
	Action     string
	RunID      int64
	Count      int
	TotalUnits int64
	Base       int64
	Units      []int64
	Items      []JobItem
	Done       bool
	T0         float64
	TEnd       *float64
	Log        []string
	Actives    []*JobContext
	M          *Meter
}

func JobLog(j *Job, tag string, text string) {
	if j == nil {
		return
	}
	ts := time.Now().In(TZ).Format("15:04:05")
	prefix := ""
	if tag != "" {
		prefix = "[" + tag + "] "
	}
	line := fmt.Sprintf("[%s] %s%s", ts, prefix, text)
	PL.Lock()
	j.Log = append(j.Log, line)
	if len(j.Log) > 1500 {
		j.Log = append([]string(nil), j.Log[len(j.Log)-1500:]...)
	}
	PL.Unlock()
}

func CtxLog(ctx *JobContext, text string) {
	if ctx == nil || ctx.Parent == nil {
		return
	}
	JobLog(ctx.Parent, ctx.Tag, text)
}

// AddProgressSent updates dataset progress for already-present remote files without
// counting those bytes as network throughput.
func AddProgressSent(ctx *JobContext, n int64) {
	if ctx == nil { return }
	PL.Lock()
	p := &ctx.Prog
	next := p.Sent + n
	if next < 0 { next = 0 }
	if p.Total > 0 && next > p.Total { next = p.Total }
	p.Sent = next
	PL.Unlock()
}

func AddSent(ctx *JobContext, n int64) {
	if ctx == nil {
		return
	}
	PL.Lock()
	p := &ctx.Prog
	next := p.Sent + n
	if next < 0 {
		next = 0
	}
	if p.Total > 0 && next > p.Total {
		next = p.Total
	}
	p.Sent = next
	PL.Unlock()
	// Progress may be rolled back after a failed attempt. Keep the throughput
	// meter monotonic so it measures transmitted request-body bytes, including
	// retries, rather than net logical file progress.
	if n > 0 && ctx.Parent != nil && ctx.Parent.M != nil {
		ctx.Parent.M.Add(n)
	}
}

func HashFile(path string, ctx *JobContext) (string, error) {
	return hashFileWithStop(path, ctx, nil)
}

// hashFileWithStop checks the caller's stop signal between bounded reads so
// background pre-hashing can stop promptly after an upload failure.
func hashFileWithStop(path string, ctx *JobContext, stop <-chan struct{}) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 4*1024*1024)
	for {
		select {
		case <-stop:
			return "", context.Canceled
		case <-AppCtx.Done():
			return "", AppCtx.Err()
		default:
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
			if ctx != nil {
				PL.Lock()
				ctx.Prog.Hashed += int64(n)
				PL.Unlock()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type fileSig struct {
	Size  int64
	Mtime int64
}

func statSig(st os.FileInfo) fileSig {
	return fileSig{Size: st.Size(), Mtime: st.ModTime().UnixNano()}
}

func Snapshot(source string, workers int, ctx *JobContext) ([]ManifestFile, error) {
	type pendingItem struct {
		rel  string
		path string
		sig  fileSig
	}
	var pending []pendingItem

	var visit func(dir string, parts []string) error
	visit = func(dir string, parts []string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		sort.Slice(names, func(i, j int) bool {
			return UTF16Compare(names[i], names[j]) < 0
		})
		for _, name := range names {
			if err := SafePart(name); err != nil {
				return err
			}
			nextParts := append(append([]string(nil), parts...), name)
			rel := joinSlash(nextParts)
			if IsExcluded(rel) {
				continue
			}
			full := filepath.Join(dir, name)
			st, err := os.Lstat(full)
			if err != nil {
				return err
			}
			if st.Mode()&os.ModeSymlink != 0 {
				return NewUploadError("数据集不支持符号链接: %s", full)
			}
			if st.IsDir() {
				if err := visit(full, nextParts); err != nil {
					return err
				}
			} else if st.Mode().IsRegular() {
				pending = append(pending, pendingItem{rel: rel, path: full, sig: statSig(st)})
			} else {
				return NewUploadError("不支持的文件类型: %s", full)
			}
		}
		return nil
	}

	if err := visit(source, nil); err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, NewUploadError("数据集为空: %s", source)
	}

	if workers < 1 {
		workers = 1
	}
	out := make([]ManifestFile, len(pending))
	var firstErr error
	var errMu sync.Mutex
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for idx, it := range pending {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, item pendingItem) {
			defer wg.Done()
			defer func() { <-sem }()
			errMu.Lock()
			failed := firstErr != nil
			errMu.Unlock()
			if failed {
				return
			}
			sum, err := HashFile(item.path, ctx)
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				return
			}
			after, err := os.Lstat(item.path)
			if err != nil || statSig(after) != item.sig {
				errMu.Lock()
				if firstErr == nil {
					firstErr = NewUploadError("文件正在变化，请结束采集后重试: %s", item.path)
				}
				errMu.Unlock()
				return
			}
			out[i] = ManifestFile{Path: item.rel, Size: after.Size(), SHA256: sum}
		}(idx, it)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	sort.Slice(out, func(i, j int) bool {
		return UTF16Compare(out[i].Path, out[j].Path) < 0
	})
	return out, nil
}

func joinSlash(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	s := parts[0]
	for i := 1; i < len(parts); i++ {
		s += "/" + parts[i]
	}
	return s
}

// ProgView aggregates progress across all concurrently running dataset workers in a Job, matching sample/umi/progress.py.
func ProgView(j *Job) map[string]any {
	PL.Lock()
	if j == nil {
		PL.Unlock()
		return nil
	}

	type actSnap struct {
		ProgState
		Last float64
	}
	acts := make([]actSnap, len(j.Actives))
	for i, a := range j.Actives {
		acts[i] = actSnap{ProgState: a.Prog, Last: a.LastSpeed}
	}
	base := j.Base
	tot := j.TotalUnits
	if tot < 1 {
		tot = 1
	}
	done := j.Done
	t0 := j.T0
	var tEnd *float64
	if j.TEnd != nil {
		v := *j.TEnd
		tEnd = &v
	}
	dsN := j.Count
	ok := 0
	fail := 0
	for _, it := range j.Items {
		if it.State == "done" {
			ok++
		} else if it.State == "fail" {
			fail++
		}
	}
	meter := j.M
	PL.Unlock()

	now := nowSec()
	endT := now
	if tEnd != nil {
		endT = *tEnd
	}
	el := math.Max(endT-t0, 0.001)

	var cur int64 = base
	var sentSum, hashedSum, totalSum int64
	var filesSum, totalFilesSum, fdoneSum, ftotalSum int
	var lastSum float64
	allRep := len(acts) > 0
	var repSum float64
	for _, a := range acts {
		cur += a.Hashed + a.Sent
		sentSum += a.Sent
		hashedSum += a.Hashed
		totalSum += a.Total
		filesSum += a.Files
		totalFilesSum += a.TotalFiles
		fdoneSum += a.FDone
		ftotalSum += a.FTotal
		lastSum += a.Last
		if a.RepSpeed != nil {
			repSum += *a.RepSpeed
		} else {
			allRep = false
		}
	}

	pct := 100.0
	if !done {
		pct = math.Min(100.0*float64(cur)/float64(tot), 99.9)
		if pct < 0 {
			pct = 0
		}
	}
	var eta any
	if !done && cur > 0 {
		rate := float64(cur) / el
		if rate > 0 && tot >= cur {
			eta = float64(tot-cur) / rate
		}
	}

	phase := "准备"
	name := ""
	urlStr := ""
	fileStr := ""
	rawStr := ""
	if len(acts) > 0 {
		phase = acts[0].Phase
		name = acts[0].Name
		urlStr = acts[0].URL
		fileStr = acts[0].File
		rawStr = acts[0].Raw
	}

	spd := 0.0
	avgSpd := 0.0
	if meter != nil {
		avgSpd = float64(meter.Total()) / el
		if !done {
			spd = meter.Speed()
			if spd <= 0 {
				spd = lastSum
			}
		}
	}
	var repVal any
	if allRep {
		repVal = repSum
	}

	return map[string]any{
		"pct":         pct,
		"eta":         eta,
		"elapsed":     el,
		"phase":       phase,
		"running":     len(acts),
		"done_n":      ok + fail,
		"ds_n":        dsN,
		"name":        name,
		"url":         urlStr,
		"file":        fileStr,
		"raw":         rawStr,
		"files":       filesSum,
		"total_files": totalFilesSum,
		"fdone":       fdoneSum,
		"ftotal":      ftotalSum,
		"sent":        sentSum,
		"hashed":      hashedSum,
		"total":       totalSum,
		"speed":       spd,
		"rep_speed":   repVal,
		"avg":         avgSpd,
		"ok":          ok,
		"fail":        fail,
	}
}
