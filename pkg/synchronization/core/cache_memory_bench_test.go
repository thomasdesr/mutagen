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

// newSyntheticCacheAndIgnoreCache builds a Cache and IgnoreCache shaped like
// a large monorepo checkout: deeply nested paths with repeated directory
// segments and basenames, averaging roughly 60-100 characters per path. Each
// file gets its own CacheEntry (with a distinct *timestamppb.Timestamp and
// digest, mirroring what proto unmarshaling produces on load) and a
// corresponding IgnoreCache entry; each unique parent directory also gets an
// IgnoreCache entry, mirroring real scan behavior.
func newSyntheticCacheAndIgnoreCache(fileCount int) (*Cache, IgnoreCache) {
	topDirs := []string{"backend", "frontend", "go", "infra", "shared_anyscale_utils", "tools", "cicd", "schema_registry"}
	midDirs := []string{"server", "database", "clusteroperator", "controllers", "workload_scheduler", "streaming", "kubernetes_manager", "adminzonemanager"}
	leafDirs := []string{"handlers", "reconcilers", "daos", "services", "models", "utils", "config", "middleware"}
	basenames := []string{
		"reconciler.go", "handler_test.go", "service_impl.go", "config_loader.go",
		"database_client.go", "permission_checker.go", "event_consumer.go", "model_definitions.go",
	}

	entries := make(map[string]*CacheEntry, fileCount)
	ignoreCache := make(IgnoreCache, fileCount*2)
	baseTime := time.Now()

	for i := 0; i < fileCount; i++ {
		top := topDirs[i%len(topDirs)]
		mid := midDirs[(i/len(topDirs))%len(midDirs)]
		leaf := leafDirs[(i/(len(topDirs)*len(midDirs)))%len(leafDirs)]
		module := fmt.Sprintf("module_%04d", i%500)
		base := basenames[i%len(basenames)]

		directory := fmt.Sprintf("%s/%s/%s/%s", top, mid, leaf, module)
		path := directory + "/" + base

		digest := make([]byte, 16)
		digest[0], digest[1] = byte(i), byte(i>>8)

		entries[path] = &CacheEntry{
			Mode:             0100644,
			ModificationTime: timestamppb.New(baseTime.Add(time.Duration(i) * time.Second)),
			Size:             uint64(1000 + i%9000),
			Digest:           digest,
		}
		ignoreCache[IgnoreCacheKey{path: path, directory: false}] = false
		ignoreCache[IgnoreCacheKey{path: directory, directory: true}] = false
	}

	return &Cache{Entries: entries}, ignoreCache
}

// BenchmarkCacheMemoryFootprint measures the retained heap for a Cache and
// IgnoreCache pair shaped per newSyntheticCacheAndIgnoreCache. Run with
// -benchtime=1x for a clean single-sample reading, e.g.:
//
//	go test ./pkg/synchronization/core/ -run '^$' \
//	    -bench BenchmarkCacheMemoryFootprint -benchtime=1x
func BenchmarkCacheMemoryFootprint(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		cache, ignoreCache := newSyntheticCacheAndIgnoreCache(syntheticCacheFileCount)

		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		b.StartTimer()

		b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc), "retained-bytes")
		b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/float64(syntheticCacheFileCount), "retained-bytes/file")
		runtime.KeepAlive(cache)
		runtime.KeepAlive(ignoreCache)
	}
}
