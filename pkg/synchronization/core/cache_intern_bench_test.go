// This file holds the retained-memory measurement for cache interning; run it
// with
//
//	go test ./pkg/synchronization/core/ -run '^$' \
//	    -bench BenchmarkCacheInterningRetention -benchtime=1x
package core

import (
	"fmt"
	"runtime"
	"testing"
)

// BenchmarkCacheInterningRetention measures the heap retained by N
// independently-built but identical ScanCache/IgnoreCache pairs, with and
// without interning. N models both duplications the change targets: two
// sessions over one root, and one endpoint's successive cache generations.
//
// The reading: retained-bytes/entry normalizes by N x file count, so the flat
// arm should hold roughly flat across N (each copy costs a full copy) while the
// interned arm should fall towards 1/N of it (the shards collapse and only the
// entry maps of the first copy survive). Absolute numbers depend on platform
// and allocator; the ratio between the arms is the result. The interned arm
// retains the table itself, so its footprint counts against the saving, which
// is the daemon-wide configuration rather than a best case.
//
// It understates the production win in one direction worth stating: all N copies
// are built from one generated entry slice, so directory and basename strings
// are already shared in both arms. Two real scans allocate those strings
// separately, and interning a shard collapses its keys too.
func BenchmarkCacheInterningRetention(b *testing.B) {
	for _, copies := range []int{1, 2, 4} {
		b.Run(fmt.Sprintf("flat/copies=%d", copies), func(b *testing.B) {
			reportCacheInterningRetention(b, copies, func(entries []syntheticEntry, keepAlive func(...any)) {
				for i := 0; i < copies; i++ {
					cache, ignoreCache := newShardedCacheAndIgnoreCache(entries)
					keepAlive(cache, ignoreCache)
				}
			})
		})
		b.Run(fmt.Sprintf("interned/copies=%d", copies), func(b *testing.B) {
			reportCacheInterningRetention(b, copies, func(entries []syntheticEntry, keepAlive func(...any)) {
				interner := &CacheInterner{}
				keepAlive(interner)
				for i := 0; i < copies; i++ {
					// Only the interned results are anchored: keeping the source
					// caches alive would pin the very shards interning drops.
					cache, ignoreCache := newShardedCacheAndIgnoreCache(entries)
					keepAlive(interner.InternScanCache(cache), interner.InternIgnoreCache(ignoreCache))
				}
			})
		})
	}
}

// reportCacheInterningRetention runs build between two GC-and-measure brackets
// and reports the heap it retains, normalized by the number of cache entries
// built across all copies. It mirrors reportCacheMemoryFootprint, differing only
// in that copies-many caches are built per iteration and the per-entry
// normalization accounts for them; build is likewise responsible for anchoring
// what it constructs via keepAlive and nothing else, since the generated entry
// slice is dropped before the final measurement.
func reportCacheInterningRetention(b *testing.B, copies int, build func(entries []syntheticEntry, keepAlive func(...any))) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		entries := newSyntheticEntries(syntheticCacheFileCount, syntheticFilesPerDirectory)
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

		retained := after.HeapAlloc - before.HeapAlloc
		b.ReportMetric(float64(retained), "retained-bytes")
		b.ReportMetric(float64(retained)/float64(syntheticCacheFileCount*copies), "retained-bytes/entry")
		runtime.KeepAlive(kept)
	}
}
