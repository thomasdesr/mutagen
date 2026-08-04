package core

import (
	"fmt"
	"math/rand"
	"runtime"
	"testing"
)

// TestInternCollapsesExactlyEqualSubtrees verifies the safety property that the
// whole optimization rests on: two trees interned against the same table share a
// root pointer if and only if they are deep-equal. A false positive (sharing
// unequal trees) would silently corrupt synchronization state, so this is
// checked over many randomly generated small trees, whose shallow depth and tiny
// name and digest alphabets make collisions and near-misses likely.
func TestInternCollapsesExactlyEqualSubtrees(t *testing.T) {
	random := rand.New(rand.NewSource(1))

	const treeCount = 300
	originals := make([]*Entry, treeCount)
	interned := make([]*Entry, treeCount)
	var interner Interner
	for i := range originals {
		tree := randomTree(random, 0)
		// Intern a copy so that the original remains available for comparison:
		// Intern rewrites content maps in place.
		originals[i] = tree
		interned[i] = interner.Intern(tree.Copy(true))
	}

	for i := range interned {
		for j := range interned {
			shared := interned[i] == interned[j]
			equal := originals[i].Equal(originals[j], true)
			if shared != equal {
				t.Fatalf("tree %d and %d: shared=%v but deep-equal=%v", i, j, shared, equal)
			}
		}
	}
}

// TestInternPreservesContent verifies that interning is content-preserving: the
// returned tree is deep-equal to the input and remains valid.
func TestInternPreservesContent(t *testing.T) {
	random := rand.New(rand.NewSource(2))
	var interner Interner
	for i := 0; i < 200; i++ {
		original := randomTree(random, 0)
		result := interner.Intern(original.Copy(true))
		if !result.Equal(original, true) {
			t.Fatalf("tree %d: interned tree is not deep-equal to the original", i)
		}
		if err := result.EnsureValid(false); err != nil {
			t.Fatalf("tree %d: interned tree is invalid: %v", i, err)
		}
	}
}

// TestInternDiscriminatesEachField verifies that every field canonicalEqual
// compares also participates in the subtree hash and in the equality check, so
// that a difference in any single field prevents collapsing. A field omitted
// from the hash would only show up as a rare missed sharing opportunity, but a
// field omitted from the equality check would be a correctness bug, so each is
// covered explicitly rather than left to the random test above.
func TestInternDiscriminatesEachField(t *testing.T) {
	base := func() *Entry {
		return &Entry{Kind: EntryKind_Directory, Contents: map[string]*Entry{
			"child": {Kind: EntryKind_File, Digest: []byte("0123456789abcdef")},
		}}
	}

	tests := []struct {
		name   string
		modify func(*Entry)
	}{
		{"kind", func(e *Entry) { e.Contents["child"].Kind = EntryKind_SymbolicLink }},
		{"digest", func(e *Entry) { e.Contents["child"].Digest = []byte("fedcba9876543210") }},
		{"digest length", func(e *Entry) { e.Contents["child"].Digest = []byte("0123456789abcde") }},
		{"executable", func(e *Entry) { e.Contents["child"].Executable = true }},
		{"target", func(e *Entry) { e.Contents["child"].Target = "elsewhere" }},
		{"problem", func(e *Entry) { e.Contents["child"].Problem = "broken" }},
		{"child name", func(e *Entry) {
			e.Contents["other"] = e.Contents["child"]
			delete(e.Contents, "child")
		}},
		{"child count", func(e *Entry) {
			e.Contents["extra"] = &Entry{Kind: EntryKind_File, Digest: []byte("aaaaaaaaaaaaaaaa")}
		}},
		{"nested vs flat", func(e *Entry) {
			e.Contents["child"] = &Entry{Kind: EntryKind_Directory, Contents: map[string]*Entry{
				"child": {Kind: EntryKind_File, Digest: []byte("0123456789abcdef")},
			}}
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var interner Interner
			unmodified := interner.Intern(base())
			modified := base()
			test.modify(modified)
			result := interner.Intern(modified)
			if result == unmodified {
				t.Error("entries differing in this field were incorrectly collapsed")
			}
		})
	}
}

// TestInternSharesAcrossDecodedCopies verifies the daemon's actual scenario:
// independently decoded copies of the same tree collapse onto one object graph.
func TestInternSharesAcrossDecodedCopies(t *testing.T) {
	tree := buildSyntheticTree(syntheticTreeSpec{
		files: 2000, directoryFanout: 4, filesPerDirectory: 8, duplicateContentFraction: 0.25,
	})
	copies := decodeCopies(t, tree, 3)

	var interner Interner
	for i, copied := range copies {
		copies[i] = interner.Intern(copied)
	}

	for i := 1; i < len(copies); i++ {
		if copies[i] != copies[0] {
			t.Errorf("decoded copy %d did not collapse onto copy 0", i)
		}
	}
	if !copies[0].Equal(tree, true) {
		t.Error("interned tree is not deep-equal to the source tree")
	}
}

