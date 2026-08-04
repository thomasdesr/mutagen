package core

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
)

// TestApplySharesUntouchedSubtrees verifies that Apply copies only the nodes on
// the root-to-change spine and shares every untouched subtree with the base by
// pointer. Entry is documented immutable (entry.proto), and the scan code
// already shares unmodified baseline subtrees by pointer (scan.go), so sharing
// derived trees is within the existing contract.
func TestApplySharesUntouchedSubtrees(t *testing.T) {
	// Build the fixture and record the subtrees whose identity we care about
	// before Apply has a chance to observe them.
	base := sharingFixture()
	baseA := base.Contents["a"]
	baseAB := baseA.Contents["b"]

	// Apply a single change deep inside the tree.
	result, err := Apply(base, []*Change{{Path: "a/b/c", New: tF3E}})
	if err != nil {
		t.Fatalf("unable to apply changes: %v", err)
	}

	// The spine must be freshly allocated, since each node on it needs a content
	// map that differs from the base's.
	if result == base {
		t.Error("result root is the base root")
	}
	if result.Contents["a"] == baseA {
		t.Error(`spine node "a" was not copied`)
	}
	if result.Contents["a"].Contents["b"] == baseAB {
		t.Error(`spine node "a/b" was not copied`)
	}

	// Everything off the spine must be shared with the base by pointer. These
	// are the assertions that fail against the deep-copying implementation.
	for name, expected := range map[string]*Entry{
		"g": base.Contents["g"],
		"i": base.Contents["i"],
	} {
		if result.Contents[name] != expected {
			t.Errorf("root sibling %q was copied rather than shared", name)
		}
	}
	for name, expected := range map[string]*Entry{
		"e": baseA.Contents["e"],
		"f": baseA.Contents["f"],
	} {
		if result.Contents["a"].Contents[name] != expected {
			t.Errorf("sibling %q under \"a\" was copied rather than shared", name)
		}
	}
	if result.Contents["a"].Contents["b"].Contents["d"] != baseAB.Contents["d"] {
		t.Error(`sibling "a/b/d" was copied rather than shared`)
	}

	// The change itself must have landed.
	if !result.Contents["a"].Contents["b"].Contents["c"].Equal(tF3E, true) {
		t.Error(`changed entry "a/b/c" does not match the new value`)
	}
}

// TestApplyDoesNotMutateBase is a regression pin: it passes against the
// deep-copying implementation and must keep passing once Apply path-copies,
// because path-copying is only contract-legal if the base is never written
// through a shared pointer.
func TestApplyDoesNotMutateBase(t *testing.T) {
	base := sharingFixture()
	snapshot := base.Copy(true)

	changes := []*Change{
		{Path: "a/b/c", New: tF3E},
		{Path: "g/h", New: nil},
		{Path: "a/e/new", New: tF2},
	}
	if _, err := Apply(base, changes); err != nil {
		t.Fatalf("unable to apply changes: %v", err)
	}

	if !base.Equal(snapshot, true) {
		t.Error("base was mutated by Apply")
	}
}

// TestApplyEmptyChangesReturnsBase is a regression pin on the existing fast path
// (apply.go): an empty change list returns the base pointer itself, not a copy.
func TestApplyEmptyChangesReturnsBase(t *testing.T) {
	base := sharingFixture()

	for _, changes := range [][]*Change{nil, {}} {
		result, err := Apply(base, changes)
		if err != nil {
			t.Fatalf("unable to apply changes: %v", err)
		} else if result != base {
			t.Error("empty change list did not return the base pointer")
		}
	}
}

// TestApplyRootReplacement is a regression pin covering root replacement, both
// as the sole change (the fast path in apply.go) and mid-change-list, where the
// replacement discards any spine built by earlier changes.
func TestApplyRootReplacement(t *testing.T) {
	base := sharingFixture()

	// Root replacement as the sole change.
	result, err := Apply(base, []*Change{{Path: "", New: tF1}})
	if err != nil {
		t.Fatalf("unable to apply root replacement: %v", err)
	} else if !result.Equal(tF1, true) {
		t.Error("sole root replacement did not return the new root")
	}

	// Root replacement mid-change-list. The change before it must be discarded
	// and the change after it must apply to the replacement root.
	changes := []*Change{
		{Path: "a/b/c", New: tF2},
		{Path: "", New: tD1},
		{Path: "file", New: tF3E},
	}
	result, err = Apply(base, changes)
	if err != nil {
		t.Fatalf("unable to apply changes: %v", err)
	}
	expected := dir(map[string]*Entry{"file": tF3E})
	if !result.Equal(expected, true) {
		t.Error("mid-list root replacement did not produce the expected tree")
	}
}

