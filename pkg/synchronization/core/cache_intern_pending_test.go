// This file holds the contracts for cache interning: canonicalizing the
// per-directory shards of ScanCache and IgnoreCache against one daemon-wide
// weak table, the way Interner canonicalizes Entry subtrees.
//
// The motivation is measured: two sessions watching the same local directory run
// two local endpoints that each build an identical ScanCache and IgnoreCache
// over the same root, and under churn each endpoint pins up to two generations
// that share nothing, because every scan builds every inner map fresh. Together
// those caches are ~400-450MB of the daemon's ~780MB. Both duplications are the
// same duplication interning already collapsed for trees.
package core

import (
	"fmt"
	"runtime"
	"slices"
	"sync"
	"testing"
)

// TestCacheInternerCollapsesIdenticalScanCachesPending is RED: CacheInterner
// does not exist yet.
//
// The cross-session case. Two local endpoints scanning the same root produce
// caches that are equal shard for shard and share nothing, so interning both
// against one table must leave every per-directory map pointer-identical
// between them. Content equality is not enough to assert here: two equal maps
// are exactly what today's daemon has, and exactly what costs the second copy.
func TestCacheInternerCollapsesIdenticalScanCachesPending(t *testing.T) {
	entries := newSyntheticEntries(64, 8)
	first, _ := newShardedCacheAndIgnoreCache(entries)
	second, _ := newShardedCacheAndIgnoreCache(entries)

	interner := &CacheInterner{}
	internedFirst := interner.InternScanCache(first)
	internedSecond := interner.InternScanCache(second)

	checkShardSharing(t, "scan cache",
		scanCacheShardPointers(internedFirst),
		scanCacheShardPointers(internedSecond))

	// Interning may substitute shards but must not change what the caches hold.
	if !internedFirst.Equal(internedSecond) {
		t.Error("interned caches are not equal to each other")
	}
	if !internedFirst.Equal(second) {
		t.Error("interned cache is not equal to the un-interned source data")
	}
}

// TestCacheInternerSharesUnchangedScanDirectoriesPending is RED: CacheInterner
// does not exist yet.
//
// The generational case, which is the larger of the two wins under churn: a
// scan that observes a change in one directory rebuilds every shard, so the old
// and new generation of one endpoint's cache share nothing even though all but
// one directory is identical. After interning, only the changed directory may
// hold a fresh map.
func TestCacheInternerSharesUnchangedScanDirectoriesPending(t *testing.T) {
	entries := newSyntheticEntries(64, 8)
	changedDirectory := entries[0].directory

	previous, _ := newShardedCacheAndIgnoreCache(entries)
	next, _ := newShardedCacheAndIgnoreCache(withChangedDigests(entries, changedDirectory))

	interner := &CacheInterner{}
	internedPrevious := interner.InternScanCache(previous)
	internedNext := interner.InternScanCache(next)

	checkShardSharing(t, "scan cache generation",
		scanCacheShardPointers(internedPrevious),
		scanCacheShardPointers(internedNext),
		changedDirectory)

	if differing := differingScanCacheDirectories(internedPrevious, internedNext); len(differing) != 1 || differing[0] != changedDirectory {
		t.Errorf("interned generations differ in directories %q, want only %q", differing, changedDirectory)
	}
}

// TestCacheInternerCollapsesIdenticalIgnoreCachesPending is RED: CacheInterner
// does not exist yet. It is the IgnoreCache half of
// TestCacheInternerCollapsesIdenticalScanCachesPending.
func TestCacheInternerCollapsesIdenticalIgnoreCachesPending(t *testing.T) {
	entries := newSyntheticEntries(64, 8)
	first := newSyntheticIgnoreCache(entries, "")
	second := newSyntheticIgnoreCache(entries, "")

	interner := &CacheInterner{}
	internedFirst := interner.InternIgnoreCache(first)
	internedSecond := interner.InternIgnoreCache(second)

	checkShardSharing(t, "ignore cache",
		ignoreCacheShardPointers(internedFirst),
		ignoreCacheShardPointers(internedSecond))

	if !internedFirst.Equal(internedSecond) {
		t.Error("interned ignore caches are not equal to each other")
	}
	if !internedFirst.Equal(second) {
		t.Error("interned ignore cache is not equal to the un-interned source data")
	}
}

