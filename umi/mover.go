package umi

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var typeNameCN = map[string]string{
	"normal":      "常规",
	"calibration": "标定",
}

// TargetOf swaps the last umi_normal/umi_noraml <-> umi_calibration segment in `src` while preserving all deeper levels.
func TargetOf(src string, to string) string {
	if to == "" {
		to = "calibration"
	}
	fromType := "normal"
	if to == "normal" {
		fromType = "calibration"
	}
	allowed := map[string]bool{}
	for _, n := range LocalDirs[fromType] {
		allowed[strings.ToLower(n)] = true
	}
	prefix, parts := splitPathComponents(src)
	idx := -1
	for i := 0; i < len(parts)-1; i++ {
		if allowed[strings.ToLower(parts[i])] {
			idx = i
		}
	}
	if idx < 0 {
		return ""
	}
	newParts := make([]string, len(parts))
	copy(newParts, parts)
	newParts[idx] = TypeSubdir[to]
	return joinPathComponents(prefix, newParts, src)
}

func NewRelOf(rec *DatasetRecord, to string) string {
	if to == "" {
		to = "calibration"
	}
	idx := strings.IndexByte(rec.Rel, '/')
	if idx < 0 {
		return TypeSubdir[to] + "/" + rec.Rel
	}
	return TypeSubdir[to] + "/" + rec.Rel[idx+1:]
}

type MovePlan struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Source  string `json:"source"`
	Target  string `json:"target"`
	To      string `json:"to"`
	Reason  string `json:"reason"`
	Blocked string `json:"blocked"`
}

// PlanOne builds a MovePlan for a dataset.
// `to == ""` means automatic ISuit-V3 misplacement mode; `to == "normal" | "calibration"` means manual UMI_v3 mode.
func PlanOne(rec *DatasetRecord, existingRels map[string]bool, to string) *MovePlan {
	var reason string
	if to == "" {
		if rec.Type != "normal" || rec.Misplaced == "" {
			return nil
		}
		to = "calibration"
		reason = rec.Misplaced
		if idx := strings.Index(reason, "："); idx >= 0 {
			reason = reason[idx+len("："):]
		}
	} else {
		reason = "手动选择"
	}

	src := rec.Source
	tgt := TargetOf(src, to)
	p := &MovePlan{
		ID:      rec.ID,
		Name:    rec.Name,
		Source:  src,
		Target:  tgt,
		To:      to,
		Reason:  reason,
		Blocked: "",
	}

	if reason == "手动选择" && FmtOfName(rec.Name) != "umi_v3" {
		p.Blocked = "手动移动只用于 UMI_v3 数据"
		return p
	}
	if rec.Type == to {
		p.Blocked = "已经是" + typeNameCN[to] + "数据"
		return p
	}
	if tgt == "" {
		missingDir := "umi_normal"
		if to == "normal" {
			missingDir = "umi_calibration"
		}
		p.Blocked = "路径里找不到 " + missingDir + " 目录"
		return p
	}
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		p.Blocked = "本地目录不存在（可能已被移动/改名，请重新扫描）"
		return p
	}
	if _, err := os.Lstat(tgt); err == nil {
		p.Blocked = "目标已存在：" + tgt
		return p
	}
	newRel := NewRelOf(rec, to)
	if existingRels == nil {
		PL.Lock()
		existingRels = make(map[string]bool, len(Datasets))
		for _, d := range Datasets {
			existingRels[d.Rel] = true
		}
		PL.Unlock()
	}
	if existingRels[newRel] {
		p.Blocked = "已存在相同 OSS 路径的数据集：" + newRel
	}
	return p
}

func Plans(ids []string, to string) []MovePlan {
	PL.Lock()
	cur := make(map[string]*DatasetRecord, len(Datasets))
	rels := make(map[string]bool, len(Datasets))
	for k, v := range Datasets {
		cur[k] = v
		rels[v.Rel] = true
	}
	PL.Unlock()

	var pick []*DatasetRecord
	if to != "" {
		for _, id := range ids {
			if r, ok := cur[id]; ok {
				pick = append(pick, r)
			}
		}
	} else {
		for _, r := range cur {
			pick = append(pick, r)
		}
	}

	out := []MovePlan{}
	for _, r := range pick {
		if p := PlanOne(r, rels, to); p != nil {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Source < out[j].Source
	})
	return out
}

type MovedItem struct {
	ID     string `json:"id"`
	NewID  string `json:"new_id"`
	Name   string `json:"name"`
	Target string `json:"target"`
	To     string `json:"to"`
	Rescan bool   `json:"rescan"`
}

type FailedMove struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Error string `json:"error"`
}

func MoveIDs(ids []string, to string) ([]MovedItem, []FailedMove) {
	moved := []MovedItem{}
	failed := []FailedMove{}
	cfg := ApplyCfg()

	for _, id := range ids {
		PL.Lock()
		rec := Datasets[id]
		PL.Unlock()
		if rec == nil {
			failed = append(failed, FailedMove{ID: id, Name: "", Error: "数据集不存在，请重新扫描"})
			continue
		}
		p := PlanOne(rec, nil, to)
		if p == nil {
			failed = append(failed, FailedMove{ID: id, Name: rec.Name, Error: "不是可移动的数据（不在 umi_normal 或未判定为标定）"})
			continue
		}
		if p.Blocked != "" {
			failed = append(failed, FailedMove{ID: id, Name: rec.Name, Error: p.Blocked})
			continue
		}
		if rec.Sig != "" && SessionSig(rec.Source) != rec.Sig {
			failed = append(failed, FailedMove{ID: id, Name: rec.Name, Error: "本地内容在上次扫描后有变化（可能还在采集），请重新扫描后再移动"})
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p.Target), 0o755); err != nil {
			failed = append(failed, FailedMove{ID: id, Name: rec.Name, Error: err.Error()})
			continue
		}
		if err := os.Rename(rec.Source, p.Target); err != nil {
			failed = append(failed, FailedMove{ID: id, Name: rec.Name, Error: err.Error()})
			continue
		}

		var newRec *DatasetRecord
		movedDraft := rec.Clone()
		movedDraft.Type = p.To
		movedDraft.Source = p.Target
		movedDraft.Rel = NewRelOf(rec, p.To)
		if dsMeta, merr := MakeDS(movedDraft, cfg); merr == nil {
			newRec = Inspect(dsMeta, nil, false)
			newRec.Cached = false
		}

		PL.Lock()
		delete(Datasets, id)
		if newRec != nil {
			Datasets[newRec.ID] = newRec
		}
		PL.Unlock()

		newID := ""
		if newRec != nil {
			newID = newRec.ID
		}
		moved = append(moved, MovedItem{
			ID:     id,
			NewID:  newID,
			Name:   rec.Name,
			Target: p.Target,
			To:     p.To,
			Rescan: newRec == nil,
		})
	}

	if len(moved) > 0 {
		PL.Lock()
		recs := make([]*DatasetRecord, 0, len(Datasets))
		for _, d := range Datasets {
			recs = append(recs, d)
		}
		PL.Unlock()
		_ = SaveDatasets(recs)
	}
	return moved, failed
}
