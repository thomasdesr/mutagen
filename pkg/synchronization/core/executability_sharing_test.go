package core

import (
	"testing"
)

// TestPropagateExecutabilitySharesUntouchedSubtrees verifies that
// PropagateExecutability copies only the nodes on the spine leading to a file
// whose executability actually changes, sharing every other subtree with the
// target by pointer.
func TestPropagateExecutabilitySharesUntouchedSubtrees(t *testing.T) {
	// Build three independent object graphs. The target is a deep copy so that
	// none of its nodes are shared with the ancestor or source, which makes a
	// pointer comparison against the target's children meaningful rather than
	// something an ancestor-sharing implementation could satisfy by accident.
	ancestor := executabilityFixture()
	source := executabilityFixture()
	target := executabilityFixture().Copy(true)

	// Strip executability from exactly one file, so that exactly one spine needs
	// to change. Every other file already agrees with the ancestor and source.
	// The snapshot is taken after this so that it captures the target as
	// PropagateExecutability will see it.
	target.Contents["changed"].Contents["script"].Executable = false
	snapshot := target.Copy(true)

	result := PropagateExecutability(ancestor, source, target)

	// The spine to the changed file must be freshly allocated. The root is
	// included because executability_test.go pins a fresh root unconditionally.
	if result == target {
		t.Error("result root is the target root")
	}
	if result.Contents["changed"] == target.Contents["changed"] {
		t.Error(`spine node "changed" was not copied`)
	}

	// The propagation must have landed, and must not have written through to the
	// target.
	if !result.Contents["changed"].Contents["script"].Executable {
		t.Error("executability was not propagated to the changed file")
	}
	if !target.Equal(snapshot, true) {
		t.Error("target was mutated by PropagateExecutability")
	}

	// Everything off the spine must be shared with the target by pointer. These
	// are the assertions that fail against the deep-copying implementation.
	for _, name := range []string{"untouched-a", "untouched-b", "topfile"} {
		if result.Contents[name] != target.Contents[name] {
			t.Errorf("subtree %q was copied rather than shared with the target", name)
		}
	}
}

// TestPropagateExecutabilityNoOpSharesAllSubtrees pins the behavior when no
// executability change is needed anywhere: every top-level subtree of the result
// is the target's subtree by pointer.
//
// This is the weaker of the two candidate no-op contracts. The stronger form —
// returning the target pointer itself — is not implementable without changing
// existing assertions: TestPropagateExecutability in executability_test.go calls
// PropagateExecutability(nil, nil, stripped), which propagates nothing, and then
// fails with "executability propagation did not make entry copy" if the result is
// the target pointer. Two more assertions in that test pin the same thing. A
// fresh root costs one Entry and one map regardless of tree size, so the
// remaining win is entirely in the subtrees, which this test pins. Relaxing the
// existing assertions to unlock `result == target` is a separate call for the
// implementer; nothing here depends on it.
func TestPropagateExecutabilityNoOpSharesAllSubtrees(t *testing.T) {
	ancestor := executabilityFixture()
	source := executabilityFixture()
	target := executabilityFixture().Copy(true)
	snapshot := target.Copy(true)

	result := PropagateExecutability(ancestor, source, target)

	if !result.Equal(target, true) {
		t.Error("no-op propagation changed the tree's content")
	}
	if !target.Equal(snapshot, true) {
		t.Error("target was mutated by PropagateExecutability")
	}
	for name := range target.Contents {
		if result.Contents[name] != target.Contents[name] {
			t.Errorf("subtree %q was copied despite no executability change", name)
		}
	}
}

// executabilityFixture builds a tree in which one file is executable and the rest
// are not, with untouched sibling subtrees at the root level so that sharing can
// be checked independently of the spine leading to the executable file.
func executabilityFixture() *Entry {
	return dir(map[string]*Entry{
		"changed": dir(map[string]*Entry{"script": tF3E}),
		"untouched-a": dir(map[string]*Entry{
			"file": tF1,
			"deep": dir(map[string]*Entry{"file": tF2}),
		}),
		"untouched-b": dir(map[string]*Entry{"file": tF2}),
		"topfile":     tF1,
	})
}
