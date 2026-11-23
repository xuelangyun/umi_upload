package umi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

type UploadError struct {
	Msg string
}

func (e *UploadError) Error() string { return e.Msg }

func NewUploadError(format string, args ...any) *UploadError {
	return &UploadError{Msg: fmt.Sprintf(format, args...)}
}

var (
	SessionRe = regexp.MustCompile(`^.+_(\d{4}-\d{2}-\d{2})-\d{2}-\d{2}-\d{2}$`)
	UmiV3Re   = regexp.MustCompile(`^UMI_v3_(\d{8})_(\d{6})$`)
	PRTRe     = regexp.MustCompile(`(?i)^PRT_([A-Za-z]+)_room(\d+)$`)
	unsafeRe  = regexp.MustCompile(`[\\/\r\n?#\x00]`)
)

var UmiV3Dirs = []string{"head", "left hand", "right hand"}

type FormatSpec struct {
	Key    string
	Label  string
	Token  string
	Conf   string
	NameRe *regexp.Regexp
	Desc   string
}

var FormatOrder = []string{"isuit", "umi_v3"}

var Formats = map[string]FormatSpec{
	"isuit": {
		Key:    "isuit",
		Label:  "ISuit-V3",
		Token:  "ISuit-V3_xxx",
		Conf:   "datacollect_conf.yaml",
		NameRe: SessionRe,
		Desc:   "datacollect_conf.yaml 和采集切片",
	},
	"umi_v3": {
		Key:    "umi_v3",
		Label:  "UMI_v3",
		Token:  "UMI_v3_xxx",
		Conf:   "metadata/conf.yaml",
		NameRe: UmiV3Re,
		Desc:   "metadata/conf.yaml 和 head / left hand / right hand 采集目录",
	},
}

func SessionDate(name string, fmtName string) string {
	var keys []string
	if fmtName != "" {
		if _, ok := Formats[fmtName]; !ok {
			return ""
		}
		keys = []string{fmtName}
	} else {
		keys = FormatOrder
	}
	for _, f := range keys {
		m := Formats[f].NameRe.FindStringSubmatch(name)
		if m != nil {
			d := m[1]
			if strings.Contains(d, "-") {
				return d
			}
			return d[:4] + "-" + d[4:6] + "-" + d[6:]
		}
	}
	return ""
}

func FmtOfName(name string) string {
	for _, f := range FormatOrder {
		if SessionDate(name, f) != "" {
			return f
		}
	}
	return ""
}

func ValidateSession(fmtName string, source string, entries []os.DirEntry) error {
	name := filepath.Base(source)
	if fmtName == "umi_v3" {
		confPath := filepath.Join(source, "metadata", "conf.yaml")
		st, err := os.Stat(confPath)
		if err != nil || st.IsDir() {
			return NewUploadError("数据集缺少 metadata/conf.yaml")
		}
		for _, sub := range UmiV3Dirs {
			found := false
			for _, e := range entries {
				if e.Name() == sub && isDirEntryDir(source, e) {
					found = true
					break
				}
			}
			if !found {
				return NewUploadError("数据集缺少采集目录 %s", sub)
			}
		}
		return nil
	}

	hasConf := false
	hasSlice := false
	prefix := name + "_"
	for _, e := range entries {
		en := e.Name()
		if en == "datacollect_conf.yaml" && !isDirEntryDir(source, e) {
			hasConf = true
		}
		if isDirEntryDir(source, e) && strings.HasPrefix(en, prefix) {
			rest := en[len(prefix):]
			if isAllDigits(rest) {
				hasSlice = true
			}
		}
	}
	if !hasConf {
		return NewUploadError("数据集缺少 datacollect_conf.yaml")
	}
	if !hasSlice {
		return NewUploadError("数据集缺少数字编号的采集切片")
	}
	return nil
}

