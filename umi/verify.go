package umi

import (
	"encoding/json"
	"sort"
)

type CheckResult struct {
	Status      string   `json:"status"`
	LocalFiles  int      `json:"local_files"`
	RemoteFiles int      `json:"remote_files"`
	Missing     []string `json:"missing"`
	Extra       []string `json:"extra"`
	SizeDiff    []string `json:"size_diff"`
	HashDiff    []string `json:"hash_diff"`
	Ready       bool     `json:"ready"`
	Revision    string   `json:"revision"`
	Deep        bool     `json:"deep"`
	CheckedAt   string   `json:"checked_at"`
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

	rdBytes, ready, err := oss.ReadObject(raw + "_READY")
	if err != nil {
		return nil, err
	}
	revision := ""
	if ready {
		var rm ReadyMarker
		if json.Unmarshal(rdBytes, &rm) == nil {
			revision = rm.Revision
		}
	}

	bad := []string{}
	if deep && ready {
		mfBytes, mfFound, err := oss.ReadObject(raw + "manifest.json")
		if err != nil {
			return nil, err
		}
		if !mfFound {
			bad = append(bad, "manifest.json")
		} else {
			var mf Manifest
			if err := json.Unmarshal(mfBytes, &mf); err != nil {
				return nil, err
			}
			remoteHash := make(map[string]string, len(mf.Files))
			for _, f := range mf.Files {
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
	}

	status := "一致"
	if len(remote) == 0 {
		status = "未上传"
	} else if len(missing) > 0 || len(extra) > 0 || len(diff) > 0 || len(bad) > 0 {
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
		Status:      status,
		LocalFiles:  len(local),
		RemoteFiles: len(remote),
		Missing:     cap20(missing),
		Extra:       cap20(extra),
		SizeDiff:    cap20(diff),
		HashDiff:    cap20(bad),
		Ready:       ready,
		Revision:    revision,
		Deep:        deep,
		CheckedAt:   NowTS(),
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
	return err
}
