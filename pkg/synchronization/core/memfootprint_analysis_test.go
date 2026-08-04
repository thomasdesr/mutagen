package core

// Throwaway analysis benchmarks for the daemon memory investigation.
// Not intended for upstreaming; they quantify per-entry memory costs and
// the cost of a hypothetical dedupe pass on realistic tree shapes.

import (
	"fmt"
	"math/rand"
	"runtime"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// buildSyntheticTree builds a tree shaped like the monorepo session:
// ~files files in dirs of ~fanout files each, 16-byte digests,
// realistic name lengths.
func buildSyntheticTree(rng *rand.Rand, files, fanout int) *Entry {
	root := &Entry{Kind: EntryKind_Directory, Contents: map[string]*Entry{}}
	made := 0
	var mkdir func(parent *Entry, depth int)
	name := func() string {
		return fmt.Sprintf("entry_%08x_%04x", rng.Uint32(), rng.Uint32()&0xffff)
	}
	mkdir = func(parent *Entry, depth int) {
		for made < files {
			// A directory gets fanout files, then descends into subdirs.
			for i := 0; i < fanout && made < files; i++ {
				d := make([]byte, 16)
				rng.Read(d)
				parent.Contents[name()] = &Entry{Kind: EntryKind_File, Digest: d}
				made++
			}
			if made >= files {
				return
			}
			nsub := 3
			for i := 0; i < nsub && made < files; i++ {
				sub := &Entry{Kind: EntryKind_Directory, Contents: map[string]*Entry{}}
				parent.Contents[name()] = sub
				mkdir(sub, depth+1)
			}
			return
		}
	}
	mkdir(root, 0)
	return root
}

func heapInuse() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

// TestEntryTreeFootprint measures live-heap bytes per entry for a
// 703k-entry tree (the big session shape).
func TestEntryTreeFootprint(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	before := heapInuse()
	tree := buildSyntheticTree(rng, 624000, 8)
	after := heapInuse()
	n := tree.Count()
	t.Logf("entries=%d heap=%d MB bytes/entry=%.1f",
		n, (after-before)>>20, float64(after-before)/float64(n))
	runtime.KeepAlive(tree)
}

// dedupe returns a version of novel that shares subtrees with base wherever
// they are deeply equal. Pointer-equal subtrees short-circuit.
func dedupe(base, novel *Entry) *Entry {
	if novel == base {
		return novel
	}
	if base == nil || novel == nil {
		return novel
	}
	if novel.Kind == EntryKind_Directory && base.Kind == EntryKind_Directory {
		identical := len(novel.Contents) == len(base.Contents) &&
			novel.Digest == nil && base.Digest == nil
		shared := make(map[string]*Entry, len(novel.Contents))
		for name, child := range novel.Contents {
			merged := dedupe(base.Contents[name], child)
			shared[name] = merged
			if merged != base.Contents[name] {
				identical = false
			}
		}
		if identical {
			return base
		}
		novel.Contents = shared
		return novel
	}
	if novel.Equal(base, false) {
		return base
	}
	return novel
}

// TestDedupeFootprintAndCost measures the memory reclaimed and CPU cost of
// deduping a deep copy (worst case: zero pointer sharing, full equality)
// against its base — the "ancestor vs snapshot at steady state" scenario.
func TestDedupeFootprintAndCost(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	base := buildSyntheticTree(rng, 624000, 8)
	baseline := heapInuse()
	clone := base.Copy(true)
	afterClone := heapInuse()
	start := time.Now()
	clone = dedupe(base, clone)
	elapsed := time.Since(start)
	afterDedupe := heapInuse()
	if clone != base {
		t.Fatalf("dedupe failed to collapse identical tree")
	}
	t.Logf("clone cost=%d MB, dedupe pass=%v, reclaimed=%d MB",
		(afterClone-baseline)>>20, elapsed, (afterClone-afterDedupe)>>20)
	runtime.KeepAlive(base)
}

// TestCacheFootprint measures the proto Cache footprint for the big session
// (624k files, realistic path lengths) vs a compact value-type layout.
func TestCacheFootprint(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	paths := make([]string, 624000)
	for i := range paths {
		paths[i] = fmt.Sprintf(
			"backend/server/services/domain_%04x/subsystem_%04x/module_%04x/file_%08x.py",
			rng.Uint32()&0xffff, rng.Uint32()&0xffff, rng.Uint32()&0xffff, rng.Uint32())
	}

	before := heapInuse()
	proto := &Cache{Entries: make(map[string]*CacheEntry, len(paths))}
	for _, p := range paths {
		d := make([]byte, 16)
		rng.Read(d)
		proto.Entries[p] = &CacheEntry{
			Mode:             0644,
			ModificationTime: timestamppb.New(time.Now()),
			Size:             4096,
			FileID:           rng.Uint64(),
			Digest:           d,
		}
	}
	afterProto := heapInuse()

	type compactEntry struct {
		Mode      uint32
		MTimeSec  int64
		MTimeNsec int32
		Size      uint64
		FileID    uint64
		Digest    [16]byte
	}
	compact := make(map[string]compactEntry, len(paths))
	for _, p := range paths {
		var d [16]byte
		rng.Read(d[:])
		compact[p] = compactEntry{Mode: 0644, MTimeSec: 1, Size: 4096, FileID: 1, Digest: d}
	}
	afterCompact := heapInuse()

	t.Logf("proto cache=%d MB (%.0f B/file), compact=%d MB (%.0f B/file)",
		(afterProto-before)>>20, float64(afterProto-before)/float64(len(paths)),
		(afterCompact-afterProto)>>20, float64(afterCompact-afterProto)/float64(len(paths)))
	runtime.KeepAlive(proto)
	runtime.KeepAlive(compact)
}
