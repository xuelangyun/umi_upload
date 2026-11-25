package umi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestETagMatches(t *testing.T) {
	const etag = "\"dataset-v1\""
	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{name: "exact", header: etag, want: true},
		{name: "weak validator", header: "W/" + etag, want: true},
		{name: "multiple validators", header: "\"other\", " + etag, want: true},
		{name: "wildcard", header: "*", want: true},
		{name: "different", header: "\"dataset-v2\"", want: false},
		{name: "empty", header: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := etagMatches(tt.header, etag); got != tt.want {
				t.Fatalf("etagMatches(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

func TestDatasetRowsConfigKeyTracksBucketsOnly(t *testing.T) {
	base := map[string]string{"bucket": "upload-a", "bucket_umi": "upload-b", "endpoint": "https://oss.example"}
	if got, want := datasetRowsConfigKey(base), DBPath+"\x00upload-a\x00upload-b"; got != want {
		t.Fatalf("config key = %q, want %q", got, want)
	}

	changedEndpoint := map[string]string{"bucket": "upload-a", "bucket_umi": "upload-b", "endpoint": "https://other.example"}
	if datasetRowsConfigKey(changedEndpoint) != datasetRowsConfigKey(base) {
		t.Fatal("endpoint-only changes should not invalidate dataset rows")
	}
	changedBucket := map[string]string{"bucket": "upload-c", "bucket_umi": "upload-b"}
	if datasetRowsConfigKey(changedBucket) == datasetRowsConfigKey(base) {
		t.Fatal("bucket changes must invalidate dataset rows")
	}
	changedUMIBucket := map[string]string{"bucket": "upload-a", "bucket_umi": "upload-d"}
	if datasetRowsConfigKey(changedUMIBucket) == datasetRowsConfigKey(base) {
		t.Fatal("UMI bucket changes must invalidate dataset rows")
	}
}

func TestDatasetEndpointUsesETag(t *testing.T) {
	oldDBPath := DBPath
	oldCfg := ActiveCfg()
	PL.Lock()
	oldDatasets := Datasets
	PL.Unlock()

	CloseDB()
	DBPath = filepath.Join(t.TempDir(), "api-cache.db")
	invalidateDatasetRowsCache()
	defer func() {
		CloseDB()
		DBPath = oldDBPath
		setActiveCfg(oldCfg)
		PL.Lock()
		Datasets = oldDatasets
		PL.Unlock()
		invalidateDatasetRowsCache()
	}()

	if err := InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	ApplyCfg()
	PL.Lock()
	Datasets = map[string]*DatasetRecord{}
	PL.Unlock()
	invalidateDatasetRowsCache()

	handler := NewHandler(fstest.MapFS{})
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/datasets", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d: %s", first.Code, http.StatusOK, first.Body.String())
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("dataset response did not include an ETag")
	}
	firstVersion := first.Header().Get("X-Dataset-Version")
	if firstVersion == "" {
		t.Fatal("dataset response did not include X-Dataset-Version")
	}
	if got := first.Body.String(); got != "[]" {
		t.Fatalf("empty dataset response = %q, want []", got)
	}

	// Invalidate without changing the response body: the ETag remains the same,
	// but clients must receive the newer logical data version.
	invalidateDatasetRowsCache()
	req := httptest.NewRequest(http.MethodGet, "/api/datasets", nil)
	req.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, req)
	if second.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want %d", second.Code, http.StatusNotModified)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 response unexpectedly has body %q", second.Body.String())
	}
	if second.Header().Get("X-Dataset-Version") == "" || second.Header().Get("X-Dataset-Version") == firstVersion {
		t.Fatalf("304 response did not report updated data version: first=%q second=%q", firstVersion, second.Header().Get("X-Dataset-Version"))
	}
}


func TestStaticAssetsUseConditionalETags(t *testing.T) {
	data := []byte("window.appVersion = 1;")
	webFS := fstest.MapFS{
		"js/app.js": &fstest.MapFile{Data: data},
	}
	handler := NewHandler(webFS)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/static/js/app.js", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", first.Code, http.StatusOK)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("static asset response did not include an ETag")
	}
	if got := first.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
	if first.Body.String() != string(data) {
		t.Fatalf("static asset body = %q, want %q", first.Body.String(), data)
	}

	req := httptest.NewRequest(http.MethodGet, "/static/js/app.js", nil)
	req.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, req)
	if second.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want %d", second.Code, http.StatusNotModified)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 response unexpectedly has body %q", second.Body.String())
	}

	webFS["js/app.js"] = &fstest.MapFile{Data: []byte("window.appVersion = 2;")}
	updated := httptest.NewRecorder()
	handler.ServeHTTP(updated, req)
	if updated.Code != http.StatusOK {
		t.Fatalf("updated asset status = %d, want %d", updated.Code, http.StatusOK)
	}
	if updated.Header().Get("ETag") == etag {
		t.Fatal("updated asset reused the previous ETag")
	}
	if updated.Body.String() != "window.appVersion = 2;" {
		t.Fatalf("updated asset body = %q", updated.Body.String())
	}
}

func TestFinishRunReturnsDatabaseErrors(t *testing.T) {
	oldDBPath := DBPath
	CloseDB()
	DBPath = t.TempDir()
	defer func() {
		CloseDB()
		DBPath = oldDBPath
	}()

	if err := FinishRun(1, 0, 0); err == nil {
		t.Fatal("FinishRun should return an error when SQLite cannot open the database path")
	}
}
