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
	runtimeConfigMu sync.Mutex
	runtimeAppliedCfg map[string]string
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

// ResetRuntimeTuning starts a fresh upload job from its configured baseline.
// It is intentionally separate from ApplyRuntimeConfig, which preserves tuner
// discoveries when unrelated settings are saved during an active job.
func ResetRuntimeTuning(cfg map[string]string) {
	ApplyActiveRuntimeConfig(cfg, true)
}

// ApplyActiveRuntimeConfig publishes the configuration and updates runtime
// limiters under the same lock used by the tuner. This prevents a tuner from
// observing a new config snapshot halfway through its own decision.
func ApplyActiveRuntimeConfig(cfg map[string]string, resetTuning bool) {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	setActiveCfg(cfg)
	if resetTuning {
		runtimeAppliedCfg = nil
	}
	applyRuntimeConfigLocked(cfg)
}

func ApplyRuntimeConfig(cfg map[string]string) {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	applyRuntimeConfigLocked(cfg)
}

func applyRuntimeConfigLocked(cfg map[string]string) {
	previous := runtimeAppliedCfg
	parallelChanged := previous == nil ||
		previous["parallel"] != cfg["parallel"] ||
		previous["max_parallel"] != cfg["max_parallel"]
	fileLimitChanged := previous == nil ||
		previous["jobs"] != cfg["jobs"] ||
		previous["ds_jobs"] != cfg["ds_jobs"] ||
		previous["max_jobs"] != cfg["max_jobs"]
	datasetLimitChanged := previous == nil || previous["ds_jobs"] != cfg["ds_jobs"]

	jobs, _ := strconv.Atoi(cfg["jobs"])
	if jobs < 1 { jobs = 16 }
	dsJobs, _ := strconv.Atoi(cfg["ds_jobs"])
	if dsJobs < 1 { dsJobs = 1 }

	runtimeTuneMu.RLock()
	limiter := activeUploadLimiter
	dsLimiter := activeDatasetLimiter
	runtimeTuneMu.RUnlock()

	// Re-applying unrelated settings (for example target_mbps or auto_tune)
	// must not erase concurrency already discovered by the running tuner.
	// Explicit concurrency-setting changes rebase the affected runtime limit.
	if parallelChanged {
		parallel, _ := strconv.Atoi(cfg["parallel"])
		if parallel < 1 { parallel = 4 }
		maxParallel := configInt(cfg, "max_parallel", 12)
		if parallel > maxParallel { parallel = maxParallel }
		runtimeParallel.Store(int64(parallel))
	}
	if fileLimitChanged && limiter != nil {
		limiter.SetLimit(min(jobs*dsJobs, configInt(cfg, "max_jobs", 48)))
	}
	if datasetLimitChanged && dsLimiter != nil {
		dsLimiter.SetLimit(dsJobs)
	}
	runtimeAppliedCfg = copyStringMap(cfg)
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
	Change string `json:"change"`
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
	// Keep the last meaningful adjustment visible until a newer adjustment
	// replaces it. Status refreshes must not erase the notice every 2 seconds.
	if s.State != "idle" && s.Change == "" {
		s.Change = bandwidthStatus.Change
	}
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

// tuneActionAllowed revalidates a pending increase against the latest settings.
// The caller must hold runtimeConfigMu while reading the active configuration.
func tuneActionAllowed(cfg map[string]string, speedMbps float64) bool {
	if !configBool(cfg, "auto_tune", true) {
		return false
	}
	target := configFloat(cfg, "target_mbps", 350)
	return target <= 0 || speedMbps < target*0.95
}

func shouldWaitForUploadActivity(enabled, hasRecentActivity bool) bool {
	return enabled && !hasRecentActivity
}

// StartBandwidthTuner adjusts only future file admissions and future multipart
// requests. It never cancels an in-flight object upload or changes commit order.
type bandwidthTuneDecision struct {
	fileLimit int
	parallel  int
	state     string
	reason    string
	action    string
}

// decideBandwidthTune contains the tuner policy without side effects so its
// thresholds and concurrency ceilings can be tested independently.
func decideBandwidthTune(enabled bool, targetMbps, speedMbps float64, active, fileLimit, parallel, maxFiles, maxParallel int) bandwidthTuneDecision {
	d := bandwidthTuneDecision{
		fileLimit: fileLimit,
		parallel:  parallel,
		state:     "holding",
		reason:    "当前吞吐已接近目标，保持并发",
	}
	if !enabled {
		d.state, d.reason = "manual", "自动调优已关闭，使用手动配置"
		return d
	}
	if active == 0 {
		d.state, d.reason = "waiting", "等待实际文件上传，暂不调整并发"
		return d
	}
	if targetMbps > 0 && speedMbps >= targetMbps*0.95 {
		return d
	}
	if active >= fileLimit && fileLimit < maxFiles {
		next := fileLimit + 2
		if next > maxFiles {
			next = maxFiles
		}
		d.fileLimit = next
		d.state = "increasing-files"
		d.reason = "上传任务已占满并发槽，增加文件级并发"
		d.action = "files"
		return d
	}
	if parallel < maxParallel {
		d.parallel = parallel + 1
		d.state = "increasing-parts"
		d.reason = "提高后续视频的分片并发，补足文件任务数量不足"
		d.action = "parts"
		return d
	}
	if active < fileLimit {
		d.state, d.reason = "task-limited", "可用上传文件不足，且分片并发已达到上限"
		return d
	}
	d.state, d.reason = "limit-reached", "已达到配置的最大并发，可能受线路、OSS、磁盘或 CPU 限制"
	return d
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
	initialState, initialReason := "monitoring", "正在采集上传吞吐"
	if !initialEnabled {
		initialState, initialReason = "manual", "动态调优未启用，仅监控实际上传情况"
	}
	setBandwidthTuneStatus(BandwidthTuneStatus{
		Enabled: initialEnabled, TargetMbps: initialTarget,
		CurrentMbps: job.M.Speed()*8/1e6, ActiveFiles: initialActive,
		FileLimit: initialLimit, Parallel: CurrentUploadParallel(4),
		MaxFiles: configInt(initialCfg, "max_jobs", 48),
		State: initialState, Reason: initialReason,
		Change: func() string { if initialEnabled { return "等待首次调整" }; return "自动调优已关闭" }(),
	})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Refresh live upload usage frequently; keep concurrency tuning on a slower cadence.
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		lastTune := time.Now()
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
				var decision bandwidthTuneDecision
				changeNotice := ""
				switch {
				case !enabled:
					decision = decideBandwidthTune(false, target, speed, active, currentLimit, parallel, maxFiles, maxParallel)
				case time.Since(lastTune) < 20*time.Second:
					// Keep the displayed throughput and active counts fresh without
					// increasing concurrency more often than the established cadence.
					decision = bandwidthTuneDecision{
						fileLimit: currentLimit, parallel: parallel,
						state: "monitoring", reason: "正在采集上传吞吐",
					}
				default:
					lastTune = time.Now()
					if shouldWaitForUploadActivity(enabled, job.M.HasRecentActivity(10*time.Second)) {
						reason := "最近 10 秒没有实际上传字节进度，等待上传活动"
						if active == 0 {
							reason = "当前没有活动上传文件，等待上传活动后再调优"
						}
						decision = bandwidthTuneDecision{
							fileLimit: currentLimit, parallel: parallel,
							state: "waiting", reason: reason,
						}
					} else {
						decision = decideBandwidthTune(enabled, target, speed, active, currentLimit, parallel, maxFiles, maxParallel)
					}
				}

				switch decision.action {
				case "files":
					runtimeConfigMu.Lock()
					latestCfg := ActiveCfg()
					enabled = configBool(latestCfg, "auto_tune", true)
					target = configFloat(latestCfg, "target_mbps", 350)
					// Re-sample throughput under the same lock that serializes
					// configuration publication and tuner changes. The sample
					// computed at the start of this 20-second cycle may be stale.
					speed = job.M.Speed() * 8 / 1e6
					if !tuneActionAllowed(latestCfg, speed) {
						runtimeConfigMu.Unlock()
						if !configBool(latestCfg, "auto_tune", true) {
							decision.state, decision.reason = "manual", "自动调优已关闭，取消过期的并发增加"
						} else {
							decision.state, decision.reason = "holding", "最新目标带宽已达到，取消过期的并发增加"
						}
						decision.action = ""
						break
					}
					_, currentLimit := job.UploadLimiter.Stats()
					nextLimit := decision.fileLimit
					if ceiling := configInt(latestCfg, "max_jobs", 48); nextLimit > ceiling {
						nextLimit = ceiling
					}
					if nextLimit > currentLimit {
						job.UploadLimiter.SetLimit(nextLimit)
						changeNotice = fmt.Sprintf("文件并发 %d → %d", currentLimit, nextLimit)
					}
					runtimeConfigMu.Unlock()
					decision.fileLimit = nextLimit
					JobLog(job, "", fmt.Sprintf("带宽调优：文件并发上限调整为 %d，当前吞吐 %.1f Mbps，目标 %.1f Mbps", nextLimit, speed, configFloat(latestCfg, "target_mbps", 350)))
				case "parts":
					runtimeConfigMu.Lock()
					latestCfg := ActiveCfg()
					enabled = configBool(latestCfg, "auto_tune", true)
					target = configFloat(latestCfg, "target_mbps", 350)
					// Re-sample throughput under the same lock that serializes
					// configuration publication and tuner changes. The sample
					// computed at the start of this 20-second cycle may be stale.
					speed = job.M.Speed() * 8 / 1e6
					if !tuneActionAllowed(latestCfg, speed) {
						runtimeConfigMu.Unlock()
						if !configBool(latestCfg, "auto_tune", true) {
							decision.state, decision.reason = "manual", "自动调优已关闭，取消过期的并发增加"
						} else {
							decision.state, decision.reason = "holding", "最新目标带宽已达到，取消过期的并发增加"
						}
						decision.action = ""
						break
					}
					currentParallel := CurrentUploadParallel(4)
					nextParallel := currentParallel + 1
					if ceiling := configInt(latestCfg, "max_parallel", 12); nextParallel > ceiling {
						nextParallel = ceiling
					}
					if nextParallel > currentParallel {
						runtimeParallel.Store(int64(nextParallel))
						changeNotice = fmt.Sprintf("后续文件分片并发 %d → %d", currentParallel, nextParallel)
					}
					runtimeConfigMu.Unlock()
					decision.parallel = nextParallel
					JobLog(job, "", fmt.Sprintf("带宽调优：后续文件分片并发调整为 %d，当前吞吐 %.1f Mbps，目标 %.1f Mbps", nextParallel, speed, configFloat(latestCfg, "target_mbps", 350)))
				}
				setBandwidthTuneStatus(BandwidthTuneStatus{
					Enabled: enabled, TargetMbps: target, CurrentMbps: speed,
					ActiveFiles: active, FileLimit: decision.fileLimit, Parallel: decision.parallel,
					MaxFiles: maxFiles, State: decision.state, Reason: decision.reason,
					Change: changeNotice,
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
