package core

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// syntheticCacheFileCount is the number of file entries generated for the
// memory footprint benchmark, chosen to be large enough to make per-entry
// and per-key overhead dominate the measurement (real trees of concern are
// on the order of ~630k files).
const syntheticCacheFileCount = 150000

// syntheticEntry is synthetic per-file metadata, decoupled from any specific
// cache representation so the same generated data set can be used to build
// and directly compare both the flat (pre-compaction) and directory-sharded
// (post-compaction) cache shapes.
type syntheticEntry struct {
	directory        string
	name             string
	mode             uint32
	modificationTime time.Time
	size             uint64
	digest           []byte
}

// syntheticFilesPerDirectory is the number of files newSyntheticEntries
// places in each directory (one per basename) for the primary flat-vs-sharded
// comparison, chosen to mirror a moderately-populated source package rather
// than one file per directory. BenchmarkCacheMemoryFootprintByFanout sweeps
// this value to find where sharding stops paying off.
const syntheticFilesPerDirectory = 8

// newSyntheticEntries generates fileCount synthetic file entries shaped like
// a large monorepo checkout: deeply nested paths with repeated directory
// segments and basenames, averaging roughly 60-100 characters per path.
// Files are grouped filesPerDirectory-at-a-time into each directory, so
// directory strings are actually shared across entries (unlike a naive
// per-file-unique-directory generator, which would give the
// directory-sharded cache nothing to shard). filesPerDirectory must be at
// most len(basenames) (8): basenames are not reused within a directory, so a
// larger value would silently collide two files onto the same cache key.
func newSyntheticEntries(fileCount, filesPerDirectory int) []syntheticEntry {
	topDirs := []string{"backend", "frontend", "go", "infra", "shared_anyscale_utils", "tools", "cicd", "schema_registry"}
	midDirs := []string{"server", "database", "clusteroperator", "controllers", "workload_scheduler", "streaming", "kubernetes_manager", "adminzonemanager"}
	leafDirs := []string{"handlers", "reconcilers", "daos", "services", "models", "utils", "config", "middleware"}
	basenames := []string{
		"reconciler.go", "handler_test.go", "service_impl.go", "config_loader.go",
		"database_client.go", "permission_checker.go", "event_consumer.go", "model_definitions.go",
	}
	if filesPerDirectory > len(basenames) {
		panic(fmt.Sprintf("filesPerDirectory (%d) exceeds available basenames (%d)", filesPerDirectory, len(basenames)))
	}

	entries := make([]syntheticEntry, fileCount)
	baseTime := time.Now()

	for i := 0; i < fileCount; i++ {
		directoryIndex := i / filesPerDirectory
		top := topDirs[directoryIndex%len(topDirs)]
		mid := midDirs[(directoryIndex/len(topDirs))%len(midDirs)]
		leaf := leafDirs[(directoryIndex/(len(topDirs)*len(midDirs)))%len(leafDirs)]
		module := fmt.Sprintf("module_%04d", (directoryIndex/(len(topDirs)*len(midDirs)*len(leafDirs)))%500)
		directory := fmt.Sprintf("%s/%s/%s/%s", top, mid, leaf, module)

		digest := make([]byte, 16)
		digest[0], digest[1] = byte(i), byte(i>>8)

		entries[i] = syntheticEntry{
			directory:        directory,
			name:             basenames[i%filesPerDirectory],
			mode:             0100644,
			modificationTime: baseTime.Add(time.Duration(i) * time.Second),
			size:             uint64(1000 + i%9000),
			digest:           digest,
		}
	}

	return entries
}

// flatIgnoreCacheKey mirrors the pre-compaction IgnoreCacheKey (a full path
// plus a directory-ness flag), reconstructed here rather than in ignore.go
// since it exists solely to give this benchmark a pre-compaction shape to
// compare against.
type flatIgnoreCacheKey struct {
	path      string
	directory bool
}

// newFlatCacheAndIgnoreCache builds entries into the pre-compaction cache
// shapes: a Cache with entries keyed by full path, and an IgnoreCache keyed
// by (full path, directory-ness).
func newFlatCacheAndIgnoreCache(entries []syntheticEntry) (*Cache, map[flatIgnoreCacheKey]bool) {
	cacheEntries := make(map[string]*CacheEntry, len(entries))
	ignoreCache := make(map[flatIgnoreCacheKey]bool, len(entries)*2)

	for _, entry := range entries {
		path := entry.directory + "/" + entry.name
		cacheEntries[path] = &CacheEntry{
			Mode:             entry.mode,
			ModificationTime: timestamppb.New(entry.modificationTime),
			Size:             entry.size,
			Digest:           entry.digest,
		}
		ignoreCache[flatIgnoreCacheKey{path: path, directory: false}] = false
		ignoreCache[flatIgnoreCacheKey{path: entry.directory, directory: true}] = false
	}

	return &Cache{Entries: cacheEntries}, ignoreCache
}

