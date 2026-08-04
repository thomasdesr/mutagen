// This file holds the untagged harness for the cache interning contract tests.
// It is deliberately not build-tagged: the contract tests in
// cache_intern_pending_test.go assert pointer identity of per-directory cache
// shards, and such an assertion is only meaningful if the caches it compares
// genuinely start out sharing nothing and differing only where intended. The
// tests below check exactly that, so the harness is exercised on every run
// rather than only under -tags wiring_pending.
package core

import (
	"bytes"
	"maps"
	"reflect"
	"slices"
	"testing"
)

// TestSyntheticScanCacheHarness guards the assumptions the ScanCache interning
// contracts rest on: building the same synthetic scan data twice yields caches
// with equal content and no shared per-directory map, and withChangedDigests
// perturbs exactly one directory. Without this, a shard identity assertion
// could pass or fail for reasons that have nothing to do with interning.
func TestSyntheticScanCacheHarness(t *testing.T) {
	entries := newSyntheticEntries(64, 8)

	first, _ := newShardedCacheAndIgnoreCache(entries)
	second, _ := newShardedCacheAndIgnoreCache(entries)

	if !first.Equal(second) {
		t.Error("independently built caches from identical data are not equal")
	}

	firstShards, secondShards := scanCacheShardPointers(first), scanCacheShardPointers(second)
	if len(firstShards) < 2 {
		t.Fatalf("synthetic cache has %d directory shards, need at least 2 to distinguish shared from unshared", len(firstShards))
	}
	if len(firstShards) != len(secondShards) {
		t.Fatalf("independently built caches have %d and %d shards", len(firstShards), len(secondShards))
	}
	for directory, pointer := range firstShards {
		if secondShards[directory] == pointer {
			t.Errorf("independently built caches already share the shard for %q", directory)
		}
	}

	// withChangedDigests must produce a cache that differs from the original in
	// exactly the directory named, since that is the whole basis of the
	// generational sharing contract.
	changedDirectory := entries[0].directory
	changed, _ := newShardedCacheAndIgnoreCache(withChangedDigests(entries, changedDirectory))
	if first.Equal(changed) {
		t.Fatal("withChangedDigests produced a cache equal to the original")
	}
	differing := differingScanCacheDirectories(first, changed)
	if len(differing) != 1 || differing[0] != changedDirectory {
		t.Errorf("withChangedDigests changed directories %q, want only %q", differing, changedDirectory)
	}

	// Naming every directory must change every one of them, which is what makes
	// a cache built from the result share no shard with the original.
	allDirectories := distinctDirectories(entries)
	wholly, _ := newShardedCacheAndIgnoreCache(withChangedDigests(entries, allDirectories...))
	if differing := differingScanCacheDirectories(first, wholly); !slices.Equal(differing, allDirectories) {
		t.Errorf("changing every directory's digests changed %q, want %q", differing, allDirectories)
	}
}

// TestSyntheticIgnoreCacheHarness is the IgnoreCache counterpart of
// TestSyntheticScanCacheHarness.
func TestSyntheticIgnoreCacheHarness(t *testing.T) {
	entries := newSyntheticEntries(64, 8)

	first := newSyntheticIgnoreCache(entries, "")
	second := newSyntheticIgnoreCache(entries, "")

	if !first.Equal(second) {
		t.Error("independently built ignore caches from identical data are not equal")
	}

	firstShards, secondShards := ignoreCacheShardPointers(first), ignoreCacheShardPointers(second)
	if len(firstShards) < 2 {
		t.Fatalf("synthetic ignore cache has %d directory shards, need at least 2", len(firstShards))
	}
	if len(firstShards) != len(secondShards) {
		t.Fatalf("independently built ignore caches have %d and %d shards", len(firstShards), len(secondShards))
	}
	for directory, pointer := range firstShards {
		if secondShards[directory] == pointer {
			t.Errorf("independently built ignore caches already share the shard for %q", directory)
		}
	}

	// Flipping one directory's ignore verdicts must land in exactly one shard.
	ignoredDirectory := entries[0].directory
	flipped := newSyntheticIgnoreCache(entries, ignoredDirectory)
	if first.Equal(flipped) {
		t.Fatal("flipping a directory's ignore verdicts produced an equal cache")
	}
	differing := differingIgnoreCacheDirectories(first, flipped)
	if len(differing) != 1 || differing[0] != ignoredDirectory {
		t.Errorf("flipping %q changed directories %q, want only that one", ignoredDirectory, differing)
	}
}

// newSyntheticIgnoreCache builds a directory-sharded IgnoreCache covering every
// entry in entries: one verdict per file path and one per containing directory,
// plus a root verdict (the real caches always carry one, and it is the only
// source of an empty-string outer key). Files directly within ignoredDirectory
// are recorded as ignored; passing "" ignores nothing.
//
// It exists rather than reusing newShardedCacheAndIgnoreCache because that
// helper records every verdict as false, which leaves no way to build two
// ignore caches that differ in exactly one directory.
func newSyntheticIgnoreCache(entries []syntheticEntry, ignoredDirectory string) IgnoreCache {
	cache := make(IgnoreCache, countDistinctDirectories(entries))
	cache.set("", true, false)
	for _, entry := range entries {
		cache.set(entry.directory+"/"+entry.name, false, entry.directory == ignoredDirectory)
		cache.set(entry.directory, true, false)
	}
	return cache
}

