//go:build !linux

package umi

import (
	"os"
	"path/filepath"
)

// scanDirectory provides a portable fallback. Linux uses directory-relative
// fstatat lookups in scan_fs_linux.go.
type scanDirectory interface {
	entries() []os.DirEntry
	entrySize(os.DirEntry) (int64, error)
	entryModTime(os.DirEntry) (int64, error)
	openEntry(os.DirEntry) (*os.File, error)
	modTime() (int64, error)
	close()
}

type portableScanDirectory struct {
	path        string
	entriesList []os.DirEntry
}

func openScanDirectory(path string, cached []os.DirEntry) (scanDirectory, error) {
	entries := cached
	if entries == nil {
		var err error
		entries, err = os.ReadDir(path)
		if err != nil {
			return nil, err
		}
	}
	return &portableScanDirectory{path: path, entriesList: entries}, nil
}

func (d *portableScanDirectory) entries() []os.DirEntry {
	return d.entriesList
}

func (d *portableScanDirectory) entrySize(entry os.DirEntry) (int64, error) {
	info, err := entry.Info()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (d *portableScanDirectory) entryModTime(entry os.DirEntry) (int64, error) {
	info, err := entry.Info()
	if err != nil {
		return 0, err
	}
	return info.ModTime().UnixNano(), nil
}

func (d *portableScanDirectory) openEntry(entry os.DirEntry) (*os.File, error) {
	return os.Open(filepath.Join(d.path, entry.Name()))
}

func (d *portableScanDirectory) modTime() (int64, error) {
	info, err := os.Stat(d.path)
	if err != nil {
		return 0, err
	}
	return info.ModTime().UnixNano(), nil
}

func (d *portableScanDirectory) close() {}