// newShardedCacheAndIgnoreCache builds entries into the post-compaction
// cache shapes: a ScanCache and IgnoreCache, both sharded by directory.
func newShardedCacheAndIgnoreCache(entries []syntheticEntry) (*ScanCache, IgnoreCache) {
	directoryCountHint := countDistinctDirectories(entries)
	cache := newScanCache(directoryCountHint)
	ignoreCache := make(IgnoreCache, directoryCountHint)

	for _, entry := range entries {
		cache.set(entry.directory, entry.name, &ScanCacheEntry{
			mode:                    entry.mode,
			modificationTimeSeconds: entry.modificationTime.Unix(),
			modificationTimeNanos:   int32(entry.modificationTime.Nanosecond()),
			size:                    entry.size,
			digest:                  entry.digest,
		})
		ignoreCache.set(entry.directory+"/"+entry.name, false, false)
		ignoreCache.set(entry.directory, true, false)
	}

	return cache, ignoreCache
}

// countDistinctDirectories reports how many distinct directory strings
// appear across entries, used to size the outer per-directory maps of a
// sharded cache without over- or under-allocating relative to the actual
// data (rather than assuming a fixed files-per-directory constant that may
// not match the entries being sized).
func countDistinctDirectories(entries []syntheticEntry) int {
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		seen[entry.directory] = struct{}{}
	}
	return len(seen)
}

// reportCacheMemoryFootprint runs build (which must construct and return a
// cache/ignore-cache pair via keepAlive) between two GC-and-measure
// brackets, reporting the heap retained by the result. build receives the
// generated entries and is responsible for calling keepAlive on whatever it
// constructs from them and nothing else, so that dropping its local
// reference to entries (which reportCacheMemoryFootprint does before the
// final measurement) leaves only the constructed cache's own memory live.
func reportCacheMemoryFootprint(b *testing.B, filesPerDirectory int, build func(entries []syntheticEntry, keepAlive func(...any))) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		entries := newSyntheticEntries(syntheticCacheFileCount, filesPerDirectory)
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		var kept []any
		build(entries, func(values ...any) { kept = append(kept, values...) })
		entries = nil

		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		b.StartTimer()

		b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc), "retained-bytes")
		b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/float64(syntheticCacheFileCount), "retained-bytes/file")
		runtime.KeepAlive(kept)
	}
}

// BenchmarkCacheMemoryFootprintFlat measures the retained heap for the
// pre-compaction Cache/IgnoreCache shapes (full-path-keyed flat maps, heap
// allocated *timestamppb.Timestamp per entry). Run with -benchtime=1x for a
// clean single-sample reading, e.g.:
//
//	go test ./pkg/synchronization/core/ -run '^$' \
//	    -bench BenchmarkCacheMemoryFootprintFlat -benchtime=1x
func BenchmarkCacheMemoryFootprintFlat(b *testing.B) {
	reportCacheMemoryFootprint(b, syntheticFilesPerDirectory, func(entries []syntheticEntry, keepAlive func(...any)) {
		cache, ignoreCache := newFlatCacheAndIgnoreCache(entries)
		keepAlive(cache, ignoreCache)
	})
}

// BenchmarkCacheMemoryFootprintSharded measures the retained heap for the
// post-compaction ScanCache/IgnoreCache shapes (directory-sharded maps,
// inline modification-time fields). Run with -benchtime=1x for a clean
// single-sample reading, e.g.:
//
//	go test ./pkg/synchronization/core/ -run '^$' \
//	    -bench BenchmarkCacheMemoryFootprintSharded -benchtime=1x
func BenchmarkCacheMemoryFootprintSharded(b *testing.B) {
	reportCacheMemoryFootprint(b, syntheticFilesPerDirectory, func(entries []syntheticEntry, keepAlive func(...any)) {
		cache, ignoreCache := newShardedCacheAndIgnoreCache(entries)
		keepAlive(cache, ignoreCache)
	})
}

// BenchmarkCacheMemoryFootprintByFanout sweeps the average files-per-directory
// across both cache shapes, since directory-sharding only pays for its
// per-directory map overhead once fan-out is high enough: at 1 file per
// directory, sharding pays a full extra map's fixed cost per file for no
// string-sharing benefit, while at high fan-out that fixed cost amortizes
// over many entries. Run with -benchtime=1x, e.g.:
//
//	go test ./pkg/synchronization/core/ -run '^$' \
//	    -bench BenchmarkCacheMemoryFootprintByFanout -benchtime=1x
func BenchmarkCacheMemoryFootprintByFanout(b *testing.B) {
	for _, filesPerDirectory := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprintf("flat/fanout=%d", filesPerDirectory), func(b *testing.B) {
			reportCacheMemoryFootprint(b, filesPerDirectory, func(entries []syntheticEntry, keepAlive func(...any)) {
				cache, ignoreCache := newFlatCacheAndIgnoreCache(entries)
				keepAlive(cache, ignoreCache)
			})
		})
		b.Run(fmt.Sprintf("sharded/fanout=%d", filesPerDirectory), func(b *testing.B) {
			reportCacheMemoryFootprint(b, filesPerDirectory, func(entries []syntheticEntry, keepAlive func(...any)) {
				cache, ignoreCache := newShardedCacheAndIgnoreCache(entries)
				keepAlive(cache, ignoreCache)
			})
		})
	}
}