// withChangedDigests returns a copy of entries in which the digest of every
// file directly within one of the named directories has been perturbed. Naming
// a single directory models the generational case, where one scan differs from
// the previous one in one directory and nowhere else; naming all of them
// produces data that shares no shard with the original.
func withChangedDigests(entries []syntheticEntry, directories ...string) []syntheticEntry {
	changing := make(map[string]bool, len(directories))
	for _, directory := range directories {
		changing[directory] = true
	}

	changed := make([]syntheticEntry, len(entries))
	copy(changed, entries)
	for i := range changed {
		if !changing[changed[i].directory] {
			continue
		}
		// Copy rather than mutate: the digest slice is shared with the original
		// entries, which the caller is still using as the unchanged generation.
		digest := make([]byte, len(changed[i].digest))
		copy(digest, changed[i].digest)
		digest[len(digest)-1] ^= 0xff
		changed[i].digest = digest
	}
	return changed
}

// distinctDirectories returns every directory appearing in entries, sorted.
func distinctDirectories(entries []syntheticEntry) []string {
	directories := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		directories[entry.directory] = struct{}{}
	}
	return slices.Sorted(maps.Keys(directories))
}

// scanCacheShardPointers returns the identity of every per-directory map in the
// cache, keyed by directory. Comparing two of these is what distinguishes a
// shared shard from an equal-but-distinct one, which content comparison cannot.
func scanCacheShardPointers(c *ScanCache) map[string]uintptr {
	pointers := make(map[string]uintptr, len(c.directories))
	for directory, names := range c.directories {
		pointers[directory] = reflect.ValueOf(names).Pointer()
	}
	return pointers
}

// ignoreCacheShardPointers is the IgnoreCache counterpart of
// scanCacheShardPointers.
func ignoreCacheShardPointers(c IgnoreCache) map[string]uintptr {
	pointers := make(map[string]uintptr, len(c))
	for directory, names := range c {
		pointers[directory] = reflect.ValueOf(names).Pointer()
	}
	return pointers
}

// differingScanCacheDirectories returns the directories in which two caches
// disagree about any lookup, including directories present in only one of them.
// It compares through get rather than by walking shards so that it stays
// correct if shards acquire a wrapper type.
func differingScanCacheDirectories(a, b *ScanCache) []string {
	differing := make(map[string]struct{})
	for _, path := range append(scanCachePaths(a), scanCachePaths(b)...) {
		if !sameScanCacheEntry(a.get(path), b.get(path)) {
			directory, _ := splitCachePath(path)
			differing[directory] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(differing))
}

// differingIgnoreCacheDirectories is the IgnoreCache counterpart of
// differingScanCacheDirectories.
func differingIgnoreCacheDirectories(a, b IgnoreCache) []string {
	differing := make(map[string]struct{})
	for _, probe := range append(ignoreCacheProbes(a), ignoreCacheProbes(b)...) {
		aIgnored, aOK := a.get(probe.path, probe.directory)
		bIgnored, bOK := b.get(probe.path, probe.directory)
		if aIgnored != bIgnored || aOK != bOK {
			directory, _ := splitCachePath(probe.path)
			differing[directory] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(differing))
}

// sameScanCacheEntry reports whether two cache entries carry the same metadata,
// treating nil as equal only to nil. It compares content rather than identity
// because interning is allowed to substitute an equal entry from another cache;
// what it may not do is change what a lookup observes.
func sameScanCacheEntry(a, b *ScanCacheEntry) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.mode == b.mode &&
		a.modificationTimeSeconds == b.modificationTimeSeconds &&
		a.modificationTimeNanos == b.modificationTimeNanos &&
		a.size == b.size &&
		a.fileID == b.fileID &&
		bytes.Equal(a.digest, b.digest)
}

// scanCachePaths returns the full path of every entry in the cache,
// reconstructed the way Proto does, so that lookups can be replayed through
// the public get path.
//
// This and ignoreCacheProbes are the only two places in the cache interning
// tests that read a per-directory shard's contents directly, so they are the
// only two that need touching if shards acquire a wrapper type.
func scanCachePaths(c *ScanCache) []string {
	var paths []string
	for directory, shard := range c.directories {
		for name := range shard.entries {
			if directory == "" {
				paths = append(paths, name)
			} else {
				paths = append(paths, directory+"/"+name)
			}
		}
	}
	return paths
}

// ignoreCacheProbe is a single IgnoreCache lookup: a full path plus the
// directory-ness that forms the other half of the key.
type ignoreCacheProbe struct {
	path      string
	directory bool
}

// ignoreCacheProbes returns a probe for every entry in the cache.
func ignoreCacheProbes(c IgnoreCache) []ignoreCacheProbe {
	var probes []ignoreCacheProbe
	for directory, shard := range c {
		for key := range shard.entries {
			path := key.name
			if directory != "" {
				path = directory + "/" + key.name
			}
			probes = append(probes, ignoreCacheProbe{path: path, directory: key.directory})
		}
	}
	return probes
}
