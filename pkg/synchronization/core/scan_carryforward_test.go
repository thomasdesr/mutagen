// This file holds the contracts for cache-shard carry-forward in Scan: a
// rescan whose files were wholly reusable from the prior cache must adopt the
// prior cache's shard objects rather than rebuilding equal shards, so that
// the interner's O(1) already-canonical short-circuit applies and the
// per-scan re-hash of unchanged directories (measured at ~5.6% of rescan
// storm CPU, essentially all waste) disappears.
package core

import (
	"context"
	"crypto/sha1"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem/behavior"
)

// scanForCarryForwardTest runs a full (non-accelerated) warm scan of root
// with the provided prior caches, mirroring what a flush-driven rescan does.
func scanForCarryForwardTest(t *testing.T, root string, cache *ScanCache, ignoreCache IgnoreCache) (*Snapshot, *ScanCache, IgnoreCache) {
	t.Helper()
	snapshot, newCache, newIgnoreCache, err := Scan(
		context.Background(),
		root,
		nil, nil,
		sha1.New(), cache,
		[]string{"ignored-name"}, ignoreCache,
		behavior.ProbeMode_ProbeModeProbe,
		SymbolicLinkMode_SymbolicLinkModePortable,
		PermissionsMode_PermissionsModePortable,
	)
	if err != nil {
		t.Fatal("scan failed:", err)
	}
	return snapshot, newCache, newIgnoreCache
}

// createCarryForwardFixture creates a small tree with multiple directories
// holding regular files.
func createCarryForwardFixture(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	for _, directory := range []string{"a", "b", "b/c"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0700); err != nil {
			t.Fatal("unable to create fixture directory:", err)
		}
	}
	for _, file := range []string{"top", "a/one", "a/two", "b/one", "b/c/deep"} {
		if err := os.WriteFile(filepath.Join(root, file), []byte("content of "+file), 0600); err != nil {
			t.Fatal("unable to create fixture file:", err)
		}
	}
	return root
}

// TestScanCarriesForwardUnchangedCacheShards pins the core contract: a
// second full scan of an unchanged tree returns caches whose per-directory
// shards are the SAME OBJECTS as the first scan's, for both the digest cache
// and the ignore cache.
func TestScanCarriesForwardUnchangedCacheShards(t *testing.T) {
	root := createCarryForwardFixture(t)

	_, cache1, ignoreCache1 := scanForCarryForwardTest(t, root, nil, nil)
	_, cache2, ignoreCache2 := scanForCarryForwardTest(t, root, cache1, ignoreCache1)

	if len(cache2.directories) != len(cache1.directories) {
		t.Fatalf("directory count changed across identical scans: %d != %d",
			len(cache2.directories), len(cache1.directories))
	}
	for directory, shard := range cache2.directories {
		if cache1.directories[directory] != shard {
			t.Errorf("scan cache shard for %q was rebuilt rather than carried forward", directory)
		}
	}
	for directory, shard := range ignoreCache2 {
		if ignoreCache1[directory] != shard {
			t.Errorf("ignore cache shard for %q was rebuilt rather than carried forward", directory)
		}
	}
}

// TestScanCarryForwardIsolatesChangedDirectories pins the boundary: a change
// within one directory must yield a fresh shard for that directory while
// every other directory's shard is still carried forward.
func TestScanCarryForwardIsolatesChangedDirectories(t *testing.T) {
	root := createCarryForwardFixture(t)

	_, cache1, ignoreCache1 := scanForCarryForwardTest(t, root, nil, nil)

	// Modify a file in "a" with content of a different size and a
	// deliberately distinct modification time, so the change is visible to
	// the cache regardless of filesystem timestamp granularity.
	changed := filepath.Join(root, "a/one")
	if err := os.WriteFile(changed, []byte("changed content, different size"), 0600); err != nil {
		t.Fatal("unable to modify fixture file:", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(changed, future, future); err != nil {
		t.Fatal("unable to adjust modification time:", err)
	}

	_, cache2, _ := scanForCarryForwardTest(t, root, cache1, ignoreCache1)

	if cache2.directories["a"] == cache1.directories["a"] {
		t.Error("changed directory's shard was carried forward despite modification")
	}
	for _, directory := range []string{"", "b", "b/c"} {
		if cache2.directories[directory] != cache1.directories[directory] {
			t.Errorf("unchanged directory %q was rebuilt rather than carried forward", directory)
		}
	}
}