// TestApplyAllocationsDoNotScaleWithTreeSize is the burst-killer contract:
// applying one change to a large tree must not allocate per node. A single-node
// change touches a spine of a handful of directories, so the true cost is a
// couple of dozen allocations (one Entry plus one content map per spine node,
// the path split, and map bucket growth). The bound is set at 5% of the node
// count rather than a small constant so that path depth, map rebuild strategy,
// and any per-call bookkeeping the implementation needs all fit comfortably
// while still failing by more than an order of magnitude against a full deep
// copy, which allocates at least one Entry per node.
func TestApplyAllocationsDoNotScaleWithTreeSize(t *testing.T) {
	base := buildSyntheticTree(syntheticTreeSpec{
		files:                    50000,
		directoryFanout:          6,
		filesPerDirectory:        12,
		duplicateContentFraction: 0.25,
	})
	nodes := base.Count()

	// Verify up front that the change path resolves. Without this, an
	// unresolvable path would make Apply fail early, allocate almost nothing,
	// and turn this test into a false pass.
	const changePath = "internal/internal/BUILD.bazel"
	if resolveEntry(base, changePath) == nil {
		t.Fatalf("synthetic tree does not contain %q", changePath)
	}
	changes := []*Change{{Path: changePath, New: tF3E}}

	// Release the measured tree once the test finishes. The sink is package-level
	// and this tree is large, and testing_synthetic_test.go measures retained
	// heap, so leaving it anchored would inflate later measurements.
	t.Cleanup(func() { applySink = nil })

	var applyErr error
	allocations := testing.AllocsPerRun(3, func() {
		result, err := Apply(base, changes)
		if err != nil {
			applyErr = err
			return
		}
		applySink = result
	})
	if applyErr != nil {
		t.Fatalf("unable to apply changes: %v", applyErr)
	}
	if applySink == nil {
		t.Fatal("Apply returned a nil result")
	}
	if !applySink.Contents["internal"].Contents["internal"].Contents["BUILD.bazel"].Equal(tF3E, true) {
		t.Fatal("change did not land in the measured result")
	}

	bound := float64(nodes) * 0.05
	if allocations > bound {
		t.Errorf(
			"Apply of one change allocated %.0f times over a %d node tree, want at most %.0f",
			allocations, nodes, bound,
		)
	}
}

// applySink anchors the result of a measured Apply call so that neither the
// compiler nor the garbage collector can treat the call as dead.
var applySink *Entry

// TestApplySerializesCorrectlyUnderSharing verifies that a tree assembled from
// shared subtrees still serializes to exactly the expected content, and that the
// base serializes to its original content. The expected trees are constructed by
// hand rather than by calling Apply, since Apply is the implementation under
// replacement and cannot serve as its own oracle. This is a regression pin: it
// passes today and must keep passing under path-copying.
func TestApplySerializesCorrectlyUnderSharing(t *testing.T) {
	base := sharingFixture()

	changes := []*Change{
		{Path: "a/b/c", New: tF3E},
		{Path: "g/h", New: nil},
	}
	result, err := Apply(base, changes)
	if err != nil {
		t.Fatalf("unable to apply changes: %v", err)
	}

	// The expected result: "a/b/c" replaced, "g/h" removed, everything else
	// untouched.
	expectedResult := dir(map[string]*Entry{
		"a": dir(map[string]*Entry{
			"b": dir(map[string]*Entry{"c": tF3E, "d": tF2}),
			"e": dir(map[string]*Entry{"file": tF1}),
			"f": tF2,
		}),
		"g": dir(map[string]*Entry{}),
		"i": tF3,
	})

	// Round-trip both trees through the wire format. Protocol Buffers map
	// serialization is not order-deterministic, so we compare decoded trees
	// rather than encoded bytes.
	encodedResult := marshalEntry(t, result)
	encodedBase := marshalEntry(t, base)
	if bytes.Equal(encodedResult, encodedBase) {
		t.Error("result and base serialized identically despite differing content")
	}
	if decoded := unmarshalEntry(t, encodedResult); !decoded.Equal(expectedResult, true) {
		t.Error("decoded result did not match the expected tree")
	}
	if decoded := unmarshalEntry(t, encodedBase); !decoded.Equal(sharingFixture(), true) {
		t.Error("decoded base did not match the original fixture")
	}
}

// sharingFixture builds a wide, three-level tree with untouched siblings at every
// level of the "a/b/c" spine, so that pointer sharing can be checked
// independently at the root, at "a", and at "a/b". Each call returns a fresh
// spine so that tests cannot interfere with one another.
func sharingFixture() *Entry {
	return dir(map[string]*Entry{
		"a": dir(map[string]*Entry{
			"b": dir(map[string]*Entry{"c": tF1, "d": tF2}),
			"e": dir(map[string]*Entry{"file": tF1}),
			"f": tF2,
		}),
		"g": dir(map[string]*Entry{"h": tF1}),
		"i": tF3,
	})
}

// dir builds a directory entry with the specified contents. It exists to keep
// the deeply nested sharing fixtures readable.
func dir(contents map[string]*Entry) *Entry {
	return &Entry{Kind: EntryKind_Directory, Contents: contents}
}

// resolveEntry looks up a slash-separated path within a tree, returning nil if
// any component is missing.
func resolveEntry(root *Entry, path string) *Entry {
	current := root
	for _, name := range strings.Split(path, "/") {
		child, ok := current.Contents[name]
		if !ok {
			return nil
		}
		current = child
	}
	return current
}

// marshalEntry encodes an entry to its wire format.
func marshalEntry(t testing.TB, entry *Entry) []byte {
	t.Helper()

	encoded, err := proto.Marshal(entry)
	if err != nil {
		t.Fatalf("unable to marshal entry: %v", err)
	}
	return encoded
}

// unmarshalEntry decodes an entry from its wire format.
func unmarshalEntry(t testing.TB, encoded []byte) *Entry {
	t.Helper()

	entry := &Entry{}
	if err := proto.Unmarshal(encoded, entry); err != nil {
		t.Fatalf("unable to unmarshal entry: %v", err)
	}
	return entry
}
