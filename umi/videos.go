package umi

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

var videoExts = map[string]bool{
	".mp4":  true,
	".m4v":  true,
	".mov":  true,
	".webm": true,
}

var videoGroupOrder = map[string]int{
	"head":       0,
	"left hand":  1,
	"right hand": 2,
}

type VideoItem struct {
	F    string `json:"f"`
	Size int64  `json:"size"`
}

func isVideoFilename(name string) bool {
	return videoExts[strings.ToLower(filepath.Ext(name))]
}

// ListVideos returns videos in the session root and first-level capture dirs (excluding metadata), ordered head -> left hand -> right hand.
func ListVideos(src string) []VideoItem {
	out := []VideoItem{}
	if src == "" {
		return out
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return out
	}
	for _, d := range entries {
		name := d.Name()
		if d.Type().IsRegular() && isVideoFilename(name) {
			if st, err := d.Info(); err == nil {
				out = append(out, VideoItem{F: name, Size: st.Size()})
			}
		} else if d.IsDir() && name != "metadata" {
			subDir := filepath.Join(src, name)
			subEntries, serr := os.ReadDir(subDir)
			if serr != nil {
				continue
			}
			for _, f := range subEntries {
				if f.Type().IsRegular() && isVideoFilename(f.Name()) {
					if st, err := f.Info(); err == nil {
						out = append(out, VideoItem{F: name + "/" + f.Name(), Size: st.Size()})
					}
				}
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		topI := out[i].F
		if idx := strings.IndexByte(topI, '/'); idx >= 0 {
			topI = topI[:idx]
		}
		topJ := out[j].F
		if idx := strings.IndexByte(topJ, '/'); idx >= 0 {
			topJ = topJ[:idx]
		}
		oi, okI := videoGroupOrder[topI]
		if !okI {
			oi = 9
		}
		oj, okJ := videoGroupOrder[topJ]
		if !okJ {
			oj = 9
		}
		if oi != oj {
			return oi < oj
		}
		return strings.ToLower(out[i].F) < strings.ToLower(out[j].F)
	})
	return out
}

// ResolveVideo safely resolves a relative video path inside a session directory, rejecting `..`, non-video files, and escaping symlinks.
func ResolveVideo(src string, rel string) string {
	if src == "" || rel == "" {
		return ""
	}
	cleanSlash := strings.ReplaceAll(rel, "\\", "/")
	if strings.HasPrefix(cleanSlash, "/") || (len(cleanSlash) >= 2 && cleanSlash[1] == ':') {
		return ""
	}
	var parts []string
	for _, seg := range strings.Split(cleanSlash, "/") {
		if seg == "" {
			continue
		}
		if seg == ".." || seg == "." {
			return ""
		}
		parts = append(parts, seg)
	}
	if len(parts) == 0 || !isVideoFilename(parts[len(parts)-1]) {
		return ""
	}

	baseReal, err := filepath.EvalSymlinks(src)
	if err != nil {
		return ""
	}
	baseReal, err = filepath.Abs(baseReal)
	if err != nil {
		return ""
	}

	joined := filepath.Join(append([]string{src}, parts...)...)
	targetReal, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return ""
	}
	targetReal, err = filepath.Abs(targetReal)
	if err != nil {
		return ""
	}

	sep := string(os.PathSeparator)
	baseCmp := baseReal
	if !strings.HasSuffix(baseCmp, sep) {
		baseCmp += sep
	}
	tgtCmp := targetReal
	if runtime.GOOS == "windows" {
		baseCmp = strings.ToLower(baseCmp)
		tgtCmp = strings.ToLower(tgtCmp)
	}
	if !strings.HasPrefix(tgtCmp, baseCmp) {
		return ""
	}
	st, err := os.Stat(targetReal)
	if err != nil || !st.Mode().IsRegular() {
		return ""
	}
	return targetReal
}
