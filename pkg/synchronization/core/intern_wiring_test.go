package core

import (
	"sync"
	"testing"
)

// TestInternDoesNotMutatePublishedTree pins the property that makes daemon
// wiring safe at all: interning a tree the caller owns must not write to any
// node reachable from a tree that has already been published.
//
// It matters because endpoint.watchPoll copies the published snapshot pointer,
// releases scanLock, and then reads that tree (Equal, Diff) concurrently with
// whatever the next cycle is doing. Interning a new tree necessarily touches the
// nodes it shares with the published one — canonicalize returns them — so if
// that path wrote to them, every wiring site would be a data race against the
// watcher.
//
// Run under -race for the concurrency half of the property; the pointer
// comparison catches mutation even without it.
func TestInternDoesNotMutatePublishedTree(t *testing.T) {
	spec := syntheticTreeSpec{files: 2000, directoryFanout: 4, filesPerDirectory: 8, duplicateContentFraction: 0.25}

	// Intern and publish a tree, exactly as an endpoint would: the tree is owned
	// while it is interned and shared from that point on.
	interner := &Interner{}
	published := interner.Intern(buildSyntheticTree(spec))
	before := childPointers(published)

	// Read the published tree from another goroutine for the duration of the
	// second intern pass, the way the watcher reads it across a cycle boundary.
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
			if published.Count() == 0 {
				t.Error("published tree counted zero entries")
				return
			}
		}
	}()

	// Intern a structurally identical tree that shares no nodes with the
	// published one, which is what the next cycle's decode produces.
	next := interner.Intern(decodeCopies(t, published, 1)[0])

	close(stop)
	reader.Wait()

	if next != published {
		t.Error("the next cycle's tree did not collapse onto the published tree")
	}
	after := childPointers(published)
	if len(before) != len(after) {
		t.Fatalf("published tree changed shape: %d edges before, %d after", len(before), len(after))
	}
	for edge, child := range before {
		if after[edge] != child {
			t.Errorf("interning rewrote published edge %q", edge)
		}
	}
}

// childPointers returns every parent-to-child edge reachable from root, keyed by
// path, with the child's identity as the value. Comparing two of these detects
// any in-place rewriting of a content map, which entry equality cannot: a
// rewrite that substitutes an equal child leaves the tree deep-equal.
func childPointers(root *Entry) map[string]*Entry {
	edges := make(map[string]*Entry)
	var walk func(path string, entry *Entry)
	walk = func(path string, entry *Entry) {
		for name, child := range entry.Contents {
			childPath := path + "/" + name
			edges[childPath] = child
			if child != nil {
				walk(childPath, child)
			}
		}
	}
	walk("", root)
	return edges
}
