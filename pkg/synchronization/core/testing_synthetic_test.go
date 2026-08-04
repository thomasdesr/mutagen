package core

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"testing"

	"google.golang.org/protobuf/proto"
)

// syntheticTreeSpec describes the shape of a synthetic entry hierarchy built by
// buildSyntheticTree. It is modeled on a large monorepo checkout: a wide, deep
// directory structure in which a small pool of basenames (BUILD.bazel,
// __init__.py, ...) recurs in most directories, and in which a fraction of files
// share content (and therefore digests) with other files.
type syntheticTreeSpec struct {
	// files is the approximate number of file entries to generate.
	files int
	// directoryFanout is the number of subdirectories created per directory
	// until the file budget is exhausted.
	directoryFanout int
	// filesPerDirectory is the number of file entries created per directory.
	filesPerDirectory int
	// duplicateContentFraction is the fraction of file entries (in [0,1]) whose
	// digests are drawn from a small shared pool rather than being unique. It
	// models identical files (empty __init__.py, license headers, generated
	// stubs) that recur throughout a real checkout.
	duplicateContentFraction float64
}

// defaultSyntheticTreeSpec approximates one of the monorepo sessions from the
// live daemon baseline: ~630k files is the real number, but 150k keeps the
// benchmark runnable while preserving the shape and the per-node cost profile.
var defaultSyntheticTreeSpec = syntheticTreeSpec{
	files:                    150000,
	directoryFanout:          6,
	filesPerDirectory:        12,
	duplicateContentFraction: 0.25,
}

// buildSyntheticTree constructs a synthetic entry hierarchy matching the
// specified shape. The result is deterministic for a given spec.
func buildSyntheticTree(spec syntheticTreeSpec) *Entry {
	b := &syntheticTreeBuilder{spec: spec, remaining: spec.files}
	return b.directory(0)
}

// syntheticTreeBuilder holds the state of an in-progress synthetic tree build.
type syntheticTreeBuilder struct {
	// spec is the shape being built.
	spec syntheticTreeSpec
	// remaining is the number of file entries left in the budget.
	remaining int
	// counter is a monotonic sequence used to derive unique names and digests.
	counter uint64
}

// directory builds one directory entry (and, recursively, its subtree) at the
// specified depth, consuming from the file budget.
func (b *syntheticTreeBuilder) directory(depth int) *Entry {
	contents := make(map[string]*Entry)

	// Add file entries, drawing the first names from the recurring pool so that
	// the same basename strings appear in most directories.
	for i := 0; i < b.spec.filesPerDirectory && b.remaining > 0; i++ {
		b.remaining--
		b.counter++
		var name string
		if i < len(recurringBasenames) {
			name = recurringBasenames[i]
		} else {
			name = fmt.Sprintf("file_%d.go", b.counter)
		}
		contents[name] = &Entry{
			Kind:       EntryKind_File,
			Digest:     b.digest(),
			Executable: b.counter%16 == 0,
		}
	}

	// Recurse into subdirectories while budget remains. Depth is bounded so that
	// wide trees don't degenerate into a single deep chain.
	if depth < 12 {
		for i := 0; i < b.spec.directoryFanout && b.remaining > 0; i++ {
			b.counter++
			var name string
			if i < len(recurringDirnames) {
				name = recurringDirnames[i]
			} else {
				name = fmt.Sprintf("pkg_%d", b.counter)
			}
			contents[name] = b.directory(depth + 1)
		}
	}

	return &Entry{Kind: EntryKind_Directory, Contents: contents}
}

// digest returns the next digest in the build sequence. Digests are 16 bytes to
// match the xxh128 digests that Mutagen actually stores. A configured fraction
// of them are drawn from a small pool to model duplicate file content.
func (b *syntheticTreeBuilder) digest() []byte {
	// Decide whether this digest is drawn from the duplicate pool. Using the
	// counter modulo a scale keeps the split deterministic and evenly spread.
	const scale = 1000
	threshold := uint64(b.spec.duplicateContentFraction * scale)

	seed := b.counter
	if b.counter%scale < threshold {
		seed = b.counter % duplicateContentPoolSize
	}

	digest := make([]byte, 16)
	binary.LittleEndian.PutUint64(digest, seed*0x9e3779b97f4a7c15)
	binary.LittleEndian.PutUint64(digest[8:], seed*0xc2b2ae3d27d4eb4f)
	return digest
}

// duplicateContentPoolSize is the number of distinct digests in the duplicate
// content pool. It is small relative to the file count, matching the handful of
// boilerplate file bodies that recur throughout a real checkout.
const duplicateContentPoolSize = 64

// recurringBasenames are file basenames that appear in most directories of a
// real polyglot monorepo. They exist to reproduce the repeated-name string
// retention seen in the live heap profile.
var recurringBasenames = []string{
	"BUILD.bazel",
	"__init__.py",
	"README.md",
	"main.go",
	"doc.go",
	"conftest.py",
	"index.ts",
	"types.ts",
}

// recurringDirnames are directory basenames that appear at many levels of a real
// monorepo checkout.
var recurringDirnames = []string{
	"internal",
	"pkg",
	"cmd",
	"testdata",
	"api",
}

// decodeCopies serializes the specified tree once and then decodes it the
// specified number of times, returning the independently decoded trees. This
// reproduces what the daemon does: the ancestor is decoded from the session
// archive and the beta snapshot is decoded from the wire, each producing a fully
// distinct object graph even when the content is identical.
func decodeCopies(t testing.TB, tree *Entry, count int) []*Entry {
	t.Helper()

	encoded, err := proto.Marshal(&Snapshot{Content: tree})
	if err != nil {
		t.Fatalf("unable to marshal snapshot: %v", err)
	}

	copies := make([]*Entry, count)
	for i := range copies {
		snapshot := &Snapshot{}
		if err := proto.Unmarshal(encoded, snapshot); err != nil {
			t.Fatalf("unable to unmarshal snapshot: %v", err)
		}
		copies[i] = snapshot.Content
	}
	return copies
}

// heapProfile records live heap statistics.
type heapProfile struct {
	// bytes is the number of live heap bytes.
	bytes uint64
	// objects is the number of live heap objects. GC scan cost tracks this more
	// closely than it tracks byte count.
	objects uint64
}

// retentionHold anchors the value under measurement. A package-level variable is
// used rather than a local because local liveness is only precise to the
// instruction, so a local can remain reachable from a stale stack slot after the
// last use and silently inflate the following measurement. Assigning nil here is
// unambiguous.
var retentionHold any

// measureRetained returns the live heap growth attributable to the value
// returned by construct, which is dropped again before returning. It forces a
// full GC before sampling both endpoints so that the difference reflects
// retained rather than allocated memory.
func measureRetained(construct func() any) heapProfile {
	// Ensure nothing from a previous measurement is still anchored.
	retentionHold = nil

	before := readLiveHeap()
	retentionHold = construct()
	after := readLiveHeap()
	retentionHold = nil

	return heapProfile{
		bytes:   after.bytes - before.bytes,
		objects: after.objects - before.objects,
	}
}

// readLiveHeap forces a garbage collection and returns the resulting live heap
// statistics. Two collections are performed because the first may leave objects
// finalized but not yet freed.
func readLiveHeap() heapProfile {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return heapProfile{bytes: stats.HeapAlloc, objects: stats.HeapObjects}
}
