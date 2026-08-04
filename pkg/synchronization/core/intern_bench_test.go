package core

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

// benchmarkTreeSpec is smaller than the retention measurement's tree so that a
// benchmark can afford many iterations, while keeping the same shape.
var benchmarkTreeSpec = syntheticTreeSpec{
	files:                    20000,
	directoryFanout:          6,
	filesPerDirectory:        12,
	duplicateContentFraction: 0.25,
}

// BenchmarkDecode measures decoding one tree. The interning pass runs
// immediately after a decode, so decode cost is the yardstick its cost should be
// judged against.
func BenchmarkDecode(b *testing.B) {
	tree := buildSyntheticTree(benchmarkTreeSpec)
	encoded := marshalTree(b, tree)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		decodeTree(b, encoded)
	}
}

// BenchmarkInternCold measures interning a tree against an empty table, which is
// what the first tree of a session pays: every subtree is inserted.
func BenchmarkInternCold(b *testing.B) {
	tree := buildSyntheticTree(benchmarkTreeSpec)
	encoded := marshalTree(b, tree)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		fresh := decodeTree(b, encoded)
		interner := &Interner{}
		b.StartTimer()

		interner.Intern(fresh)
	}
}

// BenchmarkInternWarm measures interning a tree against a table that already
// contains it, which is the steady-state per-cycle cost in the daemon: every
// subtree is found and the pass allocates nothing.
func BenchmarkInternWarm(b *testing.B) {
	tree := buildSyntheticTree(benchmarkTreeSpec)
	encoded := marshalTree(b, tree)

	interner := &Interner{}
	canonical := interner.Intern(decodeTree(b, encoded))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		fresh := decodeTree(b, encoded)
		b.StartTimer()

		if interner.Intern(fresh) != canonical {
			b.Fatal("interning a decoded copy did not yield the canonical tree")
		}
	}
}

// BenchmarkDiffIdenticalUnshared measures diffing two structurally identical but
// unshared trees, which is what the daemon does today.
func BenchmarkDiffIdenticalUnshared(b *testing.B) {
	tree := buildSyntheticTree(benchmarkTreeSpec)
	encoded := marshalTree(b, tree)
	base, target := decodeTree(b, encoded), decodeTree(b, encoded)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if changes := Diff(base, target); len(changes) != 0 {
			b.Fatalf("identical trees produced %d changes", len(changes))
		}
	}
}

// BenchmarkDiffIdenticalShared measures diffing two identical trees that have
// been interned onto a shared object graph, which the pointer-equality fast path
// reduces to a single comparison.
func BenchmarkDiffIdenticalShared(b *testing.B) {
	tree := buildSyntheticTree(benchmarkTreeSpec)
	encoded := marshalTree(b, tree)
	interner := &Interner{}
	base := interner.Intern(decodeTree(b, encoded))
	target := interner.Intern(decodeTree(b, encoded))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if changes := Diff(base, target); len(changes) != 0 {
			b.Fatalf("identical trees produced %d changes", len(changes))
		}
	}
}

// marshalTree serializes a tree as the daemon serializes snapshots.
func marshalTree(b *testing.B, tree *Entry) []byte {
	b.Helper()
	encoded, err := proto.Marshal(&Snapshot{Content: tree})
	if err != nil {
		b.Fatalf("unable to marshal snapshot: %v", err)
	}
	return encoded
}

// decodeTree decodes a tree from serialized snapshot bytes.
func decodeTree(b *testing.B, encoded []byte) *Entry {
	b.Helper()
	snapshot := &Snapshot{}
	if err := proto.Unmarshal(encoded, snapshot); err != nil {
		b.Fatalf("unable to unmarshal snapshot: %v", err)
	}
	return snapshot.Content
}
