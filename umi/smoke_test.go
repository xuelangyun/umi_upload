package umi

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockOSSServer implements an in-process Alibaba Cloud OSS / S3-compatible HTTP server
// supporting PutObject, GetObject, ListObjectsV2, InitiateMultipartUpload, UploadPart,
// ListParts, CompleteMultipartUpload, AbortMultipartUpload, write order logging, and simulated network outages.
type mockOSSServer struct {
	mu          sync.Mutex
	objects     map[string]map[string][]byte           // bucket -> key -> data
	uploads     map[string]map[int][]byte              // uploadId -> partNum -> data
	uploadMeta  map[string]struct{ bucket, key string }
	writeLog    []string                               // "<bucket>/<key>" in exact commit order
	partUploads int64                                  // total UploadPart calls succeeded
	putSkips    map[string]int                         // bucket/key -> PutObject count
	nextUpID       int
	netMode        string // "" = healthy, "all" = all ops fail 503, "put_after_1" = fail writes & probes after 1 write
	writesSince    int
	requireV4      bool
	sigV4Requests  int
}

func newMockOSSServer() *mockOSSServer {
	return &mockOSSServer{
		objects:    map[string]map[string][]byte{},
		uploads:    map[string]map[int][]byte{},
		uploadMeta: map[string]struct{ bucket, key string }{},
		putSkips:   map[string]int{},
	}
}

func (m *mockOSSServer) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects = map[string]map[string][]byte{}
	m.uploads = map[string]map[int][]byte{}
	m.uploadMeta = map[string]struct{ bucket, key string }{}
	m.writeLog = nil
	m.putSkips = map[string]int{}
	atomic.StoreInt64(&m.partUploads, 0)
}

func (m *mockOSSServer) setNetMode(mode string) {
	m.mu.Lock()
	m.netMode = mode
	m.writesSince = 0
	m.mu.Unlock()
}

func (m *mockOSSServer) clearBucket(bucket string) {
	m.mu.Lock()
	delete(m.objects, bucket)
	m.mu.Unlock()
}

func (m *mockOSSServer) deleteKeySuffix(bucket, suffix string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.objects[bucket] {
		if strings.HasSuffix(k, suffix) {
			delete(m.objects[bucket], k)
			return
		}
	}
}

func (m *mockOSSServer) listBucketKeys(bucket string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ks []string
	for k := range m.objects[bucket] {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func (m *mockOSSServer) getObjectBytes(bucket, key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[bucket][key]
	if !ok {
		return nil, false
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp, true
}

func (m *mockOSSServer) writes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]string, len(m.writeLog))
	copy(cp, m.writeLog)
	return cp
}

