package umi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type verifyFakeOSS struct {
	objects             map[string][]byte
	files               []RemoteObj
	writes              []string
	tamperManifestRead  bool
}

func (f *verifyFakeOSS) BucketName() string { return "test" }
func (f *verifyFakeOSS) KeyOf(uri string) (string, error) { return uri, nil }
func (f *verifyFakeOSS) PutObject(uri string, data []byte) error {
	if f.objects == nil {
		f.objects = make(map[string][]byte)
	}
	f.objects[uri] = append([]byte(nil), data...)
	f.writes = append(f.writes, uri)
	return nil
}
func (f *verifyFakeOSS) ReadObject(uri string) ([]byte, bool, error) {
	b, ok := f.objects[uri]
	out := append([]byte(nil), b...)
	if ok && f.tamperManifestRead && strings.HasSuffix(uri, "manifest.json") && len(out) > 0 {
		out = []byte(strings.Replace(string(out), "aaaa", "baaa", 1))
	}
	return out, ok, nil
}
func (f *verifyFakeOSS) ListObjects(string) ([]RemoteObj, error) {
	return append([]RemoteObj(nil), f.files...), nil
}
func (f *verifyFakeOSS) UploadFile(string, string, int64, string, func(int64), func(int64)) error {
	return errors.New("not implemented")
}
func (f *verifyFakeOSS) TestBucket() error { return nil }
func (f *verifyFakeOSS) SetNetCallback(NetWaitCallback) {}

func TestRemoteCommitRejectsManifestWithInvalidRevision(t *testing.T) {
	manifest := Manifest{
		SchemaVersion: 1,
		DatasetID:     "dataset",
		CaptureDate:   "2026-11-23",
		DatasetType:   "test",
		Files:         []ManifestFile{{Path: "payload.bin", Size: 4, SHA256: strings.Repeat("a", 64)}},
		Revision:      "raw-not-derived-from-content",
		CreatedAt:     "2026-11-23T00:00:00.000Z",
	}
	manifestBytes, err := jsonMarshalNoEscape(manifest, "  ")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(manifestBytes)
	marker := ReadyMarker{
		SchemaVersion:  1,
		Revision:       manifest.Revision,
		ManifestSHA256: hex.EncodeToString(sum[:]),
	}
	markerBytes, err := jsonMarshalNoEscape(marker, "  ")
	if err != nil {
		t.Fatal(err)
	}
	fake := &verifyFakeOSS{objects: map[string][]byte{
		"oss://test/dataset/manifest.json": manifestBytes,
		"oss://test/dataset/_READY":         markerBytes,
	}}
	if _, err := RemoteCommit(fake, "oss://test/dataset/"); err == nil {
		t.Fatal("RemoteCommit accepted a manifest whose revision does not match its content")
	}
}

func fakeCommittedManifest(t *testing.T, manifest Manifest) *verifyFakeOSS {
	t.Helper()
	revision, err := manifestRevision(manifest.DatasetID, manifest.CaptureDate, manifest.DatasetType, manifest.Files)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Revision = revision
	manifestBytes, err := jsonMarshalNoEscape(manifest, "  ")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(manifestBytes)
	markerBytes, err := jsonMarshalNoEscape(ReadyMarker{
		SchemaVersion: 1,
		Revision: manifest.Revision,
		ManifestSHA256: hex.EncodeToString(sum[:]),
	}, "  ")
	if err != nil {
		t.Fatal(err)
	}
	return &verifyFakeOSS{objects: map[string][]byte{
		"oss://test/dataset/manifest.json": manifestBytes,
		"oss://test/dataset/_READY": markerBytes,
	}}
}