// TestCacheInternerSharesUnchangedIgnoreDirectoriesPending is RED:
// CacheInterner does not exist yet. It is the IgnoreCache half of
// TestCacheInternerSharesUnchangedScanDirectoriesPending.
func TestCacheInternerSharesUnchangedIgnoreDirectoriesPending(t *testing.T) {
	entries := newSyntheticEntries(64, 8)
	changedDirectory := entries[0].directory

	previous := newSyntheticIgnoreCache(entries, "")
	next := newSyntheticIgnoreCache(entries, changedDirectory)

	interner := &CacheInterner{}
	internedPrevious := interner.InternIgnoreCache(previous)
	internedNext := interner.InternIgnoreCache(next)

	checkShardSharing(t, "ignore cache generation",
		ignoreCacheShardPointers(internedPrevious),
		ignoreCacheShardPointers(internedNext),
		changedDirectory)

	if differing := differingIgnoreCacheDirectories(internedPrevious, internedNext); len(differing) != 1 || differing[0] != changedDirectory {
		t.Errorf("interned ignore generations differ in directories %q, want only %q", differing, changedDirectory)
	}
}

// TestCacheInterningPreservesLookupsPending is RED: CacheInterner does not
// exist yet.
//
// Interning is only a memory representation change, so every lookup must
// observe what it observed before — including lookups that miss, since a shard
// substituted from another cache could otherwise widen a cache's key set.
//
// The lookups are replayed against a *second*, independently built copy of the
// same data, whose shards interning is required to have replaced wholesale with
// the first copy's. Recording and replaying against one cache would hold
// trivially for an implementation that did nothing; requiring substitution
// first is what makes the equivalence claim mean anything.
//
// Asserted over a range of shapes rather than one corpus, since the shapes that
// stress substitution are the degenerate ones (one file per directory, one
// directory total, a root-level entry), not the average one.
func TestCacheInterningPreservesLookupsPending(t *testing.T) {
	// One interner across all shapes, so that later shapes intern against a
	// table already holding unrelated shards.
	interner := &CacheInterner{}

	for _, shape := range []struct {
		files             int
		filesPerDirectory int
	}{
		{files: 1, filesPerDirectory: 1},
		{files: 8, filesPerDirectory: 1},
		{files: 16, filesPerDirectory: 2},
		{files: 64, filesPerDirectory: 8},
		{files: 200, filesPerDirectory: 5},
	} {
		t.Run(fmt.Sprintf("files=%d/perDirectory=%d", shape.files, shape.filesPerDirectory), func(t *testing.T) {
			entries := newSyntheticEntries(shape.files, shape.filesPerDirectory)
			ignoredDirectory := entries[0].directory

			// Real caches carry root-level entries, which are the only source of
			// an empty-string outer key in a ScanCache; the generator produces
			// none, so every copy gets one.
			newCopy := func() (*ScanCache, IgnoreCache) {
				cache, _ := newShardedCacheAndIgnoreCache(entries)
				cache.set("", "README.md", &ScanCacheEntry{mode: 0100644, size: 12, digest: []byte("0123456789abcdef")})
				return cache, newSyntheticIgnoreCache(entries, ignoredDirectory)
			}
			cache, ignoreCache := newCopy()

			// Probe every present key plus keys that must stay absent: a
			// directory queried as a file, a file queried with the wrong
			// directory-ness, and paths in and out of populated directories.
			scanProbes := append(scanCachePaths(cache),
				"", ignoredDirectory, ignoredDirectory+"/absent.go",
				"absent", "absent/absent.go", ignoredDirectory+"/"+entries[0].name+"/nested.go",
			)
			ignoreProbes := append(ignoreCacheProbes(ignoreCache),
				ignoreCacheProbe{path: "", directory: false},
				ignoreCacheProbe{path: ignoredDirectory, directory: false},
				ignoreCacheProbe{path: ignoredDirectory + "/" + entries[0].name, directory: true},
				ignoreCacheProbe{path: "absent/absent.go", directory: false},
			)

			scanBefore := make([]*ScanCacheEntry, len(scanProbes))
			for i, path := range scanProbes {
				scanBefore[i] = cache.get(path)
			}
			type ignoreResult struct {
				ignored bool
				ok      bool
			}
			ignoreBefore := make([]ignoreResult, len(ignoreProbes))
			for i, probe := range ignoreProbes {
				ignored, ok := ignoreCache.get(probe.path, probe.directory)
				ignoreBefore[i] = ignoreResult{ignored, ok}
			}

			internedCache := interner.InternScanCache(cache)
			internedIgnoreCache := interner.InternIgnoreCache(ignoreCache)

			// The replay target: a second copy whose every shard interning must
			// have replaced with the first copy's.
			secondCache, secondIgnoreCache := newCopy()
			replayCache := interner.InternScanCache(secondCache)
			replayIgnoreCache := interner.InternIgnoreCache(secondIgnoreCache)
			checkShardSharing(t, "scan cache",
				scanCacheShardPointers(internedCache), scanCacheShardPointers(replayCache))
			checkShardSharing(t, "ignore cache",
				ignoreCacheShardPointers(internedIgnoreCache), ignoreCacheShardPointers(replayIgnoreCache))

			for i, path := range scanProbes {
				if got := replayCache.get(path); !sameScanCacheEntry(got, scanBefore[i]) {
					t.Errorf("lookup of %q changed across interning: %+v, want %+v", path, got, scanBefore[i])
				}
			}
			for i, probe := range ignoreProbes {
				ignored, ok := replayIgnoreCache.get(probe.path, probe.directory)
				if want := ignoreBefore[i]; ignored != want.ignored || ok != want.ok {
					t.Errorf("ignore lookup of %q (directory=%t) changed across interning: (%t, %t), want (%t, %t)",
						probe.path, probe.directory, ignored, ok, want.ignored, want.ok)
				}
			}
		})
	}
}