func (m *mockOSSServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Path-style URL: /{bucket} or /{bucket}/{key...}
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	var bucket, key string
	if idx := strings.IndexByte(trimmed, '/'); idx >= 0 {
		bucket = trimmed[:idx]
		key = trimmed[idx+1:]
	} else {
		bucket = trimmed
	}
	q := r.URL.Query()

	m.mu.Lock()
	mode := m.netMode
	isProbe := r.Method == http.MethodGet && key == "" && q.Get("max-keys") == "1"
	isWrite := r.Method == http.MethodPut || r.Method == http.MethodPost
	shouldFailNet := false
	switch mode {
	case "all":
		shouldFailNet = true
	case "heal_on_probe":
		if isProbe {
			m.writesSince++
			if m.writesSince >= 2 {
				m.netMode = ""
				shouldFailNet = false
			} else {
				shouldFailNet = true
			}
		} else {
			shouldFailNet = true
		}
	case "put_after_1":
		if isProbe {
			shouldFailNet = m.writesSince >= 1
		} else if isWrite {
			if m.writesSince >= 1 {
				shouldFailNet = true
			}
		}
	}
	if m.requireV4 {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=test-ak/") {
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>InvalidRequest</Code><Message>AWS Signature V4 required</Message></Error>`))
			return
		}
		m.sigV4Requests++
	}
	m.mu.Unlock()

	if shouldFailNet {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>ServiceUnavailable</Code><Message>simulated network outage</Message></Error>`))
		return
	}

	// BucketExists: HEAD /{bucket}
	if r.Method == http.MethodHead && key == "" {
		m.mu.Lock()
		_, exists := m.objects[bucket]
		m.mu.Unlock()
		if !exists {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchBucket</Code></Error>`))
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	// ListObjectsV2: GET /{bucket}?list-type=2
	if r.Method == http.MethodGet && key == "" {
		prefix := q.Get("prefix")
		maxKeys := 1000
		if mk, err := strconv.Atoi(q.Get("max-keys")); err == nil && mk > 0 {
			maxKeys = mk
		}

		m.mu.Lock()
		bmap := m.objects[bucket]
		var keys []string
		for k := range bmap {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		truncated := false
		if len(keys) > maxKeys {
			keys = keys[:maxKeys]
			truncated = true
		}
		var sb strings.Builder
		sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`)
		fmt.Fprintf(&sb, "<Name>%s</Name><Prefix>%s</Prefix><MaxKeys>%d</MaxKeys><IsTruncated>%v</IsTruncated>",
			xmlEsc(bucket), xmlEsc(prefix), maxKeys, truncated)
		for _, k := range keys {
			data := bmap[k]
			fmt.Fprintf(&sb, "<Contents><Key>%s</Key><Size>%d</Size><ETag>\"etag\"</ETag><LastModified>2026-11-08T10:00:00.000Z</LastModified></Contents>",
				xmlEsc(k), len(data))
		}
		sb.WriteString(`</ListBucketResult>`)
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sb.String()))
		return
	}

	// InitiateMultipartUpload: POST /{bucket}/{key}?uploads
	if r.Method == http.MethodPost && q.Has("uploads") {
		m.mu.Lock()
		m.nextUpID++
		upID := fmt.Sprintf("up-%d", m.nextUpID)
		m.uploads[upID] = map[int][]byte{}
		m.uploadMeta[upID] = struct{ bucket, key string }{bucket: bucket, key: key}
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`,
			xmlEsc(bucket), xmlEsc(key), upID)
		return
	}

	// UploadPart: PUT /{bucket}/{key}?partNumber=N&uploadId=ID
	if r.Method == http.MethodPut && q.Get("uploadId") != "" {
		upID := q.Get("uploadId")
		partNum, _ := strconv.Atoi(q.Get("partNumber"))
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		sum := md5.Sum(body)
		etag := `"` + hex.EncodeToString(sum[:]) + `"`

		m.mu.Lock()
		if m.uploads[upID] == nil {
			m.uploads[upID] = map[int][]byte{}
		}
		m.uploads[upID][partNum] = body
		m.writesSince++
		m.mu.Unlock()
		atomic.AddInt64(&m.partUploads, 1)

		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
		return
	}

	// ListParts: GET /{bucket}/{key}?uploadId=ID
	if r.Method == http.MethodGet && q.Get("uploadId") != "" {
		upID := q.Get("uploadId")
		m.mu.Lock()
		partsMap := m.uploads[upID]
		var pnums []int
		for pn := range partsMap {
			pnums = append(pnums, pn)
		}
		sort.Ints(pnums)
		var sb strings.Builder
		sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListPartsResult>`)
		fmt.Fprintf(&sb, "<Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId><IsTruncated>false</IsTruncated>",
			xmlEsc(bucket), xmlEsc(key), upID)
		for _, pn := range pnums {
			b := partsMap[pn]
			sum := md5.Sum(b)
			etag := `"` + hex.EncodeToString(sum[:]) + `"`
			fmt.Fprintf(&sb, "<Part><PartNumber>%d</PartNumber><LastModified>2026-11-08T10:00:00.000Z</LastModified><ETag>%s</ETag><Size>%d</Size></Part>",
				pn, xmlEsc(etag), len(b))
		}
		sb.WriteString(`</ListPartsResult>`)
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sb.String()))
		return
	}

	// CompleteMultipartUpload: POST /{bucket}/{key}?uploadId=ID
	if r.Method == http.MethodPost && q.Get("uploadId") != "" {
		upID := q.Get("uploadId")
		type xmlPart struct {
			PartNumber int `xml:"PartNumber"`
		}
		type completeReq struct {
			Parts []xmlPart `xml:"Part"`
		}
		rawBody, _ := io.ReadAll(r.Body)
		var creq completeReq
		_ = xml.Unmarshal(rawBody, &creq)

		m.mu.Lock()
		partsMap := m.uploads[upID]
		var pnums []int
		if len(creq.Parts) > 0 {
			for _, p := range creq.Parts {
				pnums = append(pnums, p.PartNumber)
			}
		} else {
			for pn := range partsMap {
				pnums = append(pnums, pn)
			}
			sort.Ints(pnums)
		}
		var assembled bytes.Buffer
		for _, pn := range pnums {
			assembled.Write(partsMap[pn])
		}
		if m.objects[bucket] == nil {
			m.objects[bucket] = map[string][]byte{}
		}
		m.objects[bucket][key] = assembled.Bytes()
		m.writeLog = append(m.writeLog, bucket+"/"+key)
		delete(m.uploads, upID)
		delete(m.uploadMeta, upID)
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("ETag", `"multipart-etag"`)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>"multipart-etag"</ETag></CompleteMultipartUploadResult>`,
			xmlEsc(bucket), xmlEsc(key))
		return
	}

	// AbortMultipartUpload: DELETE /{bucket}/{key}?uploadId=ID
	if r.Method == http.MethodDelete && q.Get("uploadId") != "" {
		upID := q.Get("uploadId")
		m.mu.Lock()
		delete(m.uploads, upID)
		delete(m.uploadMeta, upID)
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// PutObject: PUT /{bucket}/{key}
	if r.Method == http.MethodPut {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		sum := md5.Sum(body)
		etag := `"` + hex.EncodeToString(sum[:]) + `"`

		m.mu.Lock()
		if m.objects[bucket] == nil {
			m.objects[bucket] = map[string][]byte{}
		}
		m.objects[bucket][key] = body
		m.writeLog = append(m.writeLog, bucket+"/"+key)
		m.putSkips[bucket+"/"+key]++
		m.writesSince++
		m.mu.Unlock()

		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
		return
	}

	// GetObject / HeadObject
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		m.mu.Lock()
		data, ok := m.objects[bucket][key]
		m.mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`))
			}
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"etag"`)
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
		return
	}

	w.WriteHeader(http.StatusMethodNotAllowed)
}

func xmlEsc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Helper functions mirroring sample/tests/smoke.py

func makeISuitSession(t *testing.T, root, typ, folder, prt, name, operator string, slices int, t0 int64, sn, dev string) string {
	t.Helper()
	parts := []string{root, typ}
	if dev != "" {
		parts = append(parts, dev)
	}
	parts = append(parts, folder, prt, name)
	s := filepath.Join(parts...)
	if err := os.MkdirAll(s, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := fmt.Sprintf("device_SN: %s\nrecord_purpose: Normal\nrecord_operator: %s\nrecord_location: 测试地点\ntask_name: 测试任务\nsub_task: 子任务A\n", sn, operator)
	if err := os.WriteFile(filepath.Join(s, "datacollect_conf.yaml"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("ISUITDATA"), 4000)
	for i := 0; i < slices; i++ {
		d := filepath.Join(s, fmt.Sprintf("%s_%d", name, i))
		_ = os.MkdirAll(filepath.Join(d, "camera"), 0o755)
		_ = os.MkdirAll(filepath.Join(d, "imu"), 0o755)
		yml := fmt.Sprintf("head_time: %d.0\ntail_time: %d.0\n", t0+int64(30*i), t0+int64(30*i+30))
		_ = os.WriteFile(filepath.Join(d, fmt.Sprintf("%s_%d.yaml", name, i)), []byte(yml), 0o644)
		for _, fn := range []string{"a", "b", "c"} {
			_ = os.WriteFile(filepath.Join(d, "camera", fn+".record"), payload, 0o644)
		}
		_ = os.WriteFile(filepath.Join(d, "imu", "l.record"), []byte("imudata"), 0o644)
	}
	return s
}

type umiMakeOpts struct {
	Operator string
	Conf     *string
	Skip     map[string]bool
	RootFile bool
	Log      *string
	Frames   *[2]float64
	Videos   []string
	Dev      string
	HasDev   bool
}

func makeUMISession(t *testing.T, root, typ, folder, prt, name string, opt umiMakeOpts) string {
	t.Helper()
	if opt.Operator == "" {
		opt.Operator = "zhangsan"
	}
	parts := []string{root, typ}
	if opt.HasDev && opt.Dev != "" {
		parts = append(parts, opt.Dev)
	}
	parts = append(parts, folder, prt, name)
	s := filepath.Join(parts...)
	_ = os.MkdirAll(s, 0o755)

	subSpec := []struct {
		sub   string
		files []string
	}{
		{"head", []string{"a.bin", "b.bin"}},
		{"left hand", []string{"l.bin"}},
		{"right hand", []string{"r.bin"}},
		{"metadata", nil},
	}
	binData := bytes.Repeat([]byte("UMIBIN"), 3000)
	for _, sp := range subSpec {
		if opt.Skip[sp.sub] {
			continue
		}
		_ = os.MkdirAll(filepath.Join(s, sp.sub), 0o755)
		for _, f := range sp.files {
			_ = os.WriteFile(filepath.Join(s, sp.sub, f), binData, 0o644)
		}
	}
	if !opt.Skip["metadata/conf.yaml"] {
		_ = os.MkdirAll(filepath.Join(s, "metadata"), 0o755)
		confText := fmt.Sprintf("device_ID: UMI_v3\ndevice_SN: 01351CA8\nrecord_operator: %s\nrecord_location: 测试地点\ntask_name: 测试项目\ntask_title: 测试任务\nsub_task:\n  - 子任务A\n  - 子任务B\npayload: {task: {duration_seconds: 360}}\n", opt.Operator)
		if opt.Conf != nil {
			confText = *opt.Conf
		}
		_ = os.WriteFile(filepath.Join(s, "metadata", "conf.yaml"), []byte(confText), 0o644)
		if opt.Log != nil {
			_ = os.WriteFile(filepath.Join(s, "metadata", "recording.log"), []byte(*opt.Log), 0o644)
		}
	}
	if opt.Frames != nil {
		var rows []string
		rows = append(rows, "frame_index,sync_id,exposure_ns")
		f0, f1 := opt.Frames[0], opt.Frames[1]
		for k := 0; k < 10; k++ {
			ns := int64((f0 + (f1-f0)*float64(k)/9.0) * 1e9)
			rows = append(rows, fmt.Sprintf("%d,%d,%d", k, k+1, ns))
		}
		_ = os.WriteFile(filepath.Join(s, "head", "camera-hl-head.frames.csv"), []byte(strings.Join(rows, "\n")+"\n"), 0o644)
	}
	vidBytes := make([]byte, 20480)
	for i := range vidBytes {
		vidBytes[i] = byte(i % 256)
	}
	for _, v := range opt.Videos {
		vp := filepath.Join(s, filepath.FromSlash(v))
		_ = os.MkdirAll(filepath.Dir(vp), 0o755)
		_ = os.WriteFile(vp, vidBytes, 0o644)
	}
	if opt.RootFile {
		_ = os.WriteFile(filepath.Join(s, "session.json"), []byte("{}"), 0o644)
	}
	return s
}

func makeRecLog(start string, secs float64) string {
	t0, _ := time.ParseInLocation("2006-01-02 15:04:05", start, TZ)
	fmtTime := func(d time.Time) string {
		return fmt.Sprintf("%s.%03d +0800", d.Format("2006-01-02 15:04:05"), d.Nanosecond()/1e6)
	}
	tStart := t0.Add(205 * time.Millisecond)
	tStop := tStart.Add(time.Duration(secs * float64(time.Second)))
	tEnd := t0.Add(time.Duration((secs + 1) * float64(time.Second)))
	return strings.Join([]string{
		fmt.Sprintf("%s INFO 录制日志创建：dataset=x", fmtTime(t0)),
		fmt.Sprintf("%s INFO 录制开始（操作计时起点，不含按键倒计时及启动准备）", fmtTime(tStart)),
		fmt.Sprintf("%s INFO 停止录制：原因=用户停止请求", fmtTime(tStop)),
		fmt.Sprintf("%s INFO 保存结束：结果=成功 操作录制时长秒=%v 停止后保存耗时秒=0.139016", fmtTime(tEnd), secs),
	}, "\n") + "\n"
}

type apiClient struct {
	t       *testing.T
	handler http.Handler
}

func (c *apiClient) req(method, target string, body any, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, target, rdr)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	} else {
		req.RemoteAddr = "127.0.0.1:54321"
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)
	return w
}

func (c *apiClient) getJSON(target string) (int, map[string]any) {
	w := c.req(http.MethodGet, target, nil, "", nil)
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func (c *apiClient) postJSON(target string, body any) (int, map[string]any) {
	w := c.req(http.MethodPost, target, body, "", nil)
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func (c *apiClient) waitScan() {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_, st := c.getJSON("/api/state?light=1")
		sc, _ := st["scan"].(map[string]any)
		if sc != nil && sc["running"] == false {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	c.t.Fatal("scan timed out")
}

func (c *apiClient) waitJob() map[string]any {
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		_, st := c.getJSON("/api/state?light=1")
		j, _ := st["job"].(map[string]any)
		if j != nil && j["done"] == true {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatal("job timed out")
	return nil
}

func TestFullSmokeSuite(t *testing.T) {
	tmp := t.TempDir()
	DBPath = filepath.Join(tmp, "test.db")
	CkptDir = filepath.Join(tmp, "ckpt")
	ProbeMin = 0.15
	ProbeMax = 0.30
	defer CloseDB()

	// Production endpoint routing must distinguish AWS S3 V4 from Alibaba OSS.
	probeCfg := OSSParams{StorageType: "aliyun", AK: "test-ak", SK: "test-sk", Bucket: "probe-bucket", Parallel: 1}
	// Explicit S3 selection uses AWS V4 even when the endpoint is a private IP.
	s3ProbeCfg := probeCfg
	s3ProbeCfg.StorageType = "s3"
	s3ProbeCfg.Endpoint = "http://127.0.0.1:9000"
	s3Probe, routeErr := NewOSSClientFunc(s3ProbeCfg)
	if routeErr != nil {
		t.Fatalf("create S3 endpoint client: %v", routeErr)
	}
	if _, ok := s3Probe.(*S3CompatibleClient); !ok {
		t.Fatalf("custom endpoint must select S3 Signature V4 client, got %T", s3Probe)
	}
	aliyunProbeCfg := probeCfg
	aliyunProbeCfg.Endpoint = "https://oss-cn-hangzhou.aliyuncs.com"
	aliyunProbe, routeErr := NewOSSClientFunc(aliyunProbeCfg)
	if routeErr != nil {
		t.Fatalf("create Alibaba OSS endpoint client: %v", routeErr)
	}
	if _, ok := aliyunProbe.(*AliyunOSSClient); !ok {
		t.Fatalf("Alibaba OSS endpoint must select Aliyun SDK client, got %T", aliyunProbe)
	}
	// An Aliyun custom CNAME must remain on the Alibaba SDK; protocol is not inferred from the hostname.
	cnameCfg := probeCfg
	cnameCfg.Endpoint = "https://oss-cname.example.com"
	cnameProbe, routeErr := NewOSSClientFunc(cnameCfg)
	if routeErr != nil {
		t.Fatalf("create Aliyun CNAME client: %v", routeErr)
	}
	if _, ok := cnameProbe.(*AliyunOSSClient); !ok {
		t.Fatalf("Aliyun custom CNAME must select Aliyun SDK, got %T", cnameProbe)
	}

	mockOSS := newMockOSSServer()
	ossSrv := httptest.NewServer(mockOSS)
	defer ossSrv.Close()

	var seenBucketsMu sync.Mutex
	seenBuckets := map[string]bool{}
	NewOSSClientFunc = func(p OSSParams) (OSSClient, error) {
		seenBucketsMu.Lock()
		seenBuckets[p.Bucket] = true
		seenBucketsMu.Unlock()
		return NewAliyunOSSClient(p)
	}

	repoRoot := filepath.Clean("..")
	webDir := filepath.Join(repoRoot, "web")
	cli := &apiClient{
		t:       t,
		handler: NewHandler(os.DirFS(webDir)),
	}

	// ---- 1. Database creation & v4 -> v6 migration ----
	if err := InitDB(); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	_ = SetSetting("endpoint", ossSrv.URL)
	_ = SetSetting("ak", "test-ak")
	_ = SetSetting("sk", "test-sk")
	ApplyCfg()

	db, err := OpenDB()
	if err != nil {
		t.Fatal(err)
	}
	var jm string
	var uv int
	_ = db.QueryRow("PRAGMA journal_mode").Scan(&jm)
	_ = db.QueryRow("PRAGMA user_version").Scan(&uv)
	if strings.ToLower(jm) != "wal" || uv != 7 {
		t.Fatalf("expected WAL + user_version=7, got %s / %d", jm, uv)
	}

	// Test v4 -> v6 migration preserving uploads & settings and backfilling cap_date & room
	v4Path := filepath.Join(tmp, "v4.db")
	v4Conn, err := sql.Open("sqlite3", v4Path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = v4Conn.Exec(`
		CREATE TABLE settings(k TEXT PRIMARY KEY, v TEXT);
		CREATE TABLE roots(path TEXT PRIMARY KEY);
		CREATE TABLE datasets(rel TEXT PRIMARY KEY, data TEXT, scanned_at TEXT);
		CREATE TABLE uploads(id INTEGER PRIMARY KEY AUTOINCREMENT, rel TEXT, oss_url TEXT, source TEXT, device TEXT, dir_date TEXT, task TEXT, operator TEXT, location TEXT, purpose TEXT, type TEXT, start_time TEXT, end_time TEXT, slices INTEGER, size INTEGER, duration REAL, files INTEGER, status TEXT, method TEXT, revision TEXT, elapsed INTEGER, avg_speed REAL, error TEXT, created_at TEXT);
		CREATE TABLE checks(rel TEXT PRIMARY KEY, status TEXT, ready INTEGER, deep INTEGER, detail TEXT, checked_at TEXT);
		INSERT INTO uploads(rel,status) VALUES('umi_normal/PRT_WX_room002/ISuit-V3_2026-11-08-10-30-00','成功');
		INSERT INTO settings VALUES('bucket','keepme');
		PRAGMA user_version=4;
	`)
	if err != nil {
		t.Fatalf("seed v4 db: %v", err)
	}
	_ = v4Conn.Close()

	keepDB := DBPath
	DBPath = v4Path
	if err := InitDB(); err != nil {
		t.Fatalf("v4->v6 migration failed: %v", err)
	}
	migDB, _ := OpenDB()
	var upCnt int
	var bktVal, backCap, backRoom string
	_ = migDB.QueryRow("SELECT COUNT(*) FROM uploads").Scan(&upCnt)
	_ = migDB.QueryRow("SELECT v FROM settings WHERE k='bucket'").Scan(&bktVal)
	_ = migDB.QueryRow("SELECT cap_date, room FROM uploads LIMIT 1").Scan(&backCap, &backRoom)
	if upCnt != 1 || bktVal != "keepme" || backCap != "2026-11-08" || backRoom != "PRT_WX_room002" {
		t.Fatalf("v4->v6 migration mismatch: cnt=%d bucket=%s cap=%s room=%s", upCnt, bktVal, backCap, backRoom)
	}

	DBPath = keepDB
	PL.Lock()
	Datasets = map[string]*DatasetRecord{}
	PL.Unlock()
	_ = InitDB()
	_ = SetSetting("endpoint", ossSrv.URL)
	_ = SetSetting("ak", "test-ak")
	_ = SetSetting("sk", "test-sk")
	ApplyCfg()

	// ---- 2. Static pages & Scan ----
	dataDir := filepath.Join(tmp, "data")
	makeISuitSession(t, dataDir, "umi_normal", "20261108", "PRT_XZ_room001", "ISuit-V3_2026-11-08-10-30-00", "zhangsan", 2, 1794000000, "01351CA8", "1CA8")
	makeISuitSession(t, dataDir, "umi_normal", "20261109", "PRT_XZ_room001", "ISuit-V3_2026-11-08-23-40-00", "lisi", 2, 1794040000, "01351B7C", "1CA8")
	makeISuitSession(t, dataDir, "umi_calibration", "20261108", "PRT_WX_room003", "ISuit-V3_2026-11-08-09-50-00", "zhangsan", 2, 1793990000, "01351CA8", "1CA8")

	wIdx := cli.req(http.MethodGet, "/", nil, "", nil)
	wJS := cli.req(http.MethodGet, "/static/js/main.js", nil, "", nil)
	if wIdx.Code != 200 || !strings.Contains(wIdx.Body.String(), "UMI") || wJS.Code != 200 {
		t.Fatalf("static serving failed: /=%d /static/js/main.js=%d", wIdx.Code, wJS.Code)
	}

	cli.postJSON("/api/roots", map[string]any{"add": []string{dataDir}})
	scCode, _ := cli.postJSON("/api/scan", map[string]any{"city": "xz", "room": "001"})
	if scCode != 200 {
		t.Fatalf("expected scan 200, got %d", scCode)
	}
	cli.waitScan()

	_, st := cli.getJSON("/api/state")
	dsArr := st["datasets"].([]any)
	if len(dsArr) != 3 {
		t.Fatalf("expected 3 datasets, got %d", len(dsArr))
	}
	bySuffix := map[string]map[string]any{}
	var ids []string
	for _, x := range dsArr {
		m := x.(map[string]any)
		nm := m["name"].(string)
		bySuffix[nm[len(nm)-19:]] = m
		ids = append(ids, m["id"].(string))
	}
	if got := bySuffix["2026-11-08-10-30-00"]["oss_url"]; got != "oss://hw-upload/raw_data/raw_data_261108/umi_normal/PRT_XZ_room001/ISuit-V3_2026-11-08-10-30-00/" {
		t.Fatalf("unexpected oss_url: %v", got)
	}
	if !strings.HasPrefix(bySuffix["2026-11-08-09-50-00"]["rel"].(string), "umi_calibration/PRT_XZ_room001/") || bySuffix["2026-11-08-09-50-00"]["room_diff"] == "" {
		t.Fatalf("expected city/room override + room_diff warning")
	}
	if bySuffix["2026-11-08-23-40-00"]["date_warn"] == "" || bySuffix["2026-11-08-23-40-00"]["cap_date"] != "2026-11-08" {
		t.Fatalf("expected date_warn and cap_date=2026-11-08")
	}

	// ---- 3. Aliyun OSS Go SDK Upload (ds_jobs=2, folder / slice / pipeline) ----
	uploadAll := func(targetIDs []string, extraCfg map[string]any) map[string]any {
		mockOSS.reset()
		cfgBody := map[string]any{"jobs": "4", "parallel": "2"}
		for k, v := range extraCfg {
			cfgBody[k] = v
		}
		code, resp := cli.postJSON("/api/config", cfgBody)
		if code != 200 {
			t.Fatalf("config failed: %d %v", code, resp)
		}
		code, resp = cli.postJSON("/api/run", map[string]any{"action": "upload", "ids": targetIDs, "force": true})
		if code != 200 {
			t.Fatalf("run failed: %d %v", code, resp)
		}
		return cli.waitJob()
	}

	j := uploadAll(ids, map[string]any{"order": "folder", "ds_jobs": "2"})
	prog := j["prog"].(map[string]any)
	if toInt64(prog["ok"]) != 3 || toInt64(prog["fail"]) != 0 {
		t.Fatalf("folder upload failed: %v, log=%v", prog, j["log"])
	}
	writes := mockOSS.writes()
	for idx, w := range writes {
		if strings.HasSuffix(w, "/_READY") {
			mfKey := strings.TrimSuffix(w, "_READY") + "manifest.json"
			mfIdx := -1
			for k := 0; k < idx; k++ {
				if writes[k] == mfKey {
					mfIdx = k
					break
				}
			}
			if mfIdx < 0 {
				t.Fatalf("manifest.json was not written before _READY for %s", w)
			}
		}
	}

	// Skipping already-uploaded when force=false
	skipCode, _ := cli.postJSON("/api/run", map[string]any{"action": "upload", "ids": ids, "force": false})
	if skipCode != 400 {
		t.Fatalf("expected 400 when all selected datasets are already uploaded, got %d", skipCode)
	}

	// Test `slice` (per-folder CheckUnit + AheadHasher) and `pipeline` orders, plus `_pipeline` exclusion
	PL.Lock()
	src0 := Datasets[ids[0]].Source
	PL.Unlock()
	_ = os.MkdirAll(filepath.Join(src0, "_pipeline"), 0o755)
	_ = os.WriteFile(filepath.Join(src0, "_pipeline", "skipme.json"), []byte("{}"), 0o644)

	for _, ord := range []string{"slice", "pipeline"} {
		jOrd := uploadAll(ids[:1], map[string]any{"order": ord, "ds_jobs": "1"})
		pOrd := jOrd["prog"].(map[string]any)
		if toInt64(pOrd["ok"]) != 1 || toInt64(pOrd["fail"]) != 0 {
			t.Fatalf("order=%s upload failed: %v %v", ord, pOrd, jOrd["log"])
		}
		for _, k := range mockOSS.listBucketKeys("hw-upload") {
			if strings.Contains(k, "_pipeline") {
				t.Fatalf("_pipeline file leaked to OSS: %s", k)
			}
		}
	}
	_ = os.RemoveAll(filepath.Join(src0, "_pipeline"))

	// ---- 4. Multipart (>10MB) + Network Outage Auto-Wait + Persistent Checkpoint Resume ----
	bigFile := filepath.Join(src0, "big_video.bin")
	bigPayload := bytes.Repeat([]byte("ABCDEFGH12345678"), 700*1024) // ~11.2 MiB (> 10 MiB MultipartThreshold => 2 parts!)
	if err := os.WriteFile(bigFile, bigPayload, 0o644); err != nil {
		t.Fatal(err)
	}
	cli.postJSON("/api/scan", map[string]any{"city": "xz", "room": "001", "full": true})
	cli.waitScan()

	// 4a: Outage at start -> waits & recovers automatically
	mockOSS.reset()
	_ = os.RemoveAll(CkptDir)
	cli.postJSON("/api/config", map[string]any{"order": "slice", "ds_jobs": "1", "jobs": "2", "parallel": "2", "net_wait": "1"})
	mockOSS.setNetMode("heal_on_probe")
	code, _ := cli.postJSON("/api/run", map[string]any{"action": "upload", "ids": ids[:1], "force": true})
	if code != 200 {
		t.Fatalf("expected 200 starting upload during outage, got %d", code)
	}
	jNet1 := cli.waitJob()
	logStr1 := fmt.Sprint(jNet1["log"])
	if toInt64(jNet1["prog"].(map[string]any)["ok"]) != 1 || !strings.Contains(logStr1, "网络中断，等待恢复") || !strings.Contains(logStr1, "网络已恢复") {
		t.Fatalf("expected automatic recovery from initial outage, got prog=%v log=%s", jNet1["prog"], logStr1)
	}
	// Verify multipart uploaded big_video.bin byte-for-byte
	var uploadedBig []byte
	for _, k := range mockOSS.listBucketKeys("hw-upload") {
		if strings.HasSuffix(k, "/big_video.bin") {
			uploadedBig, _ = mockOSS.getObjectBytes("hw-upload", k)
		}
	}
	if !bytes.Equal(uploadedBig, bigPayload) {
		t.Fatalf("multipart uploaded big_video.bin content mismatch (got %d bytes, want %d)", len(uploadedBig), len(bigPayload))
	}

	// 4b: Outage exceeds net_wait -> fails, keeps checkpoint (`started` + `.ucp`), then next run resumes!
	mockOSS.reset()
	_ = os.RemoveAll(CkptDir)
	cli.postJSON("/api/config", map[string]any{"order": "folder", "ds_jobs": "1", "jobs": "1", "parallel": "1", "net_wait": "0.005"}) // 0.3s budget
	mockOSS.setNetMode("put_after_1")
	cli.postJSON("/api/run", map[string]any{"action": "upload", "ids": ids[:1], "force": true})
	jFail := cli.waitJob()
	logFail := fmt.Sprint(jFail["log"])
	if toInt64(jFail["prog"].(map[string]any)["fail"]) != 1 || !strings.Contains(logFail, "网络中断超过") {
		t.Fatalf("expected timeout failure, got prog=%v log=%s", jFail["prog"], logFail)
	}
	hasStarted := false
	_ = filepath.WalkDir(CkptDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "started" {
			hasStarted = true
		}
		return nil
	})
	if !hasStarted {
		t.Fatalf("expected started checkpoint marker preserved in %s", CkptDir)
	}

	// Restore network and resume upload -> logs "断点续传" and cleans up CkptDir on commit!
	mockOSS.setNetMode("")
	cli.postJSON("/api/config", map[string]any{"net_wait": "30"})
	cli.postJSON("/api/run", map[string]any{"action": "upload", "ids": ids[:1], "force": true})
	jRes := cli.waitJob()
	logRes := fmt.Sprint(jRes["log"])
	if toInt64(jRes["prog"].(map[string]any)["ok"]) != 1 || !strings.Contains(logRes, "断点续传") {
		t.Fatalf("expected resume upload to succeed and log 断点续传, got prog=%v log=%s", jRes["prog"], logRes)
	}
	var leftoverCkpt []string
	_ = filepath.WalkDir(CkptDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			leftoverCkpt = append(leftoverCkpt, p)
		}
		return nil
	})
	if len(leftoverCkpt) > 0 {
		t.Fatalf("expected CkptDir cleaned after successful commit, found: %v", leftoverCkpt)
	}

	// 4c: Test connection fails fast when network is down (does NOT wait net_wait=30min)
	mockOSS.setNetMode("all")
	tTest0 := time.Now()
	_, testResp := cli.postJSON("/api/config/test", map[string]any{})
	mockOSS.setNetMode("")
	if testResp["ok"] != false || time.Since(tTest0) > 5*time.Second {
		t.Fatalf("expected /api/config/test to fail fast, got %v in %v", testResp, time.Since(tTest0))
	}
	_, testOKResp := cli.postJSON("/api/config/test", map[string]any{})
	if testOKResp["ok"] != true {
		t.Fatalf("expected /api/config/test ok=true when healthy, got %v", testOKResp)
	}

	_ = os.Remove(bigFile)
	cli.postJSON("/api/scan", map[string]any{"city": "xz", "room": "001", "full": true})
	cli.waitScan()

	// Config validation checks
	for _, badKV := range [][2]string{
		{"jobs", "abc"}, {"jobs", "0"}, {"ds_jobs", "3.5"}, {"net_wait", "-1"}, {"method", "ftp"}, {"order", "invalid"},
		{"bucket", "UPPER"}, {"bucket_umi", "bad_underscore"},
	} {
		cBad, _ := cli.postJSON("/api/config", map[string]any{badKV[0]: badKV[1]})
		if cBad != 400 {
			t.Fatalf("expected 400 for %v, got %d", badKV, cBad)
		}
	}

	// ---- 5. Quick & Deep Verify + Backfill + Exports ----
	j = uploadAll(ids, map[string]any{"order": "folder", "ds_jobs": "2"})
	cli.postJSON("/api/run", map[string]any{"action": "verify", "ids": ids})
	cli.waitJob()
	_, stVer := cli.getJSON("/api/state")
	for _, x := range stVer["datasets"].([]any) {
		rem := x.(map[string]any)["remote"].(map[string]any)
		if rem["status"] != "一致" {
			t.Fatalf("expected quick verify 一致, got %v", rem)
		}
	}
	cli.postJSON("/api/run", map[string]any{"action": "verify_deep", "ids": ids})
	cli.waitJob()
	_, stDeep := cli.getJSON("/api/state")
	for _, x := range stDeep["datasets"].([]any) {
		rem := x.(map[string]any)["remote"].(map[string]any)
		if rem["status"] != "一致" || rem["deep"] != true {
			t.Fatalf("expected deep verify 一致 + deep=true, got %v", rem)
		}
	}

	// Delete one .record object remotely -> verify detects 不一致
	mockOSS.deleteKeySuffix("hw-upload", ".record")
	cli.postJSON("/api/run", map[string]any{"action": "verify", "ids": ids})
	cli.waitJob()
	_, stDiff := cli.getJSON("/api/state")
	foundDiff := false
	for _, x := range stDiff["datasets"].([]any) {
		rem := x.(map[string]any)["remote"].(map[string]any)
		if rem["status"] == "不一致" {
			foundDiff = true
		}
	}
	if !foundDiff {
		t.Fatalf("expected verify to report 不一致 after deleting remote object")
	}

	// Uploads query, deduplication, keyword escaping, Excel & JSON export
	_, uq := cli.getJSON("/api/uploads")
	if toInt64(uq["datasets"]) != 3 || math.Abs(toFloat(uq["duration"])-180.0) > 1 {
		t.Fatalf("uploads deduplicated summary mismatch: %v", uq)
	}
	_, uqPct := cli.getJSON("/api/uploads?q=" + url.QueryEscape("%"))
	if toInt64(uqPct["total"]) != 0 {
		t.Fatalf("expected %% in q to be treated literally, got total=%v", uqPct["total"])
	}

	wXls := cli.req(http.MethodGet, "/api/uploads/export?operator=lisi", nil, "", nil)
	if wXls.Code != 200 || !strings.Contains(wXls.Header().Get("Content-Type"), "spreadsheetml") {
		t.Fatalf("xlsx export failed: code=%d ct=%s", wXls.Code, wXls.Header().Get("Content-Type"))
	}
	zr, err := zip.NewReader(bytes.NewReader(wXls.Body.Bytes()), int64(wXls.Body.Len()))
	if err != nil {
		t.Fatalf("xlsx zip invalid: %v", err)
	}
	var wbXML, sh1XML string
	for _, zf := range zr.File {
		rc, _ := zf.Open()
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		if zf.Name == "xl/workbook.xml" {
			wbXML = string(b)
		} else if zf.Name == "xl/worksheets/sheet1.xml" {
			sh1XML = string(b)
		}
	}
	for _, sheetTitle := range []string{"上传记录", "汇总", "按设备", "按人员"} {
		if !strings.Contains(wbXML, sheetTitle) {
			t.Fatalf("xlsx missing sheet %s: %s", sheetTitle, wbXML)
		}
	}
	if !strings.Contains(sh1XML, "lisi") || strings.Contains(sh1XML, "zhangsan") {
		t.Fatalf("xlsx filter operator=lisi failed")
	}

	wOff := cli.req(http.MethodGet, "/api/export", nil, "", nil)
	if wOff.Code != 200 || !strings.Contains(wOff.Body.String(), "dataset") {
		t.Fatalf("official export failed: %d %s", wOff.Code, wOff.Body.String())
	}

	// Backfill check (idempotent via ux_uploads_backfill)
	curDB, err := OpenDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := curDB.Exec("DELETE FROM uploads; DELETE FROM checks;"); err != nil {
		t.Fatalf("clear uploads/checks: %v", err)
	}
	if code, resp := cli.postJSON("/api/run", map[string]any{"action": "verify", "ids": ids}); code != 200 {
		t.Fatalf("verify run failed: %d %v", code, resp)
	}
	cli.waitJob()
	if code, resp := cli.postJSON("/api/run", map[string]any{"action": "verify", "ids": ids}); code != 200 {
		t.Fatalf("second verify run failed: %d %v", code, resp)
	}
	cli.waitJob()
	var bfCnt int
	if err := curDB.QueryRow("SELECT COUNT(*) FROM uploads WHERE method='校验补录'").Scan(&bfCnt); err != nil {
		t.Fatal(err)
	}
	if bfCnt != 2 {
		t.Fatalf("expected exactly 2 backfilled uploads (excluding inconsistent one, no duplicates), got %d", bfCnt)
	}

	// ---- 6. Dual Formats Coexistence (ISuit-V3 -> hw-upload, UMI_v3 -> prt-huawei), Move, Videos & Real Fixture ----
	dataI := filepath.Join(tmp, "data_isuit")
	dataU := filepath.Join(tmp, "data_umi")
	makeISuitSession(t, dataI, "umi_normal", "20261108", "PRT_XZ_room001", "ISuit-V3_2026-11-08-10-30-00", "zhangsan", 8, 1794000000, "01351CA8", "1CA8")
	makeISuitSession(t, dataI, "umi_normal", "20261108", "PRT_XZ_room001", "ISuit-V3_2026-11-08-11-00-00", "lisi", 1, 1794010000, "01351CA8", "1CA8") // 30s < 180s => auto misplaced!

	logU1 := makeRecLog("2026-11-08 10:30:00", 600.5)
	makeUMISession(t, dataU, "umi_normal", "20261108", "PRT_XZ_room001", "UMI_v3_20261108_103000", umiMakeOpts{
		Operator: "zhangsan", RootFile: true, Log: &logU1, Dev: "1CA8", HasDev: true,
	})
	logU2 := makeRecLog("2026-11-08 09:50:00", 120)
	confU2 := "device_SN: 01351CA8\nrecord_operator: zhangsan\ntask_name: 项目X\ntask_title: 开工标定\n"
	makeUMISession(t, dataU, "umi_calibration", "20261108", "PRT_XZ_room001", "UMI_v3_20261108_095000", umiMakeOpts{
		Conf: &confU2, Log: &logU2, Videos: []string{"left hand/cam-l.mp4", "head/cam-h.mp4"}, Dev: "1CA8", HasDev: true,
	})
	tF0 := float64(time.Date(2026, 11, 9, 8, 0, 2, 0, TZ).Unix())
	tF1 := float64(time.Date(2026, 11, 9, 8, 5, 2, 0, TZ).Unix())
	framesU3 := [2]float64{tF0, tF1}
	confU3 := "device_SN: 01351B7C\nrecord_operator: lisi\ntask_name: 测试任务\nrecord_location: Default\n"
	makeUMISession(t, dataU, "umi_normal", "20261108", "PRT_XZ_room001", "UMI_v3_20261109_080000", umiMakeOpts{
		Conf: &confU3, Frames: &framesU3, Dev: "1CA8", HasDev: true,
	})
	logU5 := makeRecLog("2026-11-08 14:00:00", 600.0)
	confU5 := "device_SN: 01351CA8\nrecord_operator: wang\ntask_name: 项目X\ntask_title: 收工标定\n"
	u5Dir := makeUMISession(t, dataU, "umi_normal", "20261108", "PRT_XZ_room001", "UMI_v3_20261108_140000", umiMakeOpts{
		Conf: &confU5, Log: &logU5, Dev: "1CA8", HasDev: true,
	})
	// Incomplete UMI_v3 sessions (must be reported in problems and excluded from datasets)
	makeUMISession(t, dataU, "umi_normal", "20261108", "PRT_XZ_room001", "UMI_v3_20261108_111111", umiMakeOpts{
		Skip: map[string]bool{"metadata/conf.yaml": true}, Dev: "1CA8", HasDev: true,
	})
	makeUMISession(t, dataU, "umi_normal", "20261108", "PRT_XZ_room001", "UMI_v3_20261108_121212", umiMakeOpts{
		Skip: map[string]bool{"left hand": true}, Dev: "1CA8", HasDev: true,
	})

	cli.postJSON("/api/roots", map[string]any{"remove": []string{dataDir}, "add": []string{dataI, dataU}})
	cli.postJSON("/api/scan", map[string]any{})
	cli.waitScan()

	_, stMix := cli.getJSON("/api/state")
	byName := map[string]map[string]any{}
	for _, x := range stMix["datasets"].([]any) {
		m := x.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if len(byName) != 6 {
		t.Fatalf("expected 6 valid sessions (2 ISuit + 4 UMI_v3), got %d: %v", len(byName), stMix["problems"])
	}
	probText := fmt.Sprint(stMix["problems"])
	if !strings.Contains(probText, "缺少 metadata/conf.yaml") || !strings.Contains(probText, "缺少采集目录 left hand") {
		t.Fatalf("expected incomplete UMI_v3 sessions reported in problems: %s", probText)
	}

	// Auto move plan only includes misplaced ISuit-V3 (never UMI_v3)
	_, mp := cli.getJSON("/api/move/plan")
	plans := mp["plans"].([]any)
	if len(plans) != 1 || plans[0].(map[string]any)["name"] != "ISuit-V3_2026-11-08-11-00-00" {
		t.Fatalf("expected only ISuit-V3_2026-11-08-11-00-00 in auto move plan, got %v", plans)
	}
	_, mvAuto := cli.postJSON("/api/move", map[string]any{"ids": []string{byName["ISuit-V3_2026-11-08-11-00-00"]["id"].(string)}})
	if mvAuto["ok"] != true {
		t.Fatalf("ISuit auto move failed: %v", mvAuto)
	}

	// UMI_v3 manual move normal -> calibration -> normal
	idU5 := byName["UMI_v3_20261108_140000"]["id"].(string)
	_, mvMan := cli.postJSON("/api/move", map[string]any{"ids": []string{idU5}, "to": "calibration"})
	if mvMan["ok"] != true {
		t.Fatalf("UMI_v3 manual move to calibration failed: %v", mvMan)
	}
	newIDU5 := mvMan["moved"].([]any)[0].(map[string]any)["new_id"].(string)
	_, mvBack := cli.postJSON("/api/move", map[string]any{"ids": []string{newIDU5}, "to": "normal"})
	if mvBack["ok"] != true || mvBack["moved"].([]any)[0].(map[string]any)["new_id"] != idU5 {
		t.Fatalf("UMI_v3 manual move back to normal failed: %v", mvBack)
	}
	if _, err := os.Stat(u5Dir); err != nil {
		t.Fatalf("expected U5 back at original path %s: %v", u5Dir, err)
	}

	// Video listing, HTTP Range 206 streaming, path traversal & non-local 403 guard
	idU2 := byName["UMI_v3_20261108_095000"]["id"].(string)
	_, vj := cli.getJSON("/api/videos?id=" + url.QueryEscape(idU2))
	vfiles := vj["files"].([]any)
	if len(vfiles) != 2 || vfiles[0].(map[string]any)["f"] != "head/cam-h.mp4" || vfiles[1].(map[string]any)["f"] != "left hand/cam-l.mp4" {
		t.Fatalf("unexpected video list order: %v", vfiles)
	}
	wVidFull := cli.req(http.MethodGet, "/api/video?id="+url.QueryEscape(idU2)+"&f="+url.QueryEscape("head/cam-h.mp4"), nil, "", nil)
	if wVidFull.Code != 200 || wVidFull.Body.Len() != 20480 || wVidFull.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("video full GET failed: code=%d len=%d accept-ranges=%s", wVidFull.Code, wVidFull.Body.Len(), wVidFull.Header().Get("Accept-Ranges"))
	}
	wVidRange := cli.req(http.MethodGet, "/api/video?id="+url.QueryEscape(idU2)+"&f="+url.QueryEscape("head/cam-h.mp4"), nil, "", map[string]string{"Range": "bytes=100-199"})
	if wVidRange.Code != 206 || wVidRange.Header().Get("Content-Range") != "bytes 100-199/20480" || wVidRange.Body.Len() != 100 {
		t.Fatalf("video Range GET failed: code=%d cr=%s len=%d", wVidRange.Code, wVidRange.Header().Get("Content-Range"), wVidRange.Body.Len())
	}
	for _, badF := range []string{"../UMI_v3_20261108_103000/head/a.bin", "../../outside.mp4", "metadata/conf.yaml", "/etc/passwd", ""} {
		wb := cli.req(http.MethodGet, "/api/video?id="+url.QueryEscape(idU2)+"&f="+url.QueryEscape(badF), nil, "", nil)
		if wb.Code != 404 {
			t.Fatalf("expected 404 for unsafe video path %q, got %d", badF, wb.Code)
		}
	}
	wRemote := cli.req(http.MethodGet, "/api/videos?id="+url.QueryEscape(idU2), nil, "10.0.0.8:12345", nil)
	if wRemote.Code != 403 {
		t.Fatalf("expected 403 for non-local /api/videos request, got %d", wRemote.Code)
	}

	// Mixed upload (ISuit-V3 + UMI_v3 in a single job -> isolated into hw-upload and prt-huawei buckets!)
	_, stMix = cli.getJSON("/api/state")
	var mixIDs []string
	for _, x := range stMix["datasets"].([]any) {
		mixIDs = append(mixIDs, x.(map[string]any)["id"].(string))
	}
	seenBucketsMu.Lock()
	seenBuckets = map[string]bool{}
	seenBucketsMu.Unlock()
	jMix := uploadAll(mixIDs, map[string]any{"order": "folder", "ds_jobs": "2"})
	if toInt64(jMix["prog"].(map[string]any)["ok"]) != 6 {
		t.Fatalf("mixed upload failed: %v %v", jMix["prog"], jMix["log"])
	}
	seenBucketsMu.Lock()
	bothBkts := seenBuckets["hw-upload"] && seenBuckets["prt-huawei"]
	seenBucketsMu.Unlock()
	if !bothBkts {
		t.Fatalf("expected separate clients created for hw-upload and prt-huawei")
	}
	for _, k := range mockOSS.listBucketKeys("hw-upload") {
		if strings.Contains(k, "UMI_v3_") {
			t.Fatalf("UMI_v3 leaked into hw-upload: %s", k)
		}
	}
	for _, k := range mockOSS.listBucketKeys("prt-huawei") {
		if strings.Contains(k, "ISuit-V3_") {
			t.Fatalf("ISuit-V3 leaked into prt-huawei: %s", k)
		}
	}

	// Multi-WAN plan endpoint
	npCode, np := cli.getJSON("/api/net/plan?wan=3&ds_jobs=2&parallel=4")
	if npCode != 200 || toInt64(np["recommend"].(map[string]any)["ds_jobs"]) != 6 || np["current"].(map[string]any)["enough"] != false {
		t.Fatalf("/api/net/plan mismatch: %d %v", npCode, np)
	}

	// ---- 7. Real Fixture (sample/tests/fixtures/UMI_v3_20261117_130624 with !<tag:yaml.org,2002:str>) ----
	fxSrc := filepath.Join(repoRoot, "sample", "tests", "fixtures", "UMI_v3_20261117_130624")
	if stFx, err := os.Stat(fxSrc); err == nil && stFx.IsDir() {
		dataFx := filepath.Join(tmp, "data_fx")
		sessFx := filepath.Join(dataFx, "umi_normal", "prt004", "20261117", "PRT_WX_room005", "UMI_v3_20261117_130624")
		copyDirRecursive(t, fxSrc, sessFx)

		cli.postJSON("/api/roots", map[string]any{"remove": []string{dataI, dataU}, "add": []string{dataFx}})
		cli.postJSON("/api/scan", map[string]any{})
		cli.waitScan()

		_, stReal := cli.getJSON("/api/state")
		realDS := stReal["datasets"].([]any)
		if len(realDS) != 1 || len(stReal["problems"].([]any)) != 0 {
			t.Fatalf("real fixture scan failed: ds=%d problems=%v", len(realDS), stReal["problems"])
		}
		rd := realDS[0].(map[string]any)
		if rd["oss_url"] != "oss://prt-huawei/raw_data/raw_data_261117/umi_normal/PRT_WX_room005/UMI_v3_20261117_130624/" ||
			rd["device"] != "prt004" || rd["device_sn"] != "prt004" ||
			rd["task"] != "华为居家采集 / 玩具分类收纳-自然光" || rd["sub_task"] != "" ||
			rd["start"] != "2026-11-17 13:06:24" || rd["end"] != "2026-11-17 13:17:15" || toFloat(rd["duration"]) != 651.5 {
			t.Fatalf("real fixture metadata mismatch: %v", rd)
		}
		jFx := uploadAll([]string{rd["id"].(string)}, map[string]any{"order": "slice", "ds_jobs": "1"})
		if toInt64(jFx["prog"].(map[string]any)["ok"]) != 1 {
			t.Fatalf("real fixture upload failed: %v %v", jFx["prog"], jFx["log"])
		}

		// Without recording.log -> falls back to 3 cameras' frames.csv exposure_ns (~651.47s)
		noLogDir := filepath.Join(tmp, "fx_nolog")
		copyDirRecursive(t, fxSrc, noLogDir)
		_ = os.Remove(filepath.Join(noLogDir, "metadata", "recording.log"))
		durNL, startNL, _ := UMITimes(noLogDir, "UMI_v3_20261117_130624")
		if math.Abs(durNL-651.47) > 0.02 || startNL != "2026-11-17 13:06:24" {
			t.Fatalf("frames.csv fallback mismatch: dur=%v start=%s", durNL, startNL)
		}
	}

	// ---- 8. Lifecycle: Interrupted run notice + ack + shutdown + JS syntax check ----
	curDB, _ = OpenDB()
	if _, err := curDB.Exec("INSERT INTO runs(action,method,ds_jobs,total,ok,fail,status,started_at) VALUES('upload','sdk·逐文件夹',2,9,0,0,'running','2026-11-08 10:00:00')"); err != nil {
		t.Fatalf("insert running run: %v", err)
	}
	_ = InitDB()
	_, stNotice := cli.getJSON("/api/state?light=1")
	notice, _ := stNotice["notice"].(map[string]any)
	if notice == nil || notice["status"] != "interrupted" || toInt64(notice["total"]) != 9 {
		t.Fatalf("expected interrupted notice after restart, got %v", notice)
	}
	cli.postJSON("/api/notice/ack", map[string]any{})
	_, stAcked := cli.getJSON("/api/state?light=1")
	if stAcked["notice"] != nil {
		t.Fatalf("expected notice=nil after ack, got %v", stAcked["notice"])
	}

	shutdownCalled := false
	ShutdownLaterFunc = func() { shutdownCalled = true }
	sdCode, _ := cli.postJSON("/api/shutdown", map[string]any{})
	if sdCode != 200 || !shutdownCalled {
		t.Fatalf("shutdown endpoint failed: code=%d called=%v", sdCode, shutdownCalled)
	}

	if nodeBin, err := exec.LookPath("node"); err == nil && nodeBin != "" {
		jsEntries, _ := os.ReadDir(filepath.Join(webDir, "js"))
		for _, je := range jsEntries {
			if strings.HasSuffix(je.Name(), ".js") {
				jsPath := filepath.Join(webDir, "js", je.Name())
				if out, cerr := exec.Command(nodeBin, "--check", jsPath).CombinedOutput(); cerr != nil {
					t.Fatalf("JS syntax error in %s: %v\n%s", jsPath, cerr, string(out))
				}
			}
		}
	}
}

func TestManifestParityAndUTF16Order(t *testing.T) {
	// Verify UTF-16 code unit ordering matches JavaScript/Python-UTF16 sort (including BMP high codepoints vs Astral surrogates)
	// U+FF00 (BMP: 0xFF00) vs U+1F600 (Surrogate pair: 0xD83D, 0xDE00 => 0xD83D < 0xFF00 in UTF-16, whereas 0x1F600 > 0xFF00 in UTF-8!)
	bmp := "\uFF00"
	astral := "\U0001F600"
	if UTF16Compare(astral, bmp) >= 0 || UTF16Compare(bmp, astral) <= 0 {
		t.Fatalf("UTF16Compare failed surrogate vs high-BMP test")
	}

	// Verify exact revision parity with Python's json.dumps(..., ensure_ascii=False, separators=(',', ':'), sort_keys=True)
	pyBin, err := exec.LookPath("python3")
	if err != nil {
		return
	}
	repoRoot := filepath.Clean("..")
	fxSrc := filepath.Join(repoRoot, "sample", "tests", "fixtures", "UMI_v3_20261117_130624")
	if _, err := os.Stat(fxSrc); err != nil {
		return
	}
	rec := &DatasetRecord{
		Name:   "UMI_v3_20261117_130624",
		Type:   "normal",
		Rel:    "umi_normal/PRT_WX_room005/UMI_v3_20261117_130624",
		Source: fxSrc,
	}
	ds, err := MakeDS(rec, map[string]string{"bucket_umi": "prt-huawei"})
	if err != nil {
		t.Fatal(err)
	}
	files, err := Snapshot(fxSrc, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	goManifest, err := MakeManifest(ds, files)
	if err != nil {
		t.Fatal(err)
	}

	pyScript := fmt.Sprintf(`
