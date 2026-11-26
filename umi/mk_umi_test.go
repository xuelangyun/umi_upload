package umi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMKUMIDiscoveryUsesUMIBucketAndMechUMIPrefix(t *testing.T) {
	root := t.TempDir()
	typeDir := filepath.Join(root, "MK_UMI")
	sessionName := "UMI_v3_20261126_134100"
	session := filepath.Join(typeDir, sessionName)
	for _, dir := range []string{
		filepath.Join(session, "metadata"),
		filepath.Join(session, "head"),
		filepath.Join(session, "left hand"),
		filepath.Join(session, "right hand"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(session, "metadata", "conf.yaml"), []byte("device_ID: test\ndevice_SN: test-sn\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	found := FindDirs(root, 3)
	if len(found) != 1 || found[0].Type != "MK_UMI" || found[0].Path != typeDir {
		t.Fatalf("FindDirs() = %#v, want MK_UMI at %q", found, typeDir)
	}

	cfg := copyStringMap(DefaultSettings)
	cfg["bucket_umi"] = "prt-huawei"
	cfg["bucket"] = "hw-upload"
	datasets, problems := Discover([]RouteSpec{{Type: found[0].Type, Path: found[0].Path, City: "XZ", Room: "001"}}, cfg)
	if len(problems) != 0 {
		t.Fatalf("Discover() problems = %v", problems)
	}
	if len(datasets) != 1 {
		t.Fatalf("Discover() returned %d datasets, want 1", len(datasets))
	}
	ds := datasets[0]
	wantURI := "oss://prt-huawei/raw_data/raw_data_261126/mech_umi/PRT_XZ_room001/" + sessionName + "/"
	if ds.Type != "MK_UMI" || ds.Fmt != "umi_v3" || ds.Bucket != "prt-huawei" || ds.RawURI != wantURI {
		t.Fatalf("dataset mapping = type:%q format:%q bucket:%q uri:%q; want MK_UMI / umi_v3 / prt-huawei / %q",
			ds.Type, ds.Fmt, ds.Bucket, ds.RawURI, wantURI)
	}
}

func TestMKUMIMechUMIAliasIsRecognized(t *testing.T) {
	root := t.TempDir()
	typeDir := filepath.Join(root, "mech_umi")
	if err := os.MkdirAll(typeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	found := FindDirs(root, 3)
	if len(found) != 1 || found[0].Type != "MK_UMI" || found[0].Path != typeDir {
		t.Fatalf("FindDirs() = %#v, want MK_UMI alias at %q", found, typeDir)
	}
}