func TestMakeManifestRejectsInvalidLocalFiles(t *testing.T) {
	validHash := strings.Repeat("a", 64)
	tests := []struct {
		name string
		files []ManifestFile
	}{
		{name: "invalid hash", files: []ManifestFile{{Path: "file", Size: 1, SHA256: "abc"}}},
		{name: "duplicate path", files: []ManifestFile{
			{Path: "file", Size: 1, SHA256: validHash},
			{Path: "file", Size: 1, SHA256: validHash},
		}},
		{name: "path traversal", files: []ManifestFile{{Path: "../file", Size: 1, SHA256: validHash}}},
		{name: "empty inventory", files: nil},
		{name: "unsafe query character", files: []ManifestFile{{Path: "file?x", Size: 1, SHA256: validHash}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := MakeManifest(&DatasetMeta{Name: "dataset", CaptureDate: "2026-11-23", Type: "test"}, tc.files); err == nil {
				t.Fatal("MakeManifest accepted invalid local file entries")
			}
		})
	}
}

func TestRemoteCommitRejectsInvalidManifestFileEntries(t *testing.T) {
	validHash := strings.Repeat("a", 64)
	tests := []struct {
		name string
		files []ManifestFile
	}{
		{name: "empty path", files: []ManifestFile{{Path: "", Size: 1, SHA256: validHash}}},
		{name: "parent traversal", files: []ManifestFile{{Path: "../secret", Size: 1, SHA256: validHash}}},
		{name: "absolute path", files: []ManifestFile{{Path: "/root/file", Size: 1, SHA256: validHash}}},
		{name: "backslash path", files: []ManifestFile{{Path: "dir\\\\file", Size: 1, SHA256: validHash}}},
		{name: "negative size", files: []ManifestFile{{Path: "file", Size: -1, SHA256: validHash}}},
		{name: "invalid sha256", files: []ManifestFile{{Path: "file", Size: 1, SHA256: "not-a-hash"}}},
		{name: "duplicate path", files: []ManifestFile{
			{Path: "file", Size: 1, SHA256: validHash},
			{Path: "file", Size: 1, SHA256: validHash},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := fakeCommittedManifest(t, Manifest{
				SchemaVersion: 1,
				DatasetID: "dataset",
				CaptureDate: "2026-11-23",
				DatasetType: "test",
				Files: tc.files,
				CreatedAt: "2026-11-23T00:00:00.000Z",
			})
			if _, err := RemoteCommit(fake, "oss://test/dataset/"); err == nil {
				t.Fatal("RemoteCommit accepted invalid manifest file entries")
			}
		})
	}
}

func TestRemoteCommitRejectsUnsupportedManifestSchema(t *testing.T) {
	manifestBytes := []byte(`{"schema_version":99,"dataset_id":"dataset","capture_date":"2026-11-23","dataset_type":"test","files":[],"revision":"raw-test","created_at":"2026-11-23T00:00:00.000Z"}`)
	sum := sha256.Sum256(manifestBytes)
	marker := ReadyMarker{
		SchemaVersion:  1,
		Revision:       "raw-test",
		ManifestSHA256: hex.EncodeToString(sum[:]),
	}
	markerBytes, err := jsonMarshalNoEscape(marker, "  ")
	if err != nil {
		t.Fatal(err)
	}
	fake := &verifyFakeOSS{objects: map[string][]byte{
		"oss://test/dataset/manifest.json": manifestBytes,
		"oss://test/dataset/_READY":         markerBytes,
	}}
	if _, err := RemoteCommit(fake, "oss://test/dataset/"); err == nil {
		t.Fatal("RemoteCommit accepted an unsupported manifest schema")
	}
}

func TestVerifyOneRejectsMismatchedReadyManifest(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "payload.bin"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := "oss://test/dataset/"
	fake := &verifyFakeOSS{
		objects: map[string][]byte{
			raw + "_READY":      []byte(`{"schema_version":1,"revision":"raw-test","manifest_sha256":"not-the-manifest-hash"}`),
			raw + "manifest.json": []byte(`{"schema_version":1,"revision":"raw-test"}`),
		},
		files: []RemoteObj{{Name: "payload.bin", Size: 4}},
	}
	ds := &DatasetMeta{Source: src, RawURI: raw}
	if _, err := VerifyOne(fake, ds, false, nil); err == nil {
		t.Fatal("VerifyOne accepted a _READY marker whose manifest checksum does not match")
	}
}

