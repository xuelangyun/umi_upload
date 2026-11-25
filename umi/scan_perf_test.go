package umi

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	scanBenchmarkSignature string
	scanBenchmarkStats     SessionStats
)

// TestScanFastPathPreservesResults compares the new filesystem path with the
// previous os.ReadDir/DirEntry.Info implementation on exactly the same tree.
func TestScanFastPathPreservesResults(t *testing.T) {
	root, name := createScanComparisonFixture(t)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	wantSig := legacySessionSignatureForTest(root)
	gotSig := sessionSignature(root, entries)
	if gotSig != wantSig {
		t.Fatalf("cached session signature changed: optimized=%q legacy=%q", gotSig, wantSig)
	}
	if got := SessionSig(root); got != wantSig {
		t.Fatalf("uncached session signature changed: got=%q legacy=%q", got, wantSig)
	}

	wantStats := legacyStatsForTest(root, name, true)
	gotStats := statsWithEntries(root, name, true, entries)
	if gotStats != wantStats {
		t.Fatalf("cached scan statistics changed: optimized=%#v legacy=%#v", gotStats, wantStats)
	}
	if got := Stats(root, name, true); got != wantStats {
		t.Fatalf("uncached scan statistics changed: optimized=%#v legacy=%#v", got, wantStats)
	}
}

func TestInspectPathMetadataMatchesExistingHelpers(t *testing.T) {
	paths := []string{
		"/data/umi_normal/device-01/20261125/PRT_WX_room002/ISuit-V3_xxx_2026-11-25-10-10-10",
		"/data/umi_calibration/PRT_SZ_room12/ISuit-V3_xxx_2026-11-25-10-10-10",
		"/data/umi_normal/20261125/ISuit-V3_xxx_2026-11-25-10-10-10",
		"/data/UMI_v3/20261125/UMI_v3_20261125_101010",
		`D:\\capture\\umi_normal\\device-01\\20261125\\PRT_WX_room002\\ISuit-V3_xxx_2026-11-25-10-10-10`,
		`\\\\server\\share\\umi_calibration\\device-02\\PRT_BJ_room3\\ISuit-V3_xxx_2026-11-25-10-10-10`,
		"/",
		"",
	}

	for _, path := range paths {
		wantPRT, wantPRTDir := LocalPRT(path)
		wantDevice := DeviceDir(path)
		wantDate := ""
		_, parts := splitPathComponents(path)
		for _, part := range parts {
			if reDirDate8.MatchString(part) {
				wantDate = part
				break
			}
		}

		gotPRT, gotPRTDir, gotDevice, gotDate := inspectPathMetadata(path)
		if gotPRT != wantPRT || gotPRTDir != wantPRTDir || gotDevice != wantDevice || gotDate != wantDate {
			t.Errorf("inspectPathMetadata(%q) = (%q, %q, %q, %q), want (%q, %q, %q, %q)",
				path, gotPRT, gotPRTDir, gotDevice, gotDate,
				wantPRT, wantPRTDir, wantDevice, wantDate)
		}
	}
}

// BenchmarkScanFilesystemComparison runs both implementations against the
// same directory in each benchmark process and verifies identical results
// before timing.
//
// Synthetic fixture (about 2,500 files):
//   go test ./umi -run '^$' -bench '^BenchmarkScanFilesystemComparison$' -benchmem -count=5
//
// Real Linux dataset:
//   UMI_SCAN_BENCH_DIR=/data/ISuit-V3_xxx_20261125-101010 go test ./umi -run '^$' -bench '^BenchmarkScanFilesystemComparison$' -benchmem -count=5
//
// Optional overrides: UMI_SCAN_BENCH_NAME and UMI_SCAN_BENCH_SHARDS=0|1.
func BenchmarkScanFilesystemComparison(b *testing.B) {
	root, name, shards := scanBenchmarkDirectory(b)
	entries, err := os.ReadDir(root)
	if err != nil {
		b.Fatal(err)
	}
	legacySig := legacySessionSignatureForTest(root)
	legacyStats := legacyStatsForTest(root, name, shards)
	fastSig := sessionSignature(root, entries)
	fastStats := statsWithEntries(root, name, shards, entries)
	if fastSig != legacySig {
		b.Fatalf("signature mismatch: optimized=%q legacy=%q", fastSig, legacySig)
	}
	if fastStats != legacyStats {
		b.Fatalf("statistics mismatch: optimized=%#v legacy=%#v", fastStats, legacyStats)
	}

	fileCount := countScanFiles(b, root)
	b.ReportMetric(float64(fileCount), "files")
	b.Logf("same-directory comparison: root=%q files=%d shards=%t", root, fileCount, shards)

	b.Run("baseline_read_dir_info", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(float64(fileCount), "files")
		for i := 0; i < b.N; i++ {
			// Discovery reads the dataset root once. The old signature and Stats
			// then each read the root again.
			if _, err := os.ReadDir(root); err != nil {
				b.Fatal(err)
			}
			scanBenchmarkSignature = legacySessionSignatureForTest(root)
			scanBenchmarkStats = legacyStatsForTest(root, name, shards)
		}
	})

	b.Run("optimized_reuse_and_linux_fstatat", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(float64(fileCount), "files")
		for i := 0; i < b.N; i++ {
			// Both signature and Stats reuse the listing read by discovery.
			rootEntries, err := os.ReadDir(root)
			if err != nil {
				b.Fatal(err)
			}
			scanBenchmarkSignature = sessionSignature(root, rootEntries)
			scanBenchmarkStats = statsWithEntries(root, name, shards, rootEntries)
		}
	})
}