func isDirEntryDir(parent string, e os.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&os.ModeSymlink != 0 {
		st, err := os.Stat(filepath.Join(parent, e.Name()))
		return err == nil && st.IsDir()
	}
	return false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func UTF16Compare(a, b string) int {
	if a == b {
		return 0
	}
	ua := utf16.Encode([]rune(a))
	ub := utf16.Encode([]rune(b))
	n := len(ua)
	if len(ub) < n {
		n = len(ub)
	}
	for i := 0; i < n; i++ {
		if ua[i] < ub[i] {
			return -1
		}
		if ua[i] > ub[i] {
			return 1
		}
	}
	if len(ua) < len(ub) {
		return -1
	}
	if len(ua) > len(ub) {
		return 1
	}
	return 0
}

func ToYYMMDD(value string) (string, error) {
	text := strings.TrimSpace(value)
	var full string
	switch {
	case len(text) == 10 && text[4] == '-' && text[7] == '-' && isAllDigits(text[:4]+text[5:7]+text[8:]):
		full = text[:4] + text[5:7] + text[8:]
	case len(text) == 8 && isAllDigits(text):
		full = text
	case len(text) == 6 && isAllDigits(text):
		full = "20" + text
	default:
		return "", NewUploadError("日期无效: %q", value)
	}
	year, _ := strconv.Atoi(full[:4])
	month, _ := strconv.Atoi(full[4:6])
	day, _ := strconv.Atoi(full[6:])
	t := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if t.Year() != year || int(t.Month()) != month || t.Day() != day || year < 2000 || year > 2099 {
		return "", NewUploadError("采集日期无效: %s", value)
	}
	return full[2:], nil
}

func IsExcluded(rel string) bool {
	rel = strings.ReplaceAll(rel, "\\", "/")
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if p == "_pipeline" {
			return true
		}
	}
	if len(parts) > 0 && Markers[parts[len(parts)-1]] {
		return true
	}
	return false
}

func SafePart(name string) error {
	if name == "" || name == "." || name == ".." || unsafeRe.MatchString(name) {
		return NewUploadError("目录名不安全: %q", name)
	}
	return nil
}

func FormatSize(n float64) string {
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		n = 0
	}
	units := []string{"B", "KB", "MB", "GB"}
	for i, u := range units {
		if n < 1024 || i == len(units)-1 {
			if u == "B" {
				return fmt.Sprintf("%dB", int64(math.Round(n)))
			}
			return fmt.Sprintf("%.1f%s", n, u)
		}
		n /= 1024.0
	}
	return "0B"
}

// Manifest & Commit types

type ManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type manifestIdentity struct {
	DatasetID   string         `json:"dataset_id"`
	CaptureDate string         `json:"capture_date"`
	DatasetType string         `json:"dataset_type"`
	Files       []ManifestFile `json:"files"`
}

type Manifest struct {
	SchemaVersion int            `json:"schema_version"`
	DatasetID     string         `json:"dataset_id"`
	CaptureDate   string         `json:"capture_date"`
	DatasetType   string         `json:"dataset_type"`
	Files         []ManifestFile `json:"files"`
	Revision      string         `json:"revision"`
	CreatedAt     string         `json:"created_at"`
}

