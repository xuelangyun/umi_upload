package umi

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var directoryScanBenchmarkSink []os.DirEntry

func TestReadDirDirectoriesMatchesFullReadDirFiltering(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"zeta", "Alpha", "middle", "beta"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 1400; i++ {
		name := filepath.Join(root, fmt.Sprintf("file-%04d.bin", i))
		if err := os.WriteFile(name, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	all, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, insensitive := range []bool{false, true} {
		want := make([]os.DirEntry, 0, 4)
		for _, entry := range all {
			if entry.IsDir() {
				want = append(want, entry)
			}
		}
		if insensitive {
			sort.Slice(want, func(i, j int) bool {
				return strings.ToLower(want[i].Name()) < strings.ToLower(want[j].Name())
			})
		}

		got, count, err := readDirDirectories(root, insensitive)
		if err != nil {
			t.Fatalf("readDirDirectories(caseInsensitive=%t): %v", insensitive, err)
		}
		if count != len(all) {
			t.Fatalf("entry count = %d, want %d", count, len(all))
		}
		if len(got) != len(want) {
			t.Fatalf("directory count = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i].Name() != want[i].Name() {
				t.Fatalf("directory[%d] = %q, want %q", i, got[i].Name(), want[i].Name())
			}
		}
	}
}

func BenchmarkReadDirDirectoryComparison(b *testing.B) {
	root := b.TempDir()
	for i := 0; i < 64; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("subdir-%03d", i)), 0o755); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < 4096; i++ {
		name := filepath.Join(root, fmt.Sprintf("payload-%05d.bin", i))
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	all, err := os.ReadDir(root)
	if err != nil {
		b.Fatal(err)
	}
	want := make([]string, 0, 64)
	for _, entry := range all {
		if entry.IsDir() {
			want = append(want, entry.Name())
		}
	}
	if len(want) != 64 {
		b.Fatalf("fixture directories = %d, want 64", len(want))
	}

	b.Run("full_read_dir_then_filter", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(float64(len(all)), "entries")
		for i := 0; i < b.N; i++ {
			entries, err := os.ReadDir(root)
			if err != nil {
				b.Fatal(err)
			}
			dirs := make([]os.DirEntry, 0, len(want))
			for _, entry := range entries {
				if entry.IsDir() {
					dirs = append(dirs, entry)
				}
			}
			directoryScanBenchmarkSink = dirs
		}
	})
	b.Run("stream_directories_only", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(float64(len(all)), "entries")
		for i := 0; i < b.N; i++ {
			dirs, _, err := readDirDirectories(root, false)
			if err != nil {
				b.Fatal(err)
			}
			directoryScanBenchmarkSink = dirs
		}
	})
}
