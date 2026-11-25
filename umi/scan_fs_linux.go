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
	entrySize(os.DirEntry) (int64, error)
	entryModTime(os.DirEntry) (int64, error)
	openEntry(os.DirEntry) (*os.File, error)
	modTime() (int64, error)
	close()
}

type linuxScanDirectory struct {
	file        *os.File
	fd          int
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

	return &linuxScanDirectory{file: f, fd: int(f.Fd()), entriesList: entries}, nil
}

func (d *linuxScanDirectory) entries() []os.DirEntry {
	return d.entriesList
}

func (d *linuxScanDirectory) stat(entry os.DirEntry) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstatat(d.fd, entry.Name(), &st, unix.AT_SYMLINK_NOFOLLOW)
	return st, err
}

func (d *linuxScanDirectory) entrySize(entry os.DirEntry) (int64, error) {
	st, err := d.stat(entry)
	if err != nil {
		return 0, err
	}
	return st.Size, nil
}

func (d *linuxScanDirectory) entryModTime(entry os.DirEntry) (int64, error) {
	st, err := d.stat(entry)
	if err != nil {
		return 0, err
	}
	return time.Unix(int64(st.Mtim.Sec), int64(st.Mtim.Nsec)).UnixNano(), nil
}

func (d *linuxScanDirectory) openEntry(entry os.DirEntry) (*os.File, error) {
	fd, err := unix.Openat(d.fd, entry.Name(), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), entry.Name()), nil
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
