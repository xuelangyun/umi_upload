package umi

import (
	"io"
	"os"
	"sort"
	"strings"
)

// readDirDirectories reads a directory in bounded batches and retains only
// subdirectories. Discovery needs file entries only when the current path is
// itself a recognized dataset root; ordinary parent directories need only
// their child directories.
func readDirDirectories(path string, caseInsensitive bool) ([]os.DirEntry, int, error) {
	dir, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer dir.Close()

	const batchSize = 256
	var dirs []os.DirEntry
	entryCount := 0
	for {
		entries, readErr := dir.ReadDir(batchSize)
		entryCount += len(entries)
		for _, entry := range entries {
			if entry.IsDir() {
				dirs = append(dirs, entry)
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return nil, entryCount, readErr
		}
		if len(entries) == 0 {
			break
		}
		if AppCtx.Err() != nil {
			return nil, entryCount, AppCtx.Err()
		}
	}

	if caseInsensitive {
		sort.Slice(dirs, func(i, j int) bool {
			return strings.ToLower(dirs[i].Name()) < strings.ToLower(dirs[j].Name())
		})
	} else {
		sort.Slice(dirs, func(i, j int) bool {
			return dirs[i].Name() < dirs[j].Name()
		})
	}
	return dirs, entryCount, nil
}