// TestInternEvictsCollectedSubtrees verifies that the intern table does not pin
// the trees it canonicalizes. Without eviction a daemon-wide table would grow
// without bound and turn a memory optimization into a memory leak, so this
// checks that dropping every reference to an interned tree empties the table.
func TestInternEvictsCollectedSubtrees(t *testing.T) {
	interner := &Interner{}

	tree := interner.Intern(buildSyntheticTree(syntheticTreeSpec{
		files: 500, directoryFanout: 3, filesPerDirectory: 6,
	}))
	if size := interner.size(); size == 0 {
		t.Fatal("intern table is empty after interning a tree")
	}
	runtime.KeepAlive(tree)

	// Drop the tree, let it be collected, and sweep as a terminating session
	// would. Interning a tree makes every node reachable from the root, so a
	// single collection makes the whole tree unreachable at once.
	tree = nil
	_ = tree
	runtime.GC()
	interner.Sweep()

	if size := interner.size(); size != 0 {
		t.Errorf("intern table still holds %d buckets after the tree became unreachable", size)
	}
}

// TestRetention reports the live heap retained by structurally identical decoded
// trees under each interning configuration. This is the measurement that
// motivates the change; it is reported rather than asserted because absolute
// numbers depend on the platform and allocator, but the ratios are the result.
//
// Three configurations are measured per copy count:
//
//	baseline    what the daemon does today: N independently decoded trees.
//	persistent  N trees interned against a table that is retained, which is the
//	            daemon-wide configuration that also shares across sessions and
//	            cycles. The table's own footprint counts against the savings.
//	discarded   N trees interned against a table that is dropped afterward.
//	            Sharing established within the batch survives; the table costs
//	            nothing. This is the floor that persistent is measured against.
func TestRetention(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping retention measurement in short mode")
	}

	tree := buildSyntheticTree(defaultSyntheticTreeSpec)
	t.Logf("synthetic tree: %d synchronizable entries", tree.Count())

	for _, count := range []int{1, 2, 3, 6} {
		baseline := measureRetained(func() any {
			return decodeCopies(t, tree, count)
		})

		var tableSize int
		persistent := measureRetained(func() any {
			interner := &Interner{}
			copies := internedCopies(t, interner, tree, count)
			tableSize = interner.size()
			return []any{interner, copies}
		})

		discarded := measureRetained(func() any {
			return internedCopies(t, &Interner{}, tree, count)
		})

		t.Logf("%d copies: baseline %6.1f MB/%8d obj | persistent table %6.1f MB/%8d obj (%.2fx) | discarded table %6.1f MB/%8d obj (%.2fx) | %d canonical nodes",
			count,
			float64(baseline.bytes)/(1<<20), baseline.objects,
			float64(persistent.bytes)/(1<<20), persistent.objects,
			float64(baseline.bytes)/float64(persistent.bytes),
			float64(discarded.bytes)/(1<<20), discarded.objects,
			float64(baseline.bytes)/float64(discarded.bytes),
			tableSize,
		)
	}

	runtime.KeepAlive(tree)
}

// TestSyntheticTreeShapes reports the shape of the trees used by the retention
// measurement and the benchmarks, so that per-entry costs can be derived from
// their per-tree figures.
func TestSyntheticTreeShapes(t *testing.T) {
	for name, spec := range map[string]syntheticTreeSpec{
		"retention": defaultSyntheticTreeSpec,
		"benchmark": benchmarkTreeSpec,
	} {
		tree := buildSyntheticTree(spec)
		canonical := &Interner{}
		// The interned tree must stay reachable while the table is inspected, or
		// a collection plus sweep could retire the very buckets being counted.
		interned := canonical.Intern(tree.Copy(true))
		t.Logf("%s tree: %d entries, %d distinct subtrees (%.1f%% collapse within one tree)",
			name, tree.Count(), canonical.size(),
			100*(1-float64(canonical.size())/float64(tree.Count())),
		)
		runtime.KeepAlive(interned)
	}
}

// internedCopies decodes the specified number of copies of a tree and interns
// each against the supplied interner, returning the canonical trees.
func internedCopies(t testing.TB, interner *Interner, tree *Entry, count int) []*Entry {
	copies := decodeCopies(t, tree, count)
	for i, copied := range copies {
		copies[i] = interner.Intern(copied)
	}
	return copies
}

// randomTree generates a small random entry hierarchy from tiny alphabets, so
// that distinct trees frequently differ in only one field.
func randomTree(random *rand.Rand, depth int) *Entry {
	// Bias toward leaves as depth grows so that trees terminate.
	if depth >= 3 || random.Intn(depth+2) > 0 {
		switch random.Intn(4) {
		case 0:
			return &Entry{
				Kind:       EntryKind_File,
				Digest:     []byte{byte(random.Intn(3))},
				Executable: random.Intn(2) == 1,
			}
		case 1:
			return &Entry{Kind: EntryKind_SymbolicLink, Target: fmt.Sprintf("t%d", random.Intn(3))}
		case 2:
			return &Entry{Kind: EntryKind_Problematic, Problem: fmt.Sprintf("p%d", random.Intn(3))}
		default:
			return &Entry{Kind: EntryKind_Untracked}
		}
	}

	contents := make(map[string]*Entry)
	for i := 0; i < random.Intn(4); i++ {
		contents[fmt.Sprintf("n%d", random.Intn(4))] = randomTree(random, depth+1)
	}
	if len(contents) == 0 {
		return &Entry{Kind: EntryKind_Directory}
	}
	return &Entry{Kind: EntryKind_Directory, Contents: contents}
}
