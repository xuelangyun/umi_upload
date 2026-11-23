package umi

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type verifyFakeOSS struct {
	objects map[string][]byte
	files   []RemoteObj
}

func (f *verifyFakeOSS) BucketName() string { return "test" }
func (f *verifyFakeOSS) KeyOf(uri string) (string, error) { return uri, nil }
func (f *verifyFakeOSS) PutObject(uri string, data []byte) error {
	if f.objects == nil {
		f.objects = make(map[string][]byte)
	}
	f.objects[uri] = append([]byte(nil), data...)
	return nil
}
func (f *verifyFakeOSS) ReadObject(uri string) ([]byte, bool, error) {
	b, ok := f.objects[uri]
	return append([]byte(nil), b...), ok, nil
}
func (f *verifyFakeOSS) ListObjects(string) ([]RemoteObj, error) {
	return append([]RemoteObj(nil), f.files...), nil
}
func (f *verifyFakeOSS) UploadFile(string, string, int64, string, func(int64), func(int64)) error {
	return errors.New("not implemented")
}
func (f *verifyFakeOSS) TestBucket() error { return nil }
func (f *verifyFakeOSS) SetNetCallback(NetWaitCallback) {}

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