func TestVerifyOneFlagsMissingRemoteObjectsWhenReadyExists(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "payload.bin"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := fakeCommittedManifest(t, Manifest{
		SchemaVersion: 1,
		DatasetID: "dataset",
		CaptureDate: "2026-11-23",
		DatasetType: "test",
		Files: []ManifestFile{{Path: "payload.bin", Size: 4, SHA256: strings.Repeat("a", 64)}},
		CreatedAt: "2026-11-23T00:00:00.000Z",
	})
	result, err := VerifyOne(fake, &DatasetMeta{
		Name: "dataset", CaptureDate: "2026-11-23", Type: "test",
		Source: src, RawURI: "oss://test/dataset/",
	}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "不一致" || len(result.Missing) != 1 {
		t.Fatalf("missing remote objects were hidden by empty inventory: status=%q missing=%v", result.Status, result.Missing)
	}
}

func TestVerifyOneDetectsManifestInventoryMismatchWithoutDeepHash(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "payload.bin"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := fakeCommittedManifest(t, Manifest{
		SchemaVersion: 1,
		DatasetID: "dataset",
		CaptureDate: "2026-11-23",
		DatasetType: "test",
		Files: []ManifestFile{{Path: "other.bin", Size: 4, SHA256: strings.Repeat("a", 64)}},
		CreatedAt: "2026-11-23T00:00:00.000Z",
	})
	fake.files = []RemoteObj{{Name: "payload.bin", Size: 4}}
	result, err := VerifyOne(fake, &DatasetMeta{Name: "dataset", CaptureDate: "2026-11-23", Type: "test", Source: src, RawURI: "oss://test/dataset/"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "不一致" || len(result.ManifestDiff) != 2 {
		t.Fatalf("manifest mismatch was not reported: status=%q diff=%v", result.Status, result.ManifestDiff)
	}
}

func TestVerifyOneDetectsManifestDatasetIdentityMismatch(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "payload.bin"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := fakeCommittedManifest(t, Manifest{
		SchemaVersion: 1,
		DatasetID: "different-dataset",
		CaptureDate: "2026-11-23",
		DatasetType: "test",
		Files: []ManifestFile{{Path: "payload.bin", Size: 4, SHA256: strings.Repeat("a", 64)}},
		CreatedAt: "2026-11-23T00:00:00.000Z",
	})
	fake.files = []RemoteObj{{Name: "payload.bin", Size: 4}}
	result, err := VerifyOne(fake, &DatasetMeta{
		Name: "dataset", CaptureDate: "2026-11-23", Type: "test",
		Source: src, RawURI: "oss://test/dataset/",
	}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "不一致" || len(result.ManifestDiff) != 1 || result.ManifestDiff[0] != "dataset_id" {
		t.Fatalf("manifest identity mismatch was not reported: status=%q diff=%v", result.Status, result.ManifestDiff)
	}
}

func TestVerifyOneReportsMissingReadyWhenNoCommitExists(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "payload.bin"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &verifyFakeOSS{
		objects: map[string][]byte{},
		files:   []RemoteObj{{Name: "payload.bin", Size: 4}},
	}
	result, err := VerifyOne(fake, &DatasetMeta{Source: src, RawURI: "oss://test/dataset/"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Ready || result.Status != "缺_READY" {
		t.Fatalf("unexpected verification result: ready=%v status=%q", result.Ready, result.Status)
	}
}

func TestCommitDoesNotWriteReadyWhenManifestReadbackDiffers(t *testing.T) {
	raw := "oss://test/dataset/"
	fake := &verifyFakeOSS{
		objects:            map[string][]byte{},
		files:              []RemoteObj{{Name: "payload.bin", Size: 4}},
		tamperManifestRead: true,
	}
	ds := &DatasetMeta{RawURI: raw, Name: "dataset", CaptureDate: "2026-11-23", Type: "test"}
	files := []ManifestFile{{Path: "payload.bin", Size: 4, SHA256: strings.Repeat("a", 64)}}
	if _, _, err := Commit(fake, ds, files); err == nil {
		t.Fatal("Commit accepted a manifest that failed read-back verification")
	}
	for _, key := range fake.writes {
		if strings.HasSuffix(key, "/_READY") {
			t.Fatalf("Commit wrote _READY despite manifest read-back failure: %v", fake.writes)
		}
	}
}

func TestCommitWritesReadyOnlyAfterVerifiedManifest(t *testing.T) {
	raw := "oss://test/dataset/"
	fake := &verifyFakeOSS{
		objects: map[string][]byte{},
		files:   []RemoteObj{{Name: "payload.bin", Size: 4}},
	}
	ds := &DatasetMeta{RawURI: raw, Name: "dataset", CaptureDate: "2026-11-23", Type: "test"}
	files := []ManifestFile{{Path: "payload.bin", Size: 4, SHA256: strings.Repeat("a", 64)}}
	status, revision, err := Commit(fake, ds, files)
	if err != nil {
		t.Fatal(err)
	}
	if status != "成功" || revision == "" {
		t.Fatalf("unexpected commit result: status=%q revision=%q", status, revision)
	}
	if len(fake.writes) != 2 || fake.writes[0] != raw+"manifest.json" || fake.writes[1] != raw+"_READY" {
		t.Fatalf("unexpected commit write order: %v", fake.writes)
	}
	commit, err := RemoteCommit(fake, raw)
	if err != nil {
		t.Fatal(err)
	}
	if commit == nil || commit.Manifest.Revision != revision {
		t.Fatalf("committed manifest not recoverable: %#v", commit)
	}
}


func TestDoUploadRejectsExistingCommitWithSameSizeDifferentContent(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "payload.bin"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	otherHash := sha256.Sum256([]byte("evil"))
	fake := fakeCommittedManifest(t, Manifest{
		SchemaVersion: 1,
		DatasetID: "dataset",
		CaptureDate: "2026-11-23",
		DatasetType: "test",
		Files: []ManifestFile{{
			Path: "payload.bin",
			Size: 4,
			SHA256: hex.EncodeToString(otherHash[:]),
		}},
		CreatedAt: "2026-11-23T00:00:00.000Z",
	})
	fake.files = []RemoteObj{{Name: "payload.bin", Size: 4}}

	_, _, _, err := DoUpload(nil, fake, nil, &DatasetMeta{
		Source: src,
		Name: "dataset",
		CaptureDate: "2026-11-23",
		Type: "test",
		RawURI: "oss://test/dataset/",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "不同内容") {
		t.Fatalf("DoUpload error = %v, want different-content rejection", err)
	}
}

func TestDoUploadSkipsExistingCommitOnlyWhenSHA256Matches(t *testing.T) {
	src := t.TempDir()
	payload := []byte("data")
	if err := os.WriteFile(filepath.Join(src, "payload.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	localHash := sha256.Sum256(payload)
	fake := fakeCommittedManifest(t, Manifest{
		SchemaVersion: 1,
		DatasetID: "dataset",
		CaptureDate: "2026-11-23",
		DatasetType: "test",
		Files: []ManifestFile{{
			Path: "payload.bin",
			Size: int64(len(payload)),
			SHA256: hex.EncodeToString(localHash[:]),
		}},
		CreatedAt: "2026-11-23T00:00:00.000Z",
	})
	fake.files = []RemoteObj{{Name: "payload.bin", Size: int64(len(payload))}}

	oldCheckpointDir := CkptDir
	CkptDir = filepath.Join(t.TempDir(), "checkpoints")
	defer func() { CkptDir = oldCheckpointDir }()

	status, _, count, err := DoUpload(nil, fake, nil, &DatasetMeta{
		Source: src,
		Name: "dataset",
		CaptureDate: "2026-11-23",
		Type: "test",
		RawURI: "oss://test/dataset/",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != "已存在" || count != 1 {
		t.Fatalf("DoUpload result = status %q, count %d; want 已存在, 1", status, count)
	}
}