// TestCacheInterningDoesNotMutatePublishedCachePending is RED: CacheInterner
// does not exist yet.
//
// This is the contract that makes wiring safe at all, mirroring
// TestInternDoesNotMutatePublishedTree. A published cache is read by whoever
// holds it after the scan lock is released, while the next cycle interns the
// next generation. Interning that generation necessarily touches the shards it
// shares with the published cache, so the pass must write only into the new
// cache's own outer map, and only where a shard's identity actually changes.
//
// Run under -race for the concurrency half; the shard-pointer comparison
// catches mutation of the published cache even without it.
func TestCacheInterningDoesNotMutatePublishedCachePending(t *testing.T) {
	entries := newSyntheticEntries(200, 8)
	changedDirectory := entries[0].directory

	interner := &CacheInterner{}
	firstCache, _ := newShardedCacheAndIgnoreCache(entries)
	published := interner.InternScanCache(firstCache)
	publishedIgnore := interner.InternIgnoreCache(newSyntheticIgnoreCache(entries, ""))

	shardsBefore := scanCacheShardPointers(published)
	ignoreShardsBefore := ignoreCacheShardPointers(publishedIgnore)
	paths := scanCachePaths(published)
	probes := ignoreCacheProbes(publishedIgnore)

	// Read the published caches from another goroutine for the duration of the
	// second interning pass, the way a holder reads them across a cycle
	// boundary.
	stop := make(chan struct{})
	var reader sync.WaitGroup
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, path := range paths {
				if published.get(path) == nil {
					t.Errorf("published cache lost the entry for %q", path)
					return
				}
			}
			for _, probe := range probes {
				if _, ok := publishedIgnore.get(probe.path, probe.directory); !ok {
					t.Errorf("published ignore cache lost the entry for %q", probe.path)
					return
				}
			}
		}
	}()

	// Intern the next generation: identical but for one directory, and sharing
	// no maps with the published one, which is what the next scan produces.
	nextCache, _ := newShardedCacheAndIgnoreCache(withChangedDigests(entries, changedDirectory))
	next := interner.InternScanCache(nextCache)
	nextIgnore := interner.InternIgnoreCache(newSyntheticIgnoreCache(entries, changedDirectory))

	close(stop)
	reader.Wait()

	checkShardSharing(t, "scan cache generation",
		shardsBefore, scanCacheShardPointers(next), changedDirectory)
	checkShardSharing(t, "ignore cache generation",
		ignoreShardsBefore, ignoreCacheShardPointers(nextIgnore), changedDirectory)

	for directory, pointer := range scanCacheShardPointers(published) {
		if shardsBefore[directory] != pointer {
			t.Errorf("interning rewrote the published cache's shard for %q", directory)
		}
	}
	for directory, pointer := range ignoreCacheShardPointers(publishedIgnore) {
		if ignoreShardsBefore[directory] != pointer {
			t.Errorf("interning rewrote the published ignore cache's shard for %q", directory)
		}
	}
}

