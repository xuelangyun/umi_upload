package umi

import (
	"sort"
)

type CheckResult struct {
	Status       string   `json:"status"`
	LocalFiles   int      `json:"local_files"`
	RemoteFiles  int      `json:"remote_files"`
	Missing      []string `json:"missing"`
	Extra        []string `json:"extra"`
	SizeDiff     []string `json:"size_diff"`
	HashDiff     []string `json:"hash_diff"`
	ManifestDiff []string `json:"manifest_diff"`
	Ready        bool     `json:"ready"`
	Revision     string   `json:"revision"`
	Deep         bool     `json:"deep"`
	CheckedAt    string   `json:"checked_at"`
}
func LocalInventory(src string) (map[string]int64, error) {
	inv, err := Inventory(src)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(inv))
	for _, it := range inv {
		out[it.Rel] = it.Size
	}
	return out, nil
}

func VerifyOne(oss OSSClient, ds *DatasetMeta, deep bool, ctx *JobContext) (*CheckResult, error) {
	raw := ds.RawURI
	local, err := LocalInventory(ds.Source)
	if err != nil {
		return nil, err
	}
	objs, err := oss.ListObjects(raw)
	if err != nil {
		return nil, err
	}
	remote := map[string]int64{}
	for _, f := range objs {
		if !IsExcluded(f.Name) {
			remote[f.Name] = f.Size
		}
	}

	missing := []string{}
	extra := []string{}
	diff := []string{}
	for k, lsz := range local {
		rsz, ok := remote[k]
		if !ok {
			missing = append(missing, k)
		} else if rsz != lsz {
			diff = append(diff, k)
		}
	}
	for k := range remote {
		if _, ok := local[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	sort.Strings(diff)

	// Treat a dataset as committed only when _READY and manifest.json form
	// one valid, checksum-matched commit. Merely finding _READY is insufficient.
	commit, err := RemoteCommit(oss, raw)
	if err != nil {
		return nil, err
	}
	ready := commit != nil
	revision := ""
	if ready {
		revision = commit.Manifest.Revision
	}

	bad := []string{}
	manifestDiff := []string{}
	if ready {
		if commit.Manifest.DatasetID != ds.Name {
			manifestDiff = append(manifestDiff, "dataset_id")
		}
		if commit.Manifest.CaptureDate != ds.CaptureDate {
			manifestDiff = append(manifestDiff, "capture_date")
		}
		if commit.Manifest.DatasetType != ds.Type {
			manifestDiff = append(manifestDiff, "dataset_type")
		}
		manifestFiles := make(map[string]int64, len(commit.Manifest.Files))
		for _, f := range commit.Manifest.Files {
			manifestFiles[f.Path] = f.Size
		}
		for name, size := range local {
			if manifestSize, ok := manifestFiles[name]; !ok || manifestSize != size {
				manifestDiff = append(manifestDiff, name)
			}
		}
		for name := range manifestFiles {
			if _, ok := local[name]; !ok {
				manifestDiff = append(manifestDiff, name)
			}
		}
		sort.Strings(manifestDiff)
	}
	if deep && ready {
		remoteHash := make(map[string]string, len(commit.Manifest.Files))
		for _, f := range commit.Manifest.Files {
			remoteHash[f.Path] = f.SHA256
		}
		var totalBytes int64
		for _, sz := range local {
			totalBytes += sz
		}
		if ctx != nil {
			PL.Lock()
			ctx.Prog.Phase = "深度校验 sha256"
			ctx.Prog.Total = totalBytes
			ctx.Prog.Hashed = 0
			PL.Unlock()
		}
		snap, err := Snapshot(ds.Source, 8, ctx)
		if err != nil {
			return nil, err
		}
		for _, f := range snap {
			if remoteHash[f.Path] != f.SHA256 {
				bad = append(bad, f.Path)
			}
		}
		sort.Strings(bad)
	}

	status := "一致"
	if len(remote) == 0 && !ready {
		status = "未上传"
	} else if len(missing) > 0 || len(extra) > 0 || len(diff) > 0 || len(bad) > 0 || len(manifestDiff) > 0 {
		status = "不一致"
	} else if !ready {
		status = "缺_READY"
	}

	cap20 := func(s []string) []string {
		if len(s) > 20 {
			return s[:20]
		}
		return s
	}

	return &CheckResult{
		Status:       status,
		LocalFiles:   len(local),
		RemoteFiles:  len(remote),
		Missing:      cap20(missing),
		Extra:        cap20(extra),
		SizeDiff:     cap20(diff),
		HashDiff:     cap20(bad),
		ManifestDiff: cap20(manifestDiff),
		Ready:        ready,
		Revision:     revision,
		Deep:         deep,
		CheckedAt:    NowTS(),
	}, nil
}

func SaveCheck(rel string, r *CheckResult) error {
	db, err := OpenDB()
	if err != nil {
		return err
	}
	b, _ := jsonMarshalNoEscape(r, "")
	readyInt := 0
	if r.Ready {
		readyInt = 1
	}
	deepInt := 0
	if r.Deep {
		deepInt = 1
	}
	_, err = db.Exec(
		"INSERT OR REPLACE INTO checks(rel,status,ready,deep,detail,checked_at) VALUES(?,?,?,?,?,?)",
		rel, r.Status, readyInt, deepInt, string(b), r.CheckedAt,
	)
	if err == nil {
		invalidateDatasetRowsCache()
	}
	return err
}
