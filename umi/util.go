package umi

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

var (
	RoomOKRe  = regexp.MustCompile(`^PRT_[A-Za-z]+_room\d{3}$`)
	rePadRoom = regexp.MustCompile(`(_room)(\d{1,2})(/|$)`)
	reRelURI  = regexp.MustCompile(`^oss://[^/]+/raw_data/raw_data_\d{6}/(.+?)/$`)
	reNonAZ   = regexp.MustCompile(`[^A-Z]`)
	reNonDig  = regexp.MustCompile(`\D`)
)

// PadRoom zero-pads 1- or 2-digit room numbers to 3 digits (PRT_WX_room2 -> PRT_WX_room002).
func PadRoom(rel string) string {
	return rePadRoom.ReplaceAllStringFunc(rel, func(m string) string {
		sub := rePadRoom.FindStringSubmatch(m)
		if len(sub) != 4 {
			return m
		}
		num := sub[2]
		for len(num) < 3 {
			num = "0" + num
		}
		return sub[1] + num + sub[3]
	})
}

// ReadText reads a text file trying UTF-8 -> GB18030 -> Latin-1.
func ReadText(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return DecodeTextBytes(raw), nil
}

func DecodeTextBytes(raw []byte) string {
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		raw = raw[3:]
	}
	if utf8.Valid(raw) {
		return string(raw)
	}
	rd := transform.NewReader(strings.NewReader(string(raw)), simplifiedchinese.GB18030.NewDecoder())
	if decoded, err := io.ReadAll(rd); err == nil && utf8.Valid(decoded) {
		return string(decoded)
	}
	runes := make([]rune, len(raw))
	for i, b := range raw {
		runes[i] = rune(b)
	}
	return string(runes)
}

func FmtTS(sec float64) string {
	if sec <= 0 {
		return ""
	}
	ns := int64(sec * 1e9)
	return time.Unix(0, ns).In(TZ).Format("2006-01-02 15:04:05")
}

func NowTS() string {
	return time.Now().In(TZ).Format("2006-01-02 15:04:05")
}

func RelOf(rawURI string) string {
	m := reRelURI.FindStringSubmatch(rawURI)
	if m != nil {
		return m[1]
	}
	return ""
}

func CapOf(name string) (string, error) {
	capDate := SessionDate(name, "")
	if capDate == "" {
		return "", NewUploadError("会话目录名 %s 不是 ISuit-V3_xxx_日期-时间 或 UMI_v3_日期_时间 格式，请重新扫描", name)
	}
	return capDate, nil
}

func BucketFor(fmtName string, cfg map[string]string) string {
	if cfg == nil {
		cfg = ActiveCfg()
	}
	if fmtName == "umi_v3" {
		if b := strings.TrimSpace(cfg["bucket_umi"]); b != "" {
			return b
		}
		return SDKDefaults["bucket_umi"]
	}
	if b := strings.TrimSpace(cfg["bucket"]); b != "" {
		return b
	}
	return SDKDefaults["bucket"]
}

func FullURI(rel string, cfg map[string]string) (string, error) {
	parts := strings.Split(rel, "/")
	name := parts[len(parts)-1]
	capDate, err := CapOf(name)
	if err != nil {
		return "", err
	}
	yymmdd, err := ToYYMMDD(capDate)
	if err != nil {
		return "", err
	}
	bkt := BucketFor(FmtOfName(name), cfg)
	return fmt.Sprintf("oss://%s/raw_data/raw_data_%s/%s/", bkt, yymmdd, rel), nil
}

type DatasetMeta struct {
	Source      string
	Name        string
	Type        string
	CaptureDate string
	RawURI      string
	Fmt         string
	Bucket      string
	Conf        string

	// entries are retained from discovery so inspection can reuse the dataset
	// root listing instead of reading that directory a second time.
	entries []os.DirEntry
}