// TestCacheInternerEvictsCollectedShardsPending is RED: CacheInterner does not
// exist yet.
//
// A daemon-wide table that pinned the shards it canonicalized would convert a
// memory optimization into a leak, since every generation of every session's
// cache passes through it. Dropping the last reference to a shard must therefore
// drop the table's record of it on the next sweep, exactly as
// TestInternEvictsCollectedSubtrees requires of Interner.
//
// Eviction is asserted alongside its opposite: a sweep must not discard records
// of shards that are still alive. Checking only that the table shrinks would be
// satisfied by a table that simply cleared itself, which would silently stop
// collapsing a second session's cache onto a first session's live one — the
// whole point of the table.
//
// This is the one contract that needs a window into the table's mechanism, and
// it uses the same unexported size() accessor Interner exposes for the purpose,
// because weakly-held state has no other externally visible evidence. It should
// not be read as pinning weak pointers specifically: what must hold is dropped
// shards leaving and live shards staying.
func TestCacheInternerEvictsCollectedShardsPending(t *testing.T) {
	interner := &CacheInterner{}

	retainedEntries := newSyntheticEntries(400, 8)
	retainedSource, _ := newShardedCacheAndIgnoreCache(retainedEntries)
	retained := interner.InternScanCache(retainedSource)
	retainedIgnore := interner.InternIgnoreCache(newSyntheticIgnoreCache(retainedEntries, ""))
	if size := interner.size(); size == 0 {
		t.Fatal("cache intern table is empty after interning a cache")
	}

	// Intern a cache sharing no shard with the retained one, then drop it. Its
	// construction is scoped to the closure so that no local slot can keep it
	// reachable past the collection below.
	func() {
		entries := withChangedDigests(retainedEntries, distinctDirectories(retainedEntries)...)
		cache, _ := newShardedCacheAndIgnoreCache(entries)
		dropped := interner.InternScanCache(cache)
		droppedIgnore := interner.InternIgnoreCache(newSyntheticIgnoreCache(entries, entries[0].directory))
		runtime.KeepAlive(dropped)
		runtime.KeepAlive(droppedIgnore)
	}()
	sizeWithBoth := interner.size()

	runtime.GC()
	interner.Sweep()

	if size := interner.size(); size >= sizeWithBoth {
		t.Errorf("cache intern table holds %d buckets after a sweep, down from none of %d: the dropped cache's shards were not evicted",
			size, sizeWithBoth)
	}

	// The retained caches are still alive, so a freshly built identical copy must
	// still collapse onto them.
	freshSource, _ := newShardedCacheAndIgnoreCache(retainedEntries)
	fresh := interner.InternScanCache(freshSource)
	freshIgnore := interner.InternIgnoreCache(newSyntheticIgnoreCache(retainedEntries, ""))
	checkShardSharing(t, "retained scan cache",
		scanCacheShardPointers(retained), scanCacheShardPointers(fresh))
	checkShardSharing(t, "retained ignore cache",
		ignoreCacheShardPointers(retainedIgnore), ignoreCacheShardPointers(freshIgnore))

	runtime.KeepAlive(retained)
	runtime.KeepAlive(retainedIgnore)
}

