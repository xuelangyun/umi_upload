package umi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareUploadCheckpointCreatesAndDetectsResume(t *testing.T) {
	oldDir := CkptDir
	CkptDir = filepath.Join(t.TempDir(), "checkpoints")
	defer func() { CkptDir = oldDir }()

	ds := &DatasetMeta{RawURI: "oss://bucket/session/"}
	inv := []InvItem{{Rel: "camera/video.mp4", Size: 42, Sig: fileSig{Mtime: 123}}}

	root, sub, resumed, err := prepareUploadCheckpoint(ds, inv)
	if err != nil {
		t.Fatalf("first prepareUploadCheckpoint: %v", err)
	}
	if resumed {
		t.Fatal("new checkpoint directory must not be treated as a resume")
	}
	if root != CkptRoot(ds) || sub != filepath.Join(root, datasetInvSig(inv)) {
		t.Fatalf("checkpoint paths = %q, %q", root, sub)
	}
	info, err := os.Stat(filepath.Join(sub, "started"))
	if err != nil {
		t.Fatalf("started marker was not created: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("started marker permissions = %04o, want 0600", info.Mode().Perm())
	}

	rootAgain, subAgain, resumed, err := prepareUploadCheckpoint(ds, inv)
	if err != nil {
		t.Fatalf("second prepareUploadCheckpoint: %v", err)
	}
	if !resumed {
		t.Fatal("existing started marker must be treated as a resume")
	}
	if rootAgain != root || subAgain != sub {
		t.Fatalf("resume paths changed: %q, %q", rootAgain, subAgain)
	}
}

func TestPrepareUploadCheckpointReturnsFilesystemErrors(t *testing.T) {
	oldDir := CkptDir
	defer func() { CkptDir = oldDir }()

	t.Run("checkpoint root cannot be created", func(t *testing.T) {
		CkptDir = filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(CkptDir, []byte("block"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := prepareUploadCheckpoint(&DatasetMeta{RawURI: "oss://bucket/a/"}, nil)
		if err == nil {
			t.Fatal("expected checkpoint directory creation error")
		}
	})

	t.Run("started marker cannot be written", func(t *testing.T) {
		CkptDir = filepath.Join(t.TempDir(), "checkpoints")
		ds := &DatasetMeta{RawURI: "oss://bucket/b/"}
		inv := []InvItem{{Rel: "a.bin", Size: 1}}
		sub := filepath.Join(CkptRoot(ds), datasetInvSig(inv))
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(sub, "started"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := prepareUploadCheckpoint(ds, inv)
		if err == nil {
			t.Fatal("expected started marker write error")
		}
	})
}
