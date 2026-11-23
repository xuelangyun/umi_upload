package umi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DynamicLimiter is a resizable, context-aware semaphore. Reducing its limit
// never interrupts active uploads; it only delays new acquisitions.
type DynamicLimiter struct {
	mu      sync.Mutex
	active  int
	limit   int
	changed chan struct{}
}

func NewDynamicLimiter(limit int) *DynamicLimiter {
	if limit < 1 { limit = 1 }
	return &DynamicLimiter{limit: limit, changed: make(chan struct{})}
}

func (l *DynamicLimiter) notifyLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *DynamicLimiter) Acquire(ctx context.Context) error {
	for {
		// Do not admit work when its parent job has already been canceled,
		// even if a slot happens to be available.
		if err := ctx.Err(); err != nil {
			return err
		}
		l.mu.Lock()
		if err := ctx.Err(); err != nil {
			l.mu.Unlock()
			return err
		}
		if l.active < l.limit {
			l.active++
			l.mu.Unlock()
			return nil
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (l *DynamicLimiter) Release() {
	l.mu.Lock()
	if l.active > 0 { l.active-- }
	l.notifyLocked()
	l.mu.Unlock()
}

func (l *DynamicLimiter) SetLimit(limit int) {
	if limit < 1 { limit = 1 }
	l.mu.Lock()
	l.limit = limit
	l.notifyLocked()
	l.mu.Unlock()
}

func (l *DynamicLimiter) Stats() (active, limit int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active, l.limit
}

var (
	runtimeParallel atomic.Int64
	runtimeTuneMu sync.RWMutex
	activeUploadLimiter *DynamicLimiter
	activeDatasetLimiter *DynamicLimiter
)



// CurrentUploadParallel is read for every new object upload, so a changed
// parallel setting affects subsequent files without rebuilding the OSS client.
func CurrentUploadParallel(fallback int) int {
	if n := int(runtimeParallel.Load()); n > 0 { return n }
	if fallback > 0 { return fallback }
	return 4
}

func ApplyRuntimeConfig(cfg map[string]string) {
	jobs, _ := strconv.Atoi(cfg["jobs"])
	if jobs < 1 { jobs = 16 }
	dsJobs, _ := strconv.Atoi(cfg["ds_jobs"])
	if dsJobs < 1 { dsJobs = 1 }
	parallel, _ := strconv.Atoi(cfg["parallel"])
	if parallel < 1 { parallel = 4 }
	runtimeTuneMu.RLock()
	limiter := activeUploadLimiter
	runtimeTuneMu.RUnlock()
	maxParallel := configInt(cfg, "max_parallel", 12)
	if maxParallel < parallel { maxParallel = parallel } // preserve legacy manual parallel settings
	runtimeParallel.Store(int64(parallel))
	if limiter != nil { limiter.SetLimit(min(jobs*dsJobs, configInt(cfg, "max_jobs", 48))) }
	runtimeTuneMu.RLock()
	dsLimiter := activeDatasetLimiter
	runtimeTuneMu.RUnlock()
	if dsLimiter != nil { dsLimiter.SetLimit(dsJobs) }
}

func registerDatasetLimiter(l *DynamicLimiter) {
	runtimeTuneMu.Lock()
	activeDatasetLimiter = l
	runtimeTuneMu.Unlock()
}

func unregisterDatasetLimiter(l *DynamicLimiter) {
	runtimeTuneMu.Lock()
	if activeDatasetLimiter == l { activeDatasetLimiter = nil }
	runtimeTuneMu.Unlock()
}

func registerUploadLimiter(l *DynamicLimiter) {
	runtimeTuneMu.Lock()
	activeUploadLimiter = l
	runtimeTuneMu.Unlock()
}

func unregisterUploadLimiter(l *DynamicLimiter) {
	runtimeTuneMu.Lock()
	if activeUploadLimiter == l { activeUploadLimiter = nil }
	runtimeTuneMu.Unlock()
}

type BandwidthTuneStatus struct {
	Enabled bool    `json:"enabled"`
	TargetMbps float64 `json:"target_mbps"`
	CurrentMbps float64 `json:"current_mbps"`
	ActiveFiles int `json:"active_files"`
	FileLimit int `json:"file_limit"`
	Parallel int `json:"parallel"`
	MaxFiles int `json:"max_files"`
	State string `json:"state"`
	Reason string `json:"reason"`
}

var (
	bandwidthStatusMu sync.RWMutex
	bandwidthStatus = BandwidthTuneStatus{State: "idle"}
)

func CurrentBandwidthTuneStatus() BandwidthTuneStatus {
	bandwidthStatusMu.RLock()
	defer bandwidthStatusMu.RUnlock()
	return bandwidthStatus
}

func setBandwidthTuneStatus(s BandwidthTuneStatus) {
	bandwidthStatusMu.Lock()
	bandwidthStatus = s
	bandwidthStatusMu.Unlock()
}

func configBool(cfg map[string]string, key string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(cfg[key]))
	if v == "" { return fallback }
	return v == "true" || v == "1" || v == "yes"
}

func configInt(cfg map[string]string, key string, fallback int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(cfg[key])); err == nil && n > 0 { return n }
	return fallback
}

func configFloat(cfg map[string]string, key string, fallback float64) float64 {
	if n, err := strconv.ParseFloat(strings.TrimSpace(cfg[key]), 64); err == nil && n >= 0 { return n }
	return fallback
}

// StartBandwidthTuner adjusts only future file admissions and future multipart
// requests. It never cancels an in-flight object upload or changes commit order.
func StartBandwidthTuner(job *Job, _ map[string]string) func() {
	if job == nil || job.Action != "upload" || job.UploadLimiter == nil {
		return func() {}
	}
	initialCfg := ActiveCfg()
	initialActive, initialLimit := job.UploadLimiter.Stats()
	initialTarget := configFloat(initialCfg, "target_mbps", 350)
	initialEnabled := configBool(initialCfg, "auto_tune", true)
	setBandwidthTuneStatus(BandwidthTuneStatus{
		Enabled: initialEnabled, TargetMbps: initialTarget,
		CurrentMbps: job.M.Speed()*8/1e6, ActiveFiles: initialActive,
		FileLimit: initialLimit, Parallel: CurrentUploadParallel(4),
		MaxFiles: configInt(initialCfg, "max_jobs", 48),
		State: "monitoring", Reason: "正在采集上传吞吐",
	})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				cfg := ActiveCfg() // read the latest saved settings on every cycle
				enabled := configBool(cfg, "auto_tune", true)
				target := configFloat(cfg, "target_mbps", 350)
				maxFiles := configInt(cfg, "max_jobs", 48)
				maxParallel := configInt(cfg, "max_parallel", 12)
				speed := job.M.Speed() * 8 / 1e6
				active, currentLimit := job.UploadLimiter.Stats()
				parallel := CurrentUploadParallel(4)
				state, reason := "holding", "当前吞吐已接近目标，保持并发"

				if !enabled {
					state, reason = "manual", "自动调优已关闭，使用手动配置"
				} else if active == 0 {
					state, reason = "waiting", "等待实际文件上传，暂不调整并发"
				} else if target <= 0 || speed < target*0.95 {
					if active >= currentLimit && currentLimit < maxFiles {
						next := currentLimit + 2
						if next > maxFiles { next = maxFiles }
						job.UploadLimiter.SetLimit(next)
						currentLimit = next
						reason = "上传任务已占满并发槽，增加文件级并发"
						state = "increasing-files"
						JobLog(job, "", fmt.Sprintf("带宽调优：文件并发上限调整为 %d，当前吞吐 %.1f Mbps，目标 %.1f Mbps", currentLimit, speed, target))
					} else if parallel < maxParallel {
						parallel++
						runtimeParallel.Store(int64(parallel))
						reason = "提高后续视频的分片并发，补足文件任务数量不足"
						state = "increasing-parts"
						JobLog(job, "", fmt.Sprintf("带宽调优：后续文件分片并发调整为 %d，当前吞吐 %.1f Mbps，目标 %.1f Mbps", parallel, speed, target))
					} else if active < currentLimit {
						reason = "可用上传文件不足，且分片并发已达到上限"
						state = "task-limited"
					} else {
						reason = "已达到配置的最大并发，可能受线路、OSS、磁盘或 CPU 限制"
						state = "limit-reached"
					}
				}
				setBandwidthTuneStatus(BandwidthTuneStatus{
					Enabled: enabled, TargetMbps: target, CurrentMbps: speed,
					ActiveFiles: active, FileLimit: currentLimit, Parallel: parallel,
					MaxFiles: maxFiles, State: state, Reason: reason,
				})
			}
		}
	}()
	return func() {
		close(stop)
		<-done
		setBandwidthTuneStatus(BandwidthTuneStatus{State: "idle"})
	}
}

func uploadLimiterFor(ctx *JobContext, fallback int) *DynamicLimiter {
	if ctx != nil && ctx.Parent != nil && ctx.Parent.UploadLimiter != nil {
		return ctx.Parent.UploadLimiter
	}
	return NewDynamicLimiter(fallback)
}