// TestCacheInternerHandlesEmptyCachesPending is RED: CacheInterner does not
// exist yet.
//
// ScanCache documents a nil receiver as a valid empty cache and
// NewScanCacheFromProto returns nil for an empty wire cache, so the hook sites
// will hand nil to the interner on any session whose cache has not been
// populated yet. A nil IgnoreCache is likewise a valid empty cache. Neither may
// panic, and neither may acquire content.
func TestCacheInternerHandlesEmptyCachesPending(t *testing.T) {
	interner := &CacheInterner{}

	if interned := interner.InternScanCache(nil); interned.len() != 0 {
		t.Errorf("interning a nil scan cache produced %d entries", interned.len())
	}
	if interned := interner.InternIgnoreCache(nil); interned.Len() != 0 {
		t.Errorf("interning a nil ignore cache produced %d entries", interned.Len())
	}
	if interned := interner.InternScanCache(newScanCache(0)); interned.len() != 0 {
		t.Errorf("interning an empty scan cache produced %d entries", interned.len())
	}
	if interned := interner.InternIgnoreCache(make(IgnoreCache)); interned.Len() != 0 {
		t.Errorf("interning an empty ignore cache produced %d entries", interned.Len())
	}
}

// TestSharedCacheInternerIsProcessWidePending is RED: SharedCacheInterner does
// not exist yet.
//
// Collapsing two sessions' caches requires that every hook site reach the same
// table, so two independent calls to the shared accessor must intern into one
// table, not two. Asserted through sharing rather than by comparing the returned
// pointers, since pointer equality alone would also hold for an accessor that
// returned a table nothing was ever inserted into.
func TestSharedCacheInternerIsProcessWidePending(t *testing.T) {
	entries := newSyntheticEntries(64, 8)
	first, _ := newShardedCacheAndIgnoreCache(entries)
	second, _ := newShardedCacheAndIgnoreCache(entries)

	internedFirst := SharedCacheInterner().InternScanCache(first)
	internedSecond := SharedCacheInterner().InternScanCache(second)

	checkShardSharing(t, "shared table",
		scanCacheShardPointers(internedFirst),
		scanCacheShardPointers(internedSecond))
}

// checkShardSharing verifies that two shard-pointer maps cover the same
// directories and that every directory is pointer-shared between them, except
// those named in unshared, which must not be.
func checkShardSharing(t *testing.T, kind string, first, second map[string]uintptr, unshared ...string) {
	t.Helper()

	if len(first) != len(second) {
		t.Fatalf("%s: interned caches hold %d and %d shards", kind, len(first), len(second))
	}
	if len(first) == 0 {
		t.Fatalf("%s: interned caches hold no shards", kind)
	}

	expectedUnshared := make(map[string]bool, len(unshared))
	for _, directory := range unshared {
		expectedUnshared[directory] = true
	}

	// A wholly un-interned cache fails on every directory at once, so the
	// offenders are counted and only the first few named: the count is the
	// diagnostic, and hundreds of identical lines are not.
	var unsharedButEqual, sharedButDiffering, missing []string
	for directory, pointer := range first {
		otherPointer, ok := second[directory]
		switch {
		case !ok:
			missing = append(missing, directory)
		case expectedUnshared[directory] && otherPointer == pointer:
			sharedButDiffering = append(sharedButDiffering, directory)
		case !expectedUnshared[directory] && otherPointer != pointer:
			unsharedButEqual = append(unsharedButEqual, directory)
		}
	}

	if len(missing) > 0 {
		t.Errorf("%s: %d of %d directories are present in one cache and absent from the other, including %q",
			kind, len(missing), len(first), sampleDirectories(missing))
	}
	if len(unsharedButEqual) > 0 {
		t.Errorf("%s: %d of %d directories hold equal content in two distinct shards, including %q",
			kind, len(unsharedButEqual), len(first), sampleDirectories(unsharedButEqual))
	}
	if len(sharedButDiffering) > 0 {
		t.Errorf("%s: %d directories have differing content but a shared shard: %q",
			kind, len(sharedButDiffering), sampleDirectories(sharedButDiffering))
	}
}

// sampleDirectories returns a deterministic, bounded sample of a directory list
// for inclusion in a failure message.
func sampleDirectories(directories []string) []string {
	slices.Sort(directories)
	if len(directories) > 3 {
		return directories[:3]
	}
	return directories
}
