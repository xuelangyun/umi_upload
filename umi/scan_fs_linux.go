//go:build linux

package umi

import (
	"os"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// scanDirectory keeps a directory descriptor open so metadata lookups can be
// relative to that directory instead of resolving a full path for every entry.
type scanDirectory interface {
	entries() []os.DirEntry
	metadata(os.DirEntry) (size int64, modTime int64, err error)
	modTime() (int64, error)
	close()
}

type linuxScanDirectory struct {
	file    *os.File
	entriesList []os.DirEntry
}

func openScanDirectory(path string, cached []os.DirEntry) (scanDirectory, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	entries := cached
	if entries == nil {
		entries, err = f.ReadDir(-1)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		// os.ReadDir sorts by filename. Keep exactly that ordering for stable
		// traversal and identical aggregation behavior.
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Name() < entries[j].Name()
		})
	}

	return &linuxScanDirectory{file: f, entriesList: entries}, nil
}

func (d *linuxScanDirectory) entries() []os.DirEntry {
	return d.entriesList
}

func (d *linuxScanDirectory) metadata(entry os.DirEntry) (int64, int64, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(int(d.file.Fd()), entry.Name(), &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, 0, err
	}
	modTime := time.Unix(int64(st.Mtim.Sec), int64(st.Mtim.Nsec)).UnixNano()
	return st.Size, modTime, nil
}

func (d *linuxScanDirectory) modTime() (int64, error) {
	info, err := d.file.Stat()
	if err != nil {
		return 0, err
	}
	return info.ModTime().UnixNano(), nil
}

func (d *linuxScanDirectory) close() {
	_ = d.file.Close()
}