type ReadyMarker struct {
	SchemaVersion  int    `json:"schema_version"`
	Revision       string `json:"revision"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

type CommitInfo struct {
	Ready    ReadyMarker
	Manifest Manifest
}

func jsonMarshalNoEscape(v any, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent != "" {
		enc.SetIndent("", indent)
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if indent == "" && len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

func manifestRevision(datasetID, captureDate, datasetType string, files []ManifestFile) (string, error) {
	sorted := make([]ManifestFile, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool {
		return UTF16Compare(sorted[i].Path, sorted[j].Path) < 0
	})
	id := manifestIdentity{
		DatasetID:   datasetID,
		CaptureDate: captureDate,
		DatasetType: datasetType,
		Files:       sorted,
	}
	compact, err := jsonMarshalNoEscape(id, "")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(compact)
	return "raw-" + hex.EncodeToString(sum[:])[:32], nil
}

func MakeManifest(ds *DatasetMeta, files []ManifestFile) (Manifest, error) {
	sorted := make([]ManifestFile, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool {
		return UTF16Compare(sorted[i].Path, sorted[j].Path) < 0
	})
	rev, err := manifestRevision(ds.Name, ds.CaptureDate, ds.Type, sorted)
	if err != nil {
		return Manifest{}, err
	}
	now := time.Now().UTC()
	createdAt := fmt.Sprintf("%s.%03dZ", now.Format("2006-01-02T15:04:05"), now.Nanosecond()/1e6)
	return Manifest{
		SchemaVersion: 1,
		DatasetID:     ds.Name,
		CaptureDate:   ds.CaptureDate,
		DatasetType:   ds.Type,
		Files:         sorted,
		Revision:      rev,
		CreatedAt:     createdAt,
	}, nil
}

func SameContent(a, b Manifest) bool {
	if a.SchemaVersion != 1 || a.DatasetID != b.DatasetID || a.CaptureDate != b.CaptureDate || a.DatasetType != b.DatasetType {
		return false
	}
	if len(a.Files) != len(b.Files) {
		return false
	}
	for i := range a.Files {
		if a.Files[i] != b.Files[i] {
			return false
		}
	}
	return true
}

func RemoteCommit(oss OSSClient, rawURI string) (*CommitInfo, error) {
	readyBytes, ok, err := oss.ReadObject(rawURI + "_READY")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	manifestBytes, mok, err := oss.ReadObject(rawURI + "manifest.json")
	if err != nil {
		return nil, err
	}
	if !mok {
		return nil, NewUploadError("远端已有 _READY 但缺少清单，拒绝覆盖: %s", rawURI)
	}
	var ready ReadyMarker
	if err := json.Unmarshal(readyBytes, &ready); err != nil {
		return nil, NewUploadError("远端 _READY 格式损坏: %s", rawURI)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, NewUploadError("远端 manifest.json 格式损坏: %s", rawURI)
	}
	if manifest.SchemaVersion != 1 {
		return nil, NewUploadError("远端 manifest.json schema_version 不受支持: %s", rawURI)
	}
	expectedRevision, err := manifestRevision(manifest.DatasetID, manifest.CaptureDate, manifest.DatasetType, manifest.Files)
	if err != nil {
		return nil, err
	}
	if manifest.Revision == "" || manifest.Revision != expectedRevision {
		return nil, NewUploadError("远端 manifest.json revision 与清单内容不一致: %s", rawURI)
	}
	mSum := sha256.Sum256(manifestBytes)
	mHex := hex.EncodeToString(mSum[:])
	if ready.SchemaVersion != 1 || ready.Revision != manifest.Revision || ready.ManifestSHA256 != mHex {
		return nil, NewUploadError("远端完成标记与清单不一致，拒绝覆盖: %s", rawURI)
	}
	return &CommitInfo{Ready: ready, Manifest: manifest}, nil
}

func VerifyInventory(oss OSSClient, rawURI string, files []ManifestFile) error {
	objs, err := oss.ListObjects(rawURI)
	if err != nil {
		return err
	}
	byName := make(map[string]int64, len(objs))
	remoteCount := 0
	for _, f := range objs {
		if IsExcluded(f.Name) {
			continue
		}
		remoteCount++
		byName[f.Name] = f.Size
	}
	if remoteCount != len(files) {
		return NewUploadError("远端文件列表或大小与本地清单不一致；未发送完成标记，请检查残留文件后重试: %s", rawURI)
	}
	for _, f := range files {
		if sz, exists := byName[f.Path]; !exists || sz != f.Size {
			return NewUploadError("远端文件列表或大小与本地清单不一致；未发送完成标记，请检查残留文件后重试: %s", rawURI)
		}
	}
	return nil
}

// Session discovery matching upload_umi_sdk.py `find_sessions` and `discover`.

type RouteSpec struct {
	Type string
	Path string
	City string
	Room string
}

type DiscoveredSession struct {
	Source  string
	Entries []os.DirEntry
	Fmt     string
}

func FindSessions(root string) ([]DiscoveredSession, []string) {
	var sessions []DiscoveredSession
	var issues []string

	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			issues = append(issues, fmt.Sprintf("无法读取目录 %s: %v", dir, err))
			return
		}
		name := filepath.Base(dir)
		if fmtName := FmtOfName(name); fmtName != "" {
			sessions = append(sessions, DiscoveredSession{
				Source:  dir,
				Entries: entries,
				Fmt:     fmtName,
			})
			return
		}
		sort.Slice(entries, func(i, j int) bool {
			return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
		})
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			en := e.Name()
			if strings.HasPrefix(en, ".") || strings.HasPrefix(en, "$") || en == "_pipeline" || SkipDirs[strings.ToLower(en)] {
				continue
			}
			walk(filepath.Join(dir, en))
		}
	}
	walk(root)
	return sessions, issues
}

func FindRoom(source string, fallbackCity, fallbackRoom string) (string, string, error) {
	_, parts := splitPathComponents(source)
	for i := len(parts) - 2; i >= 0; i-- {
		if m := PRTRe.FindStringSubmatch(parts[i]); m != nil {
			return strings.ToUpper(m[1]), m[2], nil
		}
	}
	city := reNonAZ.ReplaceAllString(strings.ToUpper(fallbackCity), "")
	room := reNonDig.ReplaceAllString(fallbackRoom, "")
	if city != "" && room != "" {
		return city, room, nil
	}
	return "", "", NewUploadError("路径里没有 PRT_城市_room房间号 目录，请在界面同时填写城市和房间号")
}

func Discover(routes []RouteSpec, cfg map[string]string) ([]*DatasetMeta, []string) {
	if cfg == nil {
		cfg = ActiveCfg()
	}
	var datasets []*DatasetMeta
	var problems []string
	seen := map[string]string{}
	claimed := map[string]string{}

	descParts := make([]string, 0, len(FormatOrder))
	for _, k := range FormatOrder {
		sp := Formats[k]
		descParts = append(descParts, fmt.Sprintf("%s 格式：%s 目录，需带 %s", sp.Label, sp.Token, sp.Desc))
	}
	emptyDesc := strings.Join(descParts, " 或 ")

	for _, job := range routes {
		absRoot, err := filepath.Abs(job.Path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s 文件夹无效: %s", job.Type, job.Path))
			continue
		}
		st, err := os.Stat(absRoot)
		if err != nil || !st.IsDir() {
			problems = append(problems, fmt.Sprintf("%s 文件夹不存在: %s", job.Type, absRoot))
			continue
		}
		sessions, issues := FindSessions(absRoot)
		problems = append(problems, issues...)
		if len(sessions) == 0 {
			problems = append(problems, fmt.Sprintf("%s 文件夹里没有找到数据集（%s）: %s", job.Type, emptyDesc, absRoot))
			continue
		}
		for _, s := range sessions {
			name := filepath.Base(s.Source)
			spec := Formats[s.Fmt]
			err := func() error {
				if err := SafePart(name); err != nil {
					return err
				}
				if err := ValidateSession(s.Fmt, s.Source, s.Entries); err != nil {
					return err
				}
				capDate := SessionDate(name, s.Fmt)
				yymmdd, err := ToYYMMDD(capDate)
				if err != nil {
					return err
				}
				if prevType, ok := claimed[s.Source]; ok && prevType != job.Type {
					return NewUploadError("同一个数据集同时出现在 normal 和 calibration 文件夹里，请检查路线")
				}
				claimed[s.Source] = job.Type

				var city, room string
				if job.Room != "" {
					city = reNonAZ.ReplaceAllString(strings.ToUpper(job.City), "")
					room = reNonDig.ReplaceAllString(job.Room, "")
					if city == "" || room == "" {
						return NewUploadError("路线里的 city / room 无效（城市要字母缩写，房间号要数字）")
					}
				} else {
					var rerr error
					city, room, rerr = FindRoom(s.Source, job.City, job.Room)
					if rerr != nil {
						return rerr
					}
				}

				prt := fmt.Sprintf("PRT_%s_room%s", city, room)
				bucket := BucketFor(s.Fmt, cfg)
				rawURI := fmt.Sprintf("oss://%s/raw_data/raw_data_%s/%s/%s/%s/", bucket, yymmdd, TypeSubdir[job.Type], prt, name)
				if prevSrc, ok := seen[rawURI]; ok {
					if prevSrc != s.Source {
						return NewUploadError("不同本地目录指向同一 OSS 数据集: %s 与 %s", prevSrc, s.Source)
					}
					return nil
				}
				seen[rawURI] = s.Source
				datasets = append(datasets, &DatasetMeta{
					Source:      s.Source,
					Name:        name,
					Type:        job.Type,
					CaptureDate: capDate,
					RawURI:      rawURI,
					Fmt:         s.Fmt,
					Bucket:      bucket,
					Conf:        spec.Conf,
				})
				return nil
			}()
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %s", s.Source, err.Error()))
			}
		}
	}
	return datasets, problems
}