import sys
sys.path.insert(0, %q)
import upload_umi_sdk as sdk
from pathlib import Path
p = Path(%q)
ds = {"name": p.name, "capture_date": "2026-11-17", "type": "normal"}
files = sdk.snapshot(p, 4)
m = sdk.make_manifest(ds, files)
print(m["revision"])
`, filepath.Join(repoRoot, "sample"), fxSrc)

	out, err := exec.Command(pyBin, "-B", "-c", pyScript).CombinedOutput()
	if err != nil {
		t.Fatalf("python parity script failed: %v\n%s", err, string(out))
	}
	pyRev := strings.TrimSpace(string(out))
	if goManifest.Revision != pyRev {
		t.Fatalf("manifest revision mismatch!\nGo:     %s\nPython: %s", goManifest.Revision, pyRev)
	}
	t.Logf("Verified exact manifest revision parity with Python SDK: %s", goManifest.Revision)
}

func copyDirRecursive(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}


func TestS3CompatibleClientUsesAWSV4AndMultipart(t *testing.T) {
	mock := newMockOSSServer()
	mock.objects["test-bucket"] = map[string][]byte{}
	mock.requireV4 = true
	srv := httptest.NewServer(mock)
	defer srv.Close()

	client, err := NewS3CompatibleClient(OSSParams{
		Endpoint:       srv.URL,
		AK:             "test-ak",
		SK:             "test-sk",
		Bucket:         "test-bucket",
		Parallel:       2,
		NetWaitMinutes: 0,
	})
	if err != nil {
		t.Fatalf("create S3 client: %v", err)
	}
	if err := client.TestBucket(); err != nil {
		t.Fatalf("AWS V4 bucket probe failed: %v", err)
	}

	prefix := "oss://test-bucket/parity/"
	if err := client.PutObject(prefix+"manifest.json", []byte(`{"version":1}`)); err != nil {
		t.Fatalf("small S3 object PUT failed: %v", err)
	}
	body, found, err := client.ReadObject(prefix + "manifest.json")
	if err != nil || !found || string(body) != `{"version":1}` {
		t.Fatalf("S3 read-back mismatch: found=%v body=%q err=%v", found, string(body), err)
	}

	tmp := t.TempDir()
	local := filepath.Join(tmp, "large-video.bin")
	payload := bytes.Repeat([]byte("s3-multipart-v4-"), 700*1024) // > 10 MiB
	if err := os.WriteFile(local, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	var reportedBytes int64
	var progressMu sync.Mutex
	if err := client.UploadFile(local, "parity/large-video.bin", int64(len(payload)), "", func(n int64) {
		progressMu.Lock()
		reportedBytes += n
		progressMu.Unlock()
	}); err != nil {
		t.Fatalf("S3 multipart upload failed: %v", err)
	}
	progressMu.Lock()
	gotProgress := reportedBytes
	progressMu.Unlock()
	if gotProgress != int64(len(payload)) {
		t.Fatalf("S3 request-body progress = %d, want %d", gotProgress, len(payload))
	}
	uploaded, ok := mock.getObjectBytes("test-bucket", "parity/large-video.bin")
	if !ok || !bytes.Equal(uploaded, payload) {
		t.Fatalf("S3 multipart payload mismatch: found=%v got=%d want=%d", ok, len(uploaded), len(payload))
	}

	items, err := client.ListObjects(prefix)
	if err != nil {
		t.Fatalf("S3 list objects failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected manifest + uploaded video in list, got %d: %+v", len(items), items)
	}
	mock.mu.Lock()
	v4Requests := mock.sigV4Requests
	mock.mu.Unlock()
	if v4Requests < 6 {
		t.Fatalf("expected AWS Signature V4 on all S3 requests, observed %d requests", v4Requests)
	}
}
