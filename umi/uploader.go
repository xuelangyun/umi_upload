package umi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type InvItem struct {
	Rel  string
	Path string
	Size int64
	Sig  fileSig
}

// Inventory lists all regular non-excluded files in a session directory.
func Inventory(src string) ([]InvItem, error) {
	var out []InvItem
	err := filepath.WalkDir(src, func(fp string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if fp == src {
			return nil
		}
		relOS, err := filepath.Rel(src, fp)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(relOS)
		if d.IsDir() {
			if d.Name() == "_pipeline" {
				return filepath.SkipDir
			}
			return nil
		}
		if IsExcluded(rel) {
			return nil
		}
		st, err := os.Lstat(fp)
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return NewUploadError("不支持的文件类型(符号链接等): %s", fp)
		}
		out = append(out, InvItem{
			Rel:  rel,
			Path: fp,
			Size: st.Size(),
			Sig:  statSig(st),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, NewUploadError("数据集为空: %s", src)
	}
	return out, nil
}

// Commit verifies remote inventory, writes manifest.json (with read-back verification), then writes _READY (with read-back verification).
func Commit(oss OSSClient, ds *DatasetMeta, files []ManifestFile) (string, string, error) {
	raw := ds.RawURI
	manifest, err := MakeManifest(ds, files)
	if err != nil {
		return "", "", err
	}
	if err := VerifyInventory(oss, raw, files); err != nil {
		return "", "", err
	}
	cc, err := RemoteCommit(oss, raw)
	if err != nil {
		return "", "", err
	}
	if cc != nil {
		if !SameContent(cc.Manifest, manifest) {
			return "", "", NewUploadError("其他上传进程已提交不同版本，请勿同时上传同一数据集")
		}
		return "已存在", cc.Manifest.Revision, nil
	}

	mb, err := jsonMarshalNoEscape(manifest, "  ")
	if err != nil {
		return "", "", err
	}
	if err := oss.PutObject(raw+"manifest.json", mb); err != nil {
		return "", "", err
	}
	readBack, found, err := oss.ReadObject(raw + "manifest.json")
	if err != nil {
		return "", "", err
	}
	if !found || string(readBack) != string(mb) {
		return "", "", NewUploadError("远端清单校验失败，未发送完成标记")
	}

	mSum := sha256.Sum256(mb)
	marker := ReadyMarker{
		SchemaVersion:  1,
		Revision:       manifest.Revision,
		ManifestSHA256: hex.EncodeToString(mSum[:]),
	}
	rb, err := jsonMarshalNoEscape(marker, "  ")
	if err != nil {
		return "", "", err
	}
	if err := oss.PutObject(raw+"_READY", rb); err != nil {
		return "", "", err
	}
	okCommit, err := RemoteCommit(oss, raw)
	if err != nil {
		return "", "", err
	}
	if okCommit == nil || okCommit.Ready.ManifestSHA256 != marker.ManifestSHA256 {
		return "", "", NewUploadError("完成标记确认失败，请重试以确认提交状态")
	}
	return "成功", manifest.Revision, nil
}

func CkptRoot(ds *DatasetMeta) string {
	sum := sha256.Sum256([]byte(ds.RawURI))
	return filepath.Join(CkptDir, hex.EncodeToString(sum[:])[:16])
}

func datasetInvSig(inv []InvItem) string {
	var sb strings.Builder
	for _, it := range inv {
		fmt.Fprintf(&sb, "%s:%d:%d\n", it.Rel, it.Size, it.Sig.Mtime)
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])[:24]
}


func prepareUploadCheckpoint(ds *DatasetMeta, inv []InvItem) (root, sub string, isResume bool, err error) {
	root = CkptRoot(ds)
	sub = filepath.Join(root, datasetInvSig(inv))
	if err = os.MkdirAll(sub, 0o700); err != nil {
		return "", "", false, fmt.Errorf("创建断点续传目录失败: %w", err)
	}

	markFile := filepath.Join(sub, "started")
	_, statErr := os.Stat(markFile)
	switch {
	case statErr == nil:
		isResume = true
	case !os.IsNotExist(statErr):
		return "", "", false, fmt.Errorf("检查断点续传标记失败: %w", statErr)
	}

	file, openErr := os.OpenFile(markFile, os.O_CREATE|os.O_WRONLY, 0o600)
	if openErr != nil {
		return "", "", false, fmt.Errorf("写入断点续传标记失败: %w", openErr)
	}
	if syncErr := file.Sync(); syncErr != nil {
		_ = file.Close()
		return "", "", false, fmt.Errorf("持久化断点续传标记失败: %w", syncErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		return "", "", false, fmt.Errorf("关闭断点续传标记失败: %w", closeErr)
	}
	return root, sub, isResume, nil
}

// NetCB returns a NetWaitCallback updating the dataset progress phase and job log during network outages.
func NetCB(ctx *JobContext) NetWaitCallback {
	var savedPhase string
	return func(ev string, waited float64, errText string) {
		switch ev {
		case "down":
			compact := strings.Join(strings.Fields(errText), " ")
			if len(compact) > 160 {
				compact = compact[:160]
			}
			PL.Lock()
			savedPhase = ctx.Prog.Phase
			ctx.Prog.Phase = "⚠ 网络中断，等待恢复后自动续传…"
			PL.Unlock()
			CtxLog(ctx, "   ⚠ 网络中断，等待恢复后自动续传: "+compact)
		case "tick":
			PL.Lock()
			ctx.Prog.Raw = fmt.Sprintf("网络中断，已等待 %d 秒（恢复后自动续传）", int(waited))
			PL.Unlock()
		default:
			PL.Lock()
			if savedPhase != "" {
				ctx.Prog.Phase = savedPhase
			}
			ctx.Prog.Raw = ""
			PL.Unlock()
			if ev == "up" {
				CtxLog(ctx, fmt.Sprintf("   ✔ 网络已恢复（等待 %d 秒），继续上传", int(waited)))
			} else {
				CtxLog(ctx, "   ✘ 网络中断超过等待上限，放弃本次")
			}
		}
	}
}

func CheckUnit(oss OSSClient, target string, expect map[string]int64, exact bool) error {
	objs, err := oss.ListObjects(target)
	if err != nil {
		return err
	}
	remote := map[string]int64{}
	for _, f := range objs {
		if !IsExcluded(f.Name) {
			remote[f.Name] = f.Size
		}
	}
	var bad, extra []string
	for k, v := range expect {
		if sz, ok := remote[k]; !ok || sz != v {
			bad = append(bad, k)
		}
	}
	if exact {
		for k := range remote {
			if _, ok := expect[k]; !ok {
				extra = append(extra, k)
			}
		}
	}
	sort.Strings(bad)
	sort.Strings(extra)
	if len(bad) > 0 || len(extra) > 0 {
		firstBad := "-"
		if len(bad) > 0 {
			firstBad = bad[0]
		}
		firstExtra := "-"
		if len(extra) > 0 {
			firstExtra = extra[0]
		}
		return NewUploadError("文件夹上传后远端与本地不一致 %s：缺失/大小不符 %d 个(如 %s)，多余 %d 个(如 %s)",
			target, len(bad), firstBad, len(extra), firstExtra)
	}
	return nil
}

// AheadHasher computes SHA256 in a background goroutine up to 256MB ahead of uploaded bytes.
type AheadHasher struct {
	ctx  *JobContext
	inv  []InvItem
	res  map[string]string
	err  error
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

const aheadMaxBytes int64 = 256 * 1024 * 1024

func StartAheadHasher(ctx *JobContext, inv []InvItem) *AheadHasher {
	ah := &AheadHasher{
		ctx:  ctx,
		inv:  inv,
		res:  make(map[string]string, len(inv)),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go ah.run()
	return ah
}

func (ah *AheadHasher) run() {
	defer close(ah.done)
	for _, it := range ah.inv {
		for {
			PL.Lock()
			ahead := ah.ctx.Prog.Hashed - ah.ctx.Prog.Sent
			PL.Unlock()
			if ahead <= aheadMaxBytes {
				break
			}
			select {
			case <-ah.stop:
				return
			case <-AppCtx.Done():
				ah.err = AppCtx.Err()
				return
			case <-time.After(40 * time.Millisecond):
			}
		}
		select {
		case <-ah.stop:
			return
		default:
		}
		sum, err := hashFileWithStop(it.Path, ah.ctx, ah.stop)
		if err != nil {
			ah.err = err
			return
		}
		ah.res[it.Rel] = sum
	}
}

func (ah *AheadHasher) Stop() {
	ah.once.Do(func() { close(ah.stop) })
	// Do not let a background hash continue consuming disk and CPU after
	// its owning upload pipeline has already failed or returned.
	<-ah.done
}

func (ah *AheadHasher) Result() (map[string]string, error) {
	<-ah.done
	if ah.err != nil {
		return nil, ah.err
	}
	return ah.res, nil
}

type folderGroup struct {
	Top     string
	Indices []int
}

func UploadOrdered(ctx *JobContext, oss OSSClient, ds *DatasetMeta, inv []InvItem, c map[string]string, ckptSubDir string, skipRemote map[string]int64) ([]ManifestFile, error) {
	sortedInv := append([]InvItem(nil), inv...)
	sort.Slice(sortedInv, func(i, j int) bool {
		return UTF16Compare(sortedInv[i].Rel, sortedInv[j].Rel) < 0
	})

	var groups []folderGroup
	groupIdx := map[string]int{}
	for idx, it := range sortedInv {
		top := ""
		if slash := strings.IndexByte(it.Rel, '/'); slash >= 0 {
			top = it.Rel[:slash]
		}
		gi, ok := groupIdx[top]
		if !ok {
			gi = len(groups)
			groupIdx[top] = gi
			groups = append(groups, folderGroup{Top: top})
		}
		groups[gi].Indices = append(groups[gi].Indices, idx)
	}

	PL.Lock()
	ctx.Prog.FTotal = len(groups)
	ctx.Prog.FDone = 0
	PL.Unlock()

	jobsN, _ := strconv.Atoi(c["jobs"])
	if jobsN < 1 {
		jobsN = 16
	}
	order := EffectiveOrder(c)
	keyPrefix, err := oss.KeyOf(ds.RawURI)
	if err != nil {
		return nil, err
	}

	if order == "slice" {
		return uploadBySliceVerified(ctx, oss, ds, sortedInv, groups, jobsN, keyPrefix, ckptSubDir, skipRemote)
	}

	res := make([]*ManifestFile, len(sortedInv))
	var errMu sync.Mutex
	var firstErr error
	startedGroups := map[int]bool{}
	nxt := 0

	labelOf := func(g folderGroup) string {
		if g.Top != "" {
			return ds.RawURI + g.Top + "/"
		}
		return ds.RawURI + "（根目录文件）"
	}

	uploadSingle := func(gi, idx int) {
		errMu.Lock()
		failed := firstErr != nil
		errMu.Unlock()
		if failed || AppCtx.Err() != nil {
			return
		}

		PL.Lock()
		isFirst := !startedGroups[gi]
		startedGroups[gi] = true
		PL.Unlock()
		if isFirst {
			var gSize int64
			for _, k := range groups[gi].Indices {
				gSize += sortedInv[k].Size
			}
			CtxLog(ctx, fmt.Sprintf("   ▶ 文件夹 [%d/%d] %s  %d 个文件 %s",
				gi+1, len(groups), labelOf(groups[gi]), len(groups[gi].Indices), FormatSize(float64(gSize))))
		}

		it := sortedInv[idx]
		PL.Lock()
		ctx.Prog.File = it.Rel
		PL.Unlock()

		sha, herr := HashFile(it.Path, ctx)
		if herr != nil {
			errMu.Lock()
			if firstErr == nil {
				firstErr = herr
			}
			errMu.Unlock()
			return
		}

		if sz, ok := skipRemote[it.Rel]; ok && sz == it.Size {
			AddProgressSent(ctx, it.Size)
		} else {
			uerr := oss.UploadFile(it.Path, keyPrefix+it.Rel, it.Size, ckptSubDir, func(delta int64) {
				AddProgressSent(ctx, delta)
			}, func(delta int64) {
				AddNetworkTraffic(ctx, delta)
			})
			if uerr != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = uerr
				}
				errMu.Unlock()
				return
			}
		}

		after, serr := os.Lstat(it.Path)
		if serr != nil || statSig(after) != it.Sig {
			errMu.Lock()
			if firstErr == nil {
				firstErr = NewUploadError("文件在上传过程中变化: %s", it.Path)
			}
			errMu.Unlock()
			return
		}

		mf := &ManifestFile{Path: it.Rel, Size: it.Size, SHA256: sha}
		PL.Lock()
		ctx.Prog.Files++
		res[idx] = mf
		for nxt < len(groups) {
			allDone := true
			for _, k := range groups[nxt].Indices {
				if res[k] == nil {
					allDone = false
					break
				}
			}
			if !allDone {
				break
			}
			ctx.Prog.FDone++
			doneIdx := nxt
			nxt++
			PL.Unlock()
			CtxLog(ctx, fmt.Sprintf("   ✔ 文件夹 [%d/%d] %s  完成", doneIdx+1, len(groups), labelOf(groups[doneIdx])))
			PL.Lock()
		}
		PL.Unlock()
	}

	limiter := uploadLimiterFor(ctx, jobsN)
	shouldStopScheduling := func() bool {
		errMu.Lock()
		failed := firstErr != nil
		errMu.Unlock()
		return failed || AppCtx.Err() != nil
	}
	if order == "pipeline" {
		var wg sync.WaitGroup
		for gi, g := range groups {
			if shouldStopScheduling() {
				break
			}
			for _, idx := range g.Indices {
				if shouldStopScheduling() {
					break
				}
				if err := limiter.Acquire(AppCtx); err != nil {
					errMu.Lock()
					if firstErr == nil { firstErr = err }
					errMu.Unlock()
					break
				}
				// A previous upload may have failed while Acquire was waiting.
				// Release the slot instead of scheduling a no-op goroutine.
				if shouldStopScheduling() {
					limiter.Release()
					break
				}
				wg.Add(1)
				go func(groupI, itemI int) {
					defer wg.Done()
					defer limiter.Release()
					uploadSingle(groupI, itemI)
				}(gi, idx)
			}
		}
		wg.Wait()
	} else {
		for gi, g := range groups {
			if shouldStopScheduling() { break }
			var wg sync.WaitGroup
			for _, idx := range g.Indices {
				if shouldStopScheduling() { break }
				if err := limiter.Acquire(AppCtx); err != nil {
					errMu.Lock()
					if firstErr == nil { firstErr = err }
					errMu.Unlock()
					break
				}
				if shouldStopScheduling() {
					limiter.Release()
					break
				}
				wg.Add(1)
				go func(groupI, itemI int) {
					defer wg.Done()
					defer limiter.Release()
					uploadSingle(groupI, itemI)
				}(gi, idx)
			}
			wg.Wait()
			if firstErr != nil { break }
		}
	}

	if firstErr != nil {
		return nil, firstErr
	}
	out := make([]ManifestFile, len(sortedInv))
	for i, mf := range res {
		if mf == nil {
			return nil, NewUploadError("上传未完成")
		}
		out[i] = *mf
	}
	return out, nil
}

func uploadBySliceVerified(ctx *JobContext, oss OSSClient, ds *DatasetMeta, sortedInv []InvItem, groups []folderGroup, jobsN int, keyPrefix, ckptSubDir string, skipRemote map[string]int64) ([]ManifestFile, error) {
	hasher := StartAheadHasher(ctx, sortedInv)
	defer hasher.Stop()

	limiter := uploadLimiterFor(ctx, jobsN)
	for gi, g := range groups {
		var gSize int64
		for _, k := range g.Indices {
			gSize += sortedInv[k].Size
		}
		title := "会话根目录文件"
		if g.Top != "" {
			title = ds.RawURI + g.Top + "/"
		}
		CtxLog(ctx, fmt.Sprintf("   ▶ 文件夹 [%d/%d] %s  %d 个文件 %s", gi+1, len(groups), title, len(g.Indices), FormatSize(float64(gSize))))
		PL.Lock()
		ctx.Prog.File = title
		PL.Unlock()

		t0 := time.Now()
		var errMu sync.Mutex
		var firstErr error
		var wg sync.WaitGroup
		shouldStopGroupScheduling := func() bool {
			errMu.Lock()
			failed := firstErr != nil
			errMu.Unlock()
			return failed || AppCtx.Err() != nil
		}

		for _, idx := range g.Indices {
			if shouldStopGroupScheduling() {
				break
			}
			it := sortedInv[idx]
			if err := limiter.Acquire(AppCtx); err != nil {
				errMu.Lock()
				if firstErr == nil { firstErr = err }
				errMu.Unlock()
				break
			}
			// A sibling may have failed while this goroutine waited for a slot.
			if shouldStopGroupScheduling() {
				limiter.Release()
				break
			}
			wg.Add(1)
			go func(item InvItem) {
				defer wg.Done()
				defer limiter.Release()

				errMu.Lock()
				failed := firstErr != nil
				errMu.Unlock()
				if failed || AppCtx.Err() != nil {
					return
				}

				if sz, ok := skipRemote[item.Rel]; ok && sz == item.Size {
					AddProgressSent(ctx, item.Size)
				} else {
					uerr := oss.UploadFile(item.Path, keyPrefix+item.Rel, item.Size, ckptSubDir, func(delta int64) {
						AddProgressSent(ctx, delta)
					}, func(delta int64) {
						AddNetworkTraffic(ctx, delta)
					})
					if uerr != nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = uerr
						}
						errMu.Unlock()
						return
					}
				}
				PL.Lock()
				ctx.Prog.Files++
				PL.Unlock()
			}(it)
		}
		wg.Wait()
		if firstErr != nil {
			return nil, firstErr
		}
		if err := AppCtx.Err(); err != nil {
			return nil, err
		}

		// Verify remote folder list immediately after slice finishes
		tgt := ds.RawURI
		if g.Top != "" {
			tgt = ds.RawURI + g.Top + "/"
		}
		expect := make(map[string]int64, len(g.Indices))
		for _, idx := range g.Indices {
			it := sortedInv[idx]
			relInGroup := it.Rel
			if g.Top != "" {
				relInGroup = it.Rel[len(g.Top)+1:]
			}
			expect[relInGroup] = it.Size
		}
		if err := CheckUnit(oss, tgt, expect, g.Top != ""); err != nil {
			return nil, err
		}

		dt := math.Max(time.Since(t0).Seconds(), 0.001)
		PL.Lock()
		ctx.LastSpeed = float64(gSize) / dt
		ctx.Prog.FDone++
		PL.Unlock()
		CtxLog(ctx, fmt.Sprintf("   ✔ 文件夹 [%d/%d] 完成  用时 %.1fs  平均 %s/s",
			gi+1, len(groups), dt, FormatSize(float64(gSize)/dt)))
	}

	for _, it := range sortedInv {
		after, err := os.Lstat(it.Path)
		if err != nil || statSig(after) != it.Sig {
			return nil, NewUploadError("文件在上传过程中变化: %s", it.Path)
		}
	}
	hashes, err := hasher.Result()
	if err != nil {
		return nil, err
	}
	out := make([]ManifestFile, len(sortedInv))
	for i, it := range sortedInv {
		out[i] = ManifestFile{Path: it.Rel, Size: it.Size, SHA256: hashes[it.Rel]}
	}
	return out, nil
}

