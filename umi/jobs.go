package umi

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

func NewCtx(job *Job, rec *DatasetRecord, ds *DatasetMeta) *JobContext {
	return &JobContext{
		Parent: job,
		RunID:  job.RunID,
		Tag:    TagOf(rec.Name),
		Prog: ProgState{
			Phase: "准备",
			Name:  rec.Rel,
			URL:   ds.RawURI,
		},
	}
}

func AddUpload(rec *DatasetRecord, ds *DatasetMeta, c map[string]string, status string, nf int, rev string, elapsed float64, speed float64, errMsg string, runID int64, methodOverride string) error {
	db, err := OpenDB()
	if err != nil {
		return err
	}
	method := methodOverride
	if method == "" {
		method = MLabel(c)
	}
	var runArg any
	if runID > 0 {
		runArg = runID
	}
	_, err = db.Exec(`INSERT INTO uploads(
		run_id, rel, oss_url, source, device, device_sn, dir_date, cap_date,
		task, sub_task, operator, location, purpose, type, room,
		start_time, end_time, slices, size, duration, files,
		status, method, revision, elapsed, avg_speed, error, created_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		runArg, rec.Rel, ds.RawURI, rec.Source, rec.Device, rec.DeviceSN, rec.DirDate, rec.CapDate,
		rec.Task, rec.SubTask, rec.Operator, rec.Location, rec.Purpose, rec.Type, rec.Room,
		rec.Start, rec.End, rec.Slices, rec.Size, rec.Duration, nf,
		status, method, rev, int64(math.Round(elapsed)), speed, errMsg, NowTS(),
	)
	return err
}

func BackfillUpload(rec *DatasetRecord, ds *DatasetMeta, r *CheckResult, runID int64) bool {
	db, err := OpenDB()
	if err != nil {
		return false
	}
	var dummy int
	if err := db.QueryRow("SELECT 1 FROM uploads WHERE rel=? AND status IN ('成功','已存在') LIMIT 1", rec.Rel).Scan(&dummy); err == nil {
		return false
	}
	if err := AddUpload(rec, ds, nil, "已存在", r.RemoteFiles, r.Revision, 0, 0, "", runID, "校验补录"); err != nil {
		return false
	}
	return true
}

func ProcessOne(job *Job, i int, rec *DatasetRecord, action string, c map[string]string) {
	PL.Lock()
	job.Items[i].State = "running"
	PL.Unlock()

	t0 := time.Now()
	ds, err := MakeDS(rec, c)
	if err != nil {
		PL.Lock()
		job.Items[i].State = "fail"
		job.Items[i].Msg = truncateStr(err.Error(), 200)
		job.Base += job.Units[i]
		PL.Unlock()
		return
	}

	ctx := NewCtx(job, rec, ds)
	PL.Lock()
	job.Actives = append(job.Actives, ctx)
	if action == "verify_deep" {
		CurJob = ctx
	}
	PL.Unlock()

	defer func() {
		PL.Lock()
		job.Base += job.Units[i]
		for k, a := range job.Actives {
			if a == ctx {
				job.Actives = append(job.Actives[:k], job.Actives[k+1:]...)
				break
			}
		}
		if CurJob == ctx {
			CurJob = nil
		}
		PL.Unlock()
	}()

	err = func() error {
		oss, err := NewOSSClientFunc(BuildOSSParams(c, ds.Bucket))
		if err != nil {
			return err
		}
		oss.SetNetCallback(NetCB(ctx))

		CtxLog(ctx, fmt.Sprintf("▶ [%d/%d] %s  [%s → 桶 %s]", i+1, job.Count, rec.Rel, Formats[ds.Fmt].Label, ds.Bucket))
		CtxLog(ctx, "   OSS 路径: "+ds.RawURI)

		if action == "upload" && strings.TrimSpace(rec.Location) == "Default" {
			CtxLog(ctx, "   提示：采集位置为 Default，后续交付需要正确的房间元数据；上传程序不会自动猜测房间。")
		}

		if action == "upload" {
			st, rev, nf, err := DoUpload(ctx, oss, rec, ds, c)
			if err != nil {
				return err
			}
			el := math.Max(time.Since(t0).Seconds(), 0.001)
			spd := 0.0
			if st == "成功" {
				spd = float64(rec.Size) / el
			}
			_ = AddUpload(rec, ds, c, st, nf, rev, el, spd, "", ctx.RunID, "")
			_ = SaveCheck(rec.Rel, &CheckResult{
				Status:      "一致",
				LocalFiles:  nf,
				RemoteFiles: nf,
				Missing:     []string{},
				Extra:       []string{},
				SizeDiff:    []string{},
				HashDiff:    []string{},
				Ready:       true,
				Revision:    rev,
				Deep:        false,
				CheckedAt:   NowTS(),
			})
			PL.Lock()
			job.Items[i].State = "done"
			job.Items[i].Msg = st
			PL.Unlock()

			msg := fmt.Sprintf("   ✔ %s  %d 个文件 %s  用时 %.0fs", st, nf, FormatSize(float64(rec.Size)), el)
			if st == "成功" {
				msg += fmt.Sprintf("  平均 %s/s", FormatSize(float64(rec.Size)/el))
			}
			CtxLog(ctx, msg)
			return nil
		}

		r, err := VerifyOne(oss, ds, action == "verify_deep", ctx)
		if err != nil {
			return err
		}
		_ = SaveCheck(rec.Rel, r)
		PL.Lock()
		job.Items[i].State = "done"
		job.Items[i].Msg = r.Status
		PL.Unlock()

		CtxLog(ctx, fmt.Sprintf("   校验: %s  本地%d / 远端%d 个文件", r.Status, r.LocalFiles, r.RemoteFiles))
		if r.Status == "一致" && BackfillUpload(rec, ds, r, ctx.RunID) {
			CtxLog(ctx, "   已补录上传记录（远端已提交，本地原无记录）")
		}
		return nil
	}()

	if err != nil {
		CtxLog(ctx, fmt.Sprintf("   ✘ 失败: %v", err))
		PL.Lock()
		job.Items[i].State = "fail"
		job.Items[i].Msg = truncateStr(err.Error(), 200)
		PL.Unlock()
		if action == "upload" {
			_ = AddUpload(rec, ds, c, "失败", 0, "", time.Since(t0).Seconds(), 0, truncateStr(err.Error(), 500), ctx.RunID, "")
		}
	}
}

func RunJob(job *Job, action string, recs []*DatasetRecord, c map[string]string) {
	if action == "upload" {
		c = ApplyUploadCfg()
	} else {
		c = ApplyCfg()
	}
	workers := 1
	if action != "verify_deep" {
		if n, err := strconv.Atoi(c["ds_jobs"]); err == nil && n > 1 { workers = n }
	}
	datasetLimiter := NewDynamicLimiter(workers)
	var stopTuner func()
	if action == "upload" {
		maxFiles := configInt(c, "max_jobs", 48)
		initial := workers * configInt(c, "jobs", 16)
		if initial > maxFiles { initial = maxFiles }
		if initial < 1 { initial = 1 }
		job.UploadLimiter = NewDynamicLimiter(initial)
		job.DatasetLimiter = datasetLimiter
		registerUploadLimiter(job.UploadLimiter)
		registerDatasetLimiter(datasetLimiter)
		stopTuner = StartBandwidthTuner(job, c)
	} else {
		stopTuner = func() {}
	}
	var cleanupOnce sync.Once
	cleanupRuntime := func() {
		cleanupOnce.Do(func() {
			stopTuner()
			if job.UploadLimiter != nil {
				unregisterUploadLimiter(job.UploadLimiter)
				job.UploadLimiter = nil
			}
			if job.DatasetLimiter != nil {
				unregisterDatasetLimiter(job.DatasetLimiter)
				job.DatasetLimiter = nil
			}
		})
	}
	// Keep the job marked running until the tuner is stopped and its global
	// limiter registrations are removed. Otherwise a new job can start in the
	// small window before deferred cleanup and have its tuner status overwritten.
	defer cleanupRuntime()

	var wg sync.WaitGroup
	for i, r := range recs {
		if err := datasetLimiter.Acquire(AppCtx); err != nil {
			break
		}
		wg.Add(1)
		go func(idx int, rec *DatasetRecord) {
			defer wg.Done()
			defer datasetLimiter.Release()
			ProcessOne(job, idx, rec, action, c)
		}(i, r)

		// Stagger upload dataset startup so each newly admitted dataset can
		// establish and stabilize real traffic before the next one is launched.
		// Non-upload jobs and ds_jobs=1 keep their existing scheduling behavior.
		if action == "upload" && i < len(recs)-1 {
			_, currentLimit := datasetLimiter.Stats()
			if currentLimit > 1 {
				JobLog(job, "", fmt.Sprintf("多 WAN 启动节奏：等待第 %d 个数据集的上传速度爬升并稳定", i+1))
				wait := waitForDatasetStartup(AppCtx, job, datasetLimiter)
				switch {
				case AppCtx.Err() != nil:
					break
				case wait.Stable:
					JobLog(job, "", fmt.Sprintf("多 WAN 启动节奏：第 %d 个数据集速度稳定在 %.1f Mbps，等待 %.1f 秒，继续调度", i+1, wait.SpeedMbps, wait.Elapsed.Seconds()))
				case wait.Active == 0:
					JobLog(job, "", fmt.Sprintf("多 WAN 启动节奏：第 %d 个数据集已提前完成，继续调度", i+1))
				case wait.Limit <= 1:
					JobLog(job, "", "多 WAN 启动节奏：ds_jobs 已调整为 1，取消后续启动等待")
				default:
					JobLog(job, "", fmt.Sprintf("多 WAN 启动节奏：等待 %.1f 秒仍未稳定（当前 %.1f Mbps），超时放行下一个数据集", wait.Elapsed.Seconds(), wait.SpeedMbps))
				}
				if AppCtx.Err() != nil {
					break
				}
			}
		}
	}
	wg.Wait()

	PL.Lock()
	okCount, failCount := 0, 0
	for _, it := range job.Items {
		if it.State == "done" { okCount++ } else if it.State == "fail" { failCount++ }
	}
	PL.Unlock()

	cleanupRuntime()
	FinishRun(job.RunID, okCount, failCount)
	now := nowSec()
	PL.Lock()
	job.Done = true
	job.TEnd = &now
	CurJob = nil
	PL.Unlock()
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}
