package umi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUMIFrameTimesReusesDiscoveredRootEntries(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"metadata", "head", "left hand", "right hand"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	csvFiles := map[string]string{
		filepath.Join("head", "head.frames.csv"): "frame_id,exposure_ns\n0,1000000000\n1,2500000000\n",
		filepath.Join("left hand", "left.frames.csv"): "frame_id,exposure_ns\n0,1500000000\n1,4000000000\n",
	}
	for relative, body := range csvFiles {
		path := filepath.Join(root, relative)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	wantStart, wantEnd := UMIFrameTimes(root)
	gotStart, gotEnd := umiFrameTimes(root, entries)
	if wantStart == nil || wantEnd == nil || gotStart == nil || gotEnd == nil {
		t.Fatalf("frame times should be found: baseline=(%v,%v) optimized=(%v,%v)", wantStart, wantEnd, gotStart, gotEnd)
	}
	if *gotStart != *wantStart || *gotEnd != *wantEnd {
		t.Fatalf("frame times differ: optimized=(%v,%v) baseline=(%v,%v)", *gotStart, *gotEnd, *wantStart, *wantEnd)
	}

	wantDuration, wantStartText, wantEndText := UMITimes(root, "UMI_v3_20260101_120000")
	gotDuration, gotStartText, gotEndText := umiTimes(root, "UMI_v3_20260101_120000", entries)
	if gotDuration != wantDuration || gotStartText != wantStartText || gotEndText != wantEndText {
		t.Fatalf("UMI times differ: optimized=(%v,%q,%q) baseline=(%v,%q,%q)",
			gotDuration, gotStartText, gotEndText, wantDuration, wantStartText, wantEndText)
	}
}
