// This file pins the platform behavior that drained flushes depend on: a
// recursive watcher must report a file created after a batch of writes only
// after it has reported those writes. A drained flush proves that the watcher
// has caught up with everything a client wrote before requesting the flush by
// creating a sentinel file under the synchronization root and waiting for its
// event, and that proof is only as good as this ordering property.
package watching

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// collectUntilSentinel consumes watcher events until sentinel is observed,
// returning the set of event paths delivered ahead of it.
func collectUntilSentinel(t *testing.T, watcher RecursiveWatcher, sentinel string) map[string]bool {
	// Indicate that this is a helper function.
	t.Helper()

	// Create a deadline for sentinel observation and ensure its cancellation.
	deadline := time.NewTimer(maximumEventWaitTime)
	defer deadline.Stop()

	// Consume events until the sentinel arrives.
	preceding := make(map[string]bool)
	for {
		select {
		case path := <-watcher.Events():
			if path == sentinel {
				return preceding
			}
			preceding[path] = true
		case err := <-watcher.Errors():
			t.Fatal("watcher error:", err)
		case <-deadline.C:
			t.Fatal("sentinel not observed, saw:", preceding)
		}
	}
}

// TestRecursiveWatcherOrdersSentinelAfterPrecedingWrites verifies that events
// for writes spread across a directory hierarchy are all delivered before the
// event for a file created after them.
func TestRecursiveWatcherOrdersSentinelAfterPrecedingWrites(t *testing.T) {
	// Skip this test if recursive watching is unsupported.
	if !RecursiveWatchingSupported {
		t.Skip("recursive watching unsupported")
	}

	// Create a temporary directory (that will be automatically removed) with a
	// hierarchy of subdirectories, so that the ordering property is exercised
	// across the directory boundaries that watchers coalesce events within.
	directory := t.TempDir()
	subdirectories := []string{"a", "b", "c", "d/e", "f/g/h"}
	for _, subdirectory := range subdirectories {
		if err := os.MkdirAll(filepath.Join(directory, subdirectory), 0700); err != nil {
			t.Fatal("unable to create subdirectory:", err)
		}
	}

	// Create the watcher and defer its termination.
	watcher, err := NewRecursiveWatcher(directory)
	if err != nil {
		t.Fatal("unable to establish watch:", err)
	}
	defer watcher.Terminate()

	// Establish that the watch is live. Watch establishment is asynchronous
	// within the monitoring facility, and changes made in that window may never
	// be reported, so the ordering property can only be meaningfully tested once
	// at least one event has come through.
	if err := os.WriteFile(filepath.Join(directory, "warmup"), nil, 0600); err != nil {
		t.Fatal("unable to create warmup file:", err)
	}
	verifyWatchEvent(t, watcher, map[string]bool{"warmup": true})

	// Write a batch of files and then a sentinel, requiring every file in the
	// batch to be reported ahead of the sentinel. A single round could hold by
	// luck, so run enough rounds to catch a watcher that reorders.
	for round := 0; round < 20; round++ {
		batch := make(map[string]bool)
		for _, subdirectory := range subdirectories {
			for index := 0; index < 6; index++ {
				relative := fmt.Sprintf("%s/file-%d-%d", subdirectory, round, index)
				if err := os.WriteFile(filepath.Join(directory, relative), []byte("data"), 0600); err != nil {
					t.Fatal("unable to create test file:", err)
				}
				batch[relative] = true
			}
		}
		sentinel := fmt.Sprintf("sentinel-%d", round)
		if err := os.WriteFile(filepath.Join(directory, sentinel), nil, 0600); err != nil {
			t.Fatal("unable to create sentinel file:", err)
		}
		preceding := collectUntilSentinel(t, watcher, sentinel)
		for relative := range batch {
			if !preceding[relative] {
				t.Fatalf("round %d: event for %s not delivered before sentinel", round, relative)
			}
		}
	}
}