func MakeDS(rec *DatasetRecord, cfg map[string]string) (*DatasetMeta, error) {
	if cfg == nil {
		cfg = ActiveCfg()
	}
	fmtName := FmtOfName(rec.Name)
	if fmtName == "" {
		fmtName = "isuit"
	}
	capDate, err := CapOf(rec.Name)
	if err != nil {
		return nil, err
	}
	rawURI, err := FullURI(rec.Rel, cfg)
	if err != nil {
		return nil, err
	}
	return &DatasetMeta{
		Source:      rec.Source,
		Name:        rec.Name,
		Type:        rec.Type,
		CaptureDate: capDate,
		RawURI:      rawURI,
		Fmt:         fmtName,
		Bucket:      BucketFor(fmtName, cfg),
		Conf:        Formats[fmtName].Conf,
	}, nil
}

// TagOf returns the short time suffix used in concurrent upload log lines (6 chars for UMI_v3, 8 chars for ISuit-V3).
func TagOf(name string) string {
	if UmiV3Re.MatchString(name) {
		if len(name) >= 6 {
			return name[len(name)-6:]
		}
		return name
	}
	if len(name) >= 8 {
		return name[len(name)-8:]
	}
	return name
}

// EffectiveOrder returns the Go SDK upload order: "folder" | "slice" | "pipeline".
func EffectiveOrder(cfg map[string]string) string {
	if cfg == nil {
		cfg = ActiveCfg()
	}
	if cfg["method"] == "ossutil" {
		if cfg["ossutil_unit"] == "session" {
			return "pipeline"
		}
		return "slice"
	}
	switch cfg["order"] {
	case "pipeline":
		return "pipeline"
	case "slice":
		return "slice"
	default:
		return "folder"
	}
}

func MLabel(cfg map[string]string) string {
	switch EffectiveOrder(cfg) {
	case "pipeline":
		return "sdk·流水线"
	case "slice":
		return "sdk·逐分片"
	default:
		return "sdk·逐文件夹"
	}
}

// splitPathComponents splits both Windows (`\`) and Unix (`/`) paths cleanly while preserving root/drive prefix.
func splitPathComponents(p string) (prefix string, parts []string) {
	if p == "" {
		return "", nil
	}
	if len(p) >= 2 && p[1] == ':' {
		sep := `\`
		if strings.Contains(p, "/") && !strings.Contains(p, `\`) {
			sep = "/"
		}
		prefix = p[:2] + sep
		rest := strings.TrimLeft(p[2:], `\/`)
		if rest == "" {
			return prefix, nil
		}
		parts = fieldsBySep(rest)
		return prefix, parts
	}
	if strings.HasPrefix(p, `\\`) {
		prefix = `\\`
		rest := strings.TrimLeft(p[2:], `\`)
		return prefix, fieldsBySep(rest)
	}
	if strings.HasPrefix(p, "/") {
		prefix = "/"
		rest := strings.TrimLeft(p[1:], "/")
		if rest == "" {
			return prefix, nil
		}
		return prefix, fieldsBySep(rest)
	}
	return "", fieldsBySep(p)
}

func fieldsBySep(s string) []string {
	var out []string
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '/' || s[i] == '\\' {
			if start >= 0 {
				seg := s[start:i]
				if seg != "" && seg != "." {
					out = append(out, seg)
				}
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		seg := s[start:]
		if seg != "" && seg != "." {
			out = append(out, seg)
		}
	}
	return out
}

func joinPathComponents(prefix string, parts []string, originalPath string) string {
	sep := string(os.PathSeparator)
	if strings.Contains(originalPath, `\`) && !strings.Contains(originalPath, "/") {
		sep = `\`
	} else if strings.Contains(originalPath, "/") && !strings.Contains(originalPath, `\`) {
		sep = "/"
	}
	joined := strings.Join(parts, sep)
	if prefix == "" {
		return joined
	}
	if strings.HasSuffix(prefix, `\`) || strings.HasSuffix(prefix, "/") {
		return prefix + joined
	}
	return prefix + sep + joined
}