func DoUpload(ctx *JobContext, oss OSSClient, rec *DatasetRecord, ds *DatasetMeta, c map[string]string) (string, string, int, error) {
	raw := ds.RawURI
	inv, err := Inventory(ds.Source)
	if err != nil {
		return "", "", 0, err
	}
	var totalBytes int64
	for _, it := range inv {
		totalBytes += it.Size
	}
	PL.Lock()
	ctx.Prog.Total = totalBytes
	ctx.Prog.TotalFiles = len(inv)
	ctx.Prog.Phase = "检查远端"
	PL.Unlock()

	ex, err := RemoteCommit(oss, raw)
	if err != nil {
		return "", "", 0, err
	}
	if ex != nil {
		// A committed dataset is reusable only when its identity, inventory,
		// sizes, and local content hashes all match the remote manifest. Size
		// equality alone can incorrectly accept replaced files.
		if ex.Manifest.DatasetID != ds.Name ||
			ex.Manifest.CaptureDate != ds.CaptureDate ||
			ex.Manifest.DatasetType != ds.Type {
			return "", "", 0, NewUploadError("同一路径已提交过其他数据集，拒绝覆盖: %s", raw)
		}

		localFiles, hashErr := Snapshot(ds.Source, 8, ctx)
		if hashErr != nil {
			return "", "", 0, hashErr
		}
		remoteFiles := make(map[string]ManifestFile, len(ex.Manifest.Files))
		for _, file := range ex.Manifest.Files {
			remoteFiles[file.Path] = file
		}
		sameContent := len(ex.Manifest.Files) == len(localFiles)
		if sameContent {
			for _, local := range localFiles {
				remote, ok := remoteFiles[local.Path]
				if !ok || remote.Size != local.Size || remote.SHA256 != local.SHA256 {
					sameContent = false
					break
				}
			}
		}
		if !sameContent {
			return "", "", 0, NewUploadError("同一路径已提交过不同内容，拒绝覆盖: %s", raw)
		}
		if err := VerifyInventory(oss, raw, localFiles); err != nil {
			return "", "", 0, err
		}
		CtxLog(ctx, "   远端已提交且文件 SHA-256 一致，跳过上传")
		if err := os.RemoveAll(CkptRoot(ds)); err != nil {
			CtxLog(ctx, "   ⚠ 清理已完成任务的 checkpoint 失败："+truncateStr(err.Error(), 300))
		}
		return "已存在", ex.Manifest.Revision, len(inv), nil
	}

	// Persistent breakpoint resume check (umi_ckpt/). Do not continue with a
	// partially initialized marker: it would make a later retry look like a
	// fresh upload and silently disable completed-object resume.
	ckRoot, ckptSub, isResume, err := prepareUploadCheckpoint(ds, inv)
	if err != nil {
		return "", "", 0, err
	}

	skipRemote := map[string]int64{}
	if isResume {
		CtxLog(ctx, "   断点续传：上次未传完，已传到 OSS 的文件会跳过，大文件按 checkpoint 接着传")
		remObjs, listErr := oss.ListObjects(raw)
		if listErr != nil {
			return "", "", 0, fmt.Errorf("读取远端对象以恢复断点失败: %w", listErr)
		}
		for _, ro := range remObjs {
			if !IsExcluded(ro.Name) {
				skipRemote[ro.Name] = ro.Size
			}
		}
	}

	order := EffectiveOrder(c)
	phaseDesc := "逐文件夹"
	if order == "pipeline" {
		phaseDesc = "顺序流水线"
	} else if order == "slice" {
		phaseDesc = "逐分片核对"
	}
	PL.Lock()
	ctx.Prog.Phase = fmt.Sprintf("上传中（Go SDK · %s）", phaseDesc)
	PL.Unlock()

	files, err := UploadOrdered(ctx, oss, ds, inv, c, ckptSub, skipRemote)
	if err != nil {
		return "", "", 0, err
	}

	PL.Lock()
	ctx.Prog.Phase = "复核并提交（manifest.json → _READY）"
	PL.Unlock()
	CtxLog(ctx, "   复核完成后写入 manifest.json 与 _READY")

	st, rev, err := Commit(oss, ds, files)
	if err != nil {
		return "", "", 0, err
	}
	if err := os.RemoveAll(ckRoot); err != nil {
		CtxLog(ctx, "   ⚠ 上传已提交，但清理 checkpoint 失败："+truncateStr(err.Error(), 300))
	}
	return st, rev, len(files), nil
}
