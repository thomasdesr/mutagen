//go:build wiring_pending

// This file holds the RED half of the wiring contract: tests that describe what
// the daemon wiring needs and that fail against the Interner as it stands. It is
// build-tagged so that the branch stays green; run it with
//
//	go test -tags wiring_pending -race -run Pending ./pkg/synchronization/core/
package core

import (
	"sync"
	"testing"
)

// TestInternIsWriteFreeForAlreadyCanonicalTreesPending is RED.
//
// The local endpoint hook (endpoint.scan, just before it assigns e.snapshot)
// cannot satisfy Intern's exclusive-ownership precondition. An accelerated scan
// splices unmodified subtrees of the previously published snapshot straight into
// the new one (scan.go, contents[contentName] = directoryBaseline), so the tree
// handed to Intern contains nodes the watcher is already reading. Intern
// descends into them and assigns e.Contents[name] unconditionally, even when the
// assignment stores the value the map already holds — a write to a map another
// goroutine is reading, which is a data race regardless of the value written.
//
// The contract the wiring needs: interning a tree whose subtrees are already
// canonical performs no writes. Then re-interning a spliced tree is safe, and
// the local endpoint site needs no separate ownership argument.
//
// It fails today under -race with a write/read report on an Entry content map.
// Seam required: see "Wiring test contract" in FINDINGS-structural-sharing.md.
func TestInternIsWriteFreeForAlreadyCanonicalTreesPending(t *testing.T) {
	spec := syntheticTreeSpec{files: 2000, directoryFanout: 4, filesPerDirectory: 8, duplicateContentFraction: 0.25}

	interner := &Interner{}
	published := interner.Intern(buildSyntheticTree(spec))

	// Read the published tree the way endpoint.watchPoll does, having released
	// scanLock, while the next scan interns a tree that splices it in.
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

	// The spliced case in its purest form: the whole tree is already canonical.
	if reinterned := interner.Intern(published); reinterned != published {
		t.Error("re-interning a canonical tree did not return the same tree")
	}

	close(stop)
	reader.Wait()
}
