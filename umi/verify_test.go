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
		out = []byte(strings.Replace(string(out), "abc", "abd", 1))
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
		Files:         []ManifestFile{{Path: "payload.bin", Size: 4, SHA256: "abc"}},
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
	files := []ManifestFile{{Path: "payload.bin", Size: 4, SHA256: "abc"}}
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
	files := []ManifestFile{{Path: "payload.bin", Size: 4, SHA256: "abc"}}
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
