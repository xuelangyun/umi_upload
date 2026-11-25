package umi

import (
	"os"
	"runtime"
	"sync"
)

// scanStatsBatch collects results for one directory. The directory remains
// open until wg reaches zero, so workers can safely use its descriptor.
type scanStatsBatch struct {
	wg sync.WaitGroup
	mu sync.Mutex

	totalSize int64
	slices    int
	minHead   float64
	maxTail   float64
	hasHead   bool
	duration  float64
}

type scanStatsFileJob struct {
	dir   scanDirectory
	entry os.DirEntry
	shard bool
	batch *scanStatsBatch
}

// Inspect already runs several datasets concurrently. This process-wide limit
// keeps their nested file-stat workers from creating unbounded disk pressure.
var scanStatsMetadataSlots = make(chan struct{}, scanStatsMetadataConcurrency())

func scanStatsMetadataConcurrency() int {
	n := runtime.NumCPU() * 2
	if n < 8 {
		n = 8
	}
	if n > 64 {
		n = 64
	}
	return n
}

func scanStatsWorkerCount() int {
	n := runtime.NumCPU()
	if n < 2 {
		n = 2
	}
	if n > 8 {
		n = 8
	}
	return n
}

func processScanStatsFile(job scanStatsFileJob) {
	defer job.batch.wg.Done()
	if AppCtx.Err() != nil {
		return
	}

	select {
	case scanStatsMetadataSlots <- struct{}{}:
	case <-AppCtx.Done():
		return
	}
	defer func() { <-scanStatsMetadataSlots }()

	size, err := job.dir.entrySize(job.entry)
	if err != nil {
		return
	}

	var result scanStatsBatch
	result.totalSize = size
	if job.shard {
		result.slices++
		if file, openErr := job.dir.openEntry(job.entry); openErr == nil {
			head, tail, ok := parseShardTimesFromFile(file, size)
			_ = file.Close()
			if ok {
				result.minHead = head
				result.maxTail = tail
				result.hasHead = true
				if tail > head {
					result.duration = tail - head
				}
			}
		}
	}

	job.batch.mu.Lock()
	job.batch.totalSize += result.totalSize
	job.batch.slices += result.slices
	if result.hasHead {
		if !job.batch.hasHead || result.minHead < job.batch.minHead {
			job.batch.minHead = result.minHead
		}
		if !job.batch.hasHead || result.maxTail > job.batch.maxTail {
			job.batch.maxTail = result.maxTail
		}
		job.batch.hasHead = true
	}
	job.batch.duration += result.duration
	job.batch.mu.Unlock()
}