func scanBenchmarkDirectory(b *testing.B) (root, name string, shards bool) {
	b.Helper()
	root = strings.TrimSpace(os.Getenv("UMI_SCAN_BENCH_DIR"))
	if root == "" {
		name = "ISuit-V3_benchmark_20261125-101010"
		root = filepath.Join(b.TempDir(), name)
		createScanComparisonTree(b, root, name)
		return root, name, true
	}
	info, err := os.Stat(root)
	if err != nil {
		b.Fatalf("stat UMI_SCAN_BENCH_DIR: %v", err)
	}
	if !info.IsDir() {
		b.Fatalf("UMI_SCAN_BENCH_DIR is not a directory: %s", root)
	}
	name = strings.TrimSpace(os.Getenv("UMI_SCAN_BENCH_NAME"))
	if name == "" {
		name = filepath.Base(filepath.Clean(root))
	}
	if override := strings.TrimSpace(os.Getenv("UMI_SCAN_BENCH_SHARDS")); override != "" {
		shards = override == "1" || strings.EqualFold(override, "true")
	} else {
		shards = FmtOfName(name) == "isuit"
	}
	return root, name, shards
}

func createScanComparisonFixture(t testing.TB) (string, string) {
	t.Helper()
	name := "ISuit-V3_benchmark_20261125-101010"
	root := filepath.Join(t.TempDir(), name)
	createScanComparisonTree(t, root, name)
	return root, name
}

func createScanComparisonTree(t testing.TB, root, name string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte(strings.Repeat("x", 96))
	for camera := 0; camera < 8; camera++ {
		cameraDir := filepath.Join(root, fmt.Sprintf("camera_%02d", camera))
		if err := os.MkdirAll(cameraDir, 0o755); err != nil {
			t.Fatal(err)
		}
		shard := fmt.Sprintf("head_time: %d.25\ntail_time: %d.75\n", camera+1, camera+3)
		if err := os.WriteFile(filepath.Join(cameraDir, name+"_0000.yaml"), []byte(shard), 0o644); err != nil {
			t.Fatal(err)
		}
		for segment := 0; segment < 8; segment++ {
			segmentDir := filepath.Join(cameraDir, fmt.Sprintf("segment_%02d", segment))
			if err := os.MkdirAll(segmentDir, 0o755); err != nil {
				t.Fatal(err)
			}
			for file := 0; file < 40; file++ {
				data := payload[:32+(file%65)]
				path := filepath.Join(segmentDir, fmt.Sprintf("file_%03d.bin", file))
				if err := os.WriteFile(path, data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte("ignored marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	pipeline := filepath.Join(root, "_pipeline")
	if err := os.MkdirAll(pipeline, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pipeline, "ignored.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "top-level.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
}

func countScanFiles(t testing.TB, root string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// legacySessionSignatureForTest preserves the previous implementation as a
// correctness and benchmark baseline.
func legacySessionSignatureForTest(src string) string {
	st, err := os.Stat(src)
	if err != nil {
		return ""
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return ""
	}
	n := 0
	var newest int64
	for _, entry := range entries {
		if entry.IsDir() {
			n++
			if info, err := entry.Info(); err == nil {
				if modified := info.ModTime().UnixNano(); modified > newest {
					newest = modified
				}
			}
		}
	}
	return fmt.Sprintf("%d:%d:%d", st.ModTime().UnixNano(), n, newest)
}

// legacyStatsForTest is the previous Stats traversal, kept only for
// same-directory result and timing comparisons.
func legacyStatsForTest(src, name string, shards bool) SessionStats {
	var totalSize int64
	slices := 0
	var minHead, maxTail float64
	hasHead := false
	dur := 0.0

	type stackItem struct {
		dir   string
		depth int
	}
	stack := []stackItem{{dir: src, depth: 0}}
	shardPre := name + "_"

	for len(stack) > 0 {
		if AppCtx.Err() != nil {
			return SessionStats{}
		}
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := os.ReadDir(cur.dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if AppCtx.Err() != nil {
				return SessionStats{}
			}
			entryName := entry.Name()
			if entry.IsDir() {
				if entryName != "_pipeline" {
					stack = append(stack, stackItem{dir: filepath.Join(cur.dir, entryName), depth: cur.depth + 1})
				}
				continue
			}
			if entry.Type()&os.ModeSymlink != 0 || Markers[entryName] {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			totalSize += info.Size()
			if shards && cur.depth == 1 && strings.HasSuffix(entryName, ".yaml") && strings.HasPrefix(entryName, shardPre) {
				slices++
				if head, tail, ok := parseShardTimes(filepath.Join(cur.dir, entryName)); ok {
					if !hasHead || head < minHead {
						minHead = head
					}
					if !hasHead || tail > maxTail {
						maxTail = tail
					}
					hasHead = true
					if tail > head {
						dur += tail - head
					}
				}
			}
		}
	}

	var start, end string
	if hasHead {
		start = FmtTS(minHead)
		end = FmtTS(maxTail)
	}
	return SessionStats{
		Size:     totalSize,
		Slices:   slices,
		Duration: math.Round(dur*100) / 100,
		Start:    start,
		End:      end,
	}
}
