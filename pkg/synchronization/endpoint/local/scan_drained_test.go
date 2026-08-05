// This file holds the contract that fast flushes rest on: a drained scan must
// see every change that completed before it was requested, without paying for
// the full re-walk of the synchronization root that a forced rescan performs.
package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/filesystem/watching"
	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

const (
	// accelerationWaitTime is the maximum amount of time that
	// newEndpointForTest will wait for scan acceleration to become available.
	accelerationWaitTime = 10 * time.Second
)

// newEndpointForTest creates a local endpoint rooted at a temporary directory,
// returning the endpoint and its root. Endpoint state is confined to a
// temporary data directory so that the test leaves the user's own Mutagen data
// untouched.
func newEndpointForTest(t *testing.T, configuration *synchronization.Configuration) (*endpoint, string) {
	// Indicate that this is a helper function.
	t.Helper()

	// Redirect endpoint state away from the user's data directory.
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())

	// Create the synchronization root.
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal("unable to create synchronization root:", err)
	}

	// Create the endpoint and ensure its shutdown.
	created, err := NewEndpoint(
		logging.NewLogger(logging.LevelError, os.Stderr),
		root,
		"session-for-drain-test",
		synchronization.Version_Version1,
		configuration,
		true,
	)
	if err != nil {
		t.Fatal("unable to create endpoint:", err)
	}
	t.Cleanup(func() { created.Shutdown() })

	return created.(*endpoint), root
}

// waitForAcceleration blocks until the endpoint's watching Goroutine has
// established a watch and enabled scan acceleration.
func waitForAcceleration(t *testing.T, e *endpoint) {
	// Indicate that this is a helper function.
	t.Helper()

	// Poll for acceleration availability.
	deadline := time.Now().Add(accelerationWaitTime)
	for {
		e.scanLock.Lock()
		accelerated := e.accelerate
		e.scanLock.Unlock()
		if accelerated {
			return
		} else if time.Now().After(deadline) {
			t.Fatal("scan acceleration never became available")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// contentNames returns the names of the entries at the root of a snapshot.
func contentNames(snapshot *core.Snapshot) map[string]bool {
	names := make(map[string]bool)
	if snapshot.Content != nil {
		for name := range snapshot.Content.Contents {
			names[name] = true
		}
	}
	return names
}

// TestDrainedScanObservesPrecedingWrite pins the fast flush contract: a file
// written before a drained scan is requested must appear in that scan's
// snapshot even though the scan uses acceleration rather than re-walking the
// root.
func TestDrainedScanObservesPrecedingWrite(t *testing.T) {
	// Skip if the platform can't provide the recursive watch that draining
	// requires, since the endpoint would silently fall back to a full scan and
	// the test would prove nothing.
	if !watching.RecursiveWatchingSupported {
		t.Skip("recursive watching unsupported")
	}

	// Create an endpoint with default configuration, which selects recursive
	// watching and accelerated scanning, and wait for acceleration.
	e, root := newEndpointForTest(t, &synchronization.Configuration{})
	waitForAcceleration(t, e)

	// Write a file and immediately request a drained scan, requiring the file to
	// appear. A single round could pass by luck, since the watcher may have
	// reported the write before the scan started, so run enough rounds that a
	// drain which did nothing would be caught.
	for round := 0; round < 20; round++ {
		name := fmt.Sprintf("file-%d", round)
		if err := os.WriteFile(filepath.Join(root, name), []byte("data"), 0600); err != nil {
			t.Fatal("unable to create test file:", err)
		}
		snapshot, err, _ := e.Scan(context.Background(), nil, synchronization.ScanStrategyDrained)
		if err != nil {
			t.Fatal("drained scan failed:", err)
		}
		if names := contentNames(snapshot); !names[name] {
			t.Fatalf("round %d: drained scan did not observe %s, saw: %v", round, name, names)
		}
	}
}

// TestDrainedScanFallsBackWithoutWatching verifies that a drained scan of an
// endpoint with no event stream to drain still returns correct content, by
// falling back to a full scan.
func TestDrainedScanFallsBackWithoutWatching(t *testing.T) {
	// Create an endpoint with watching disabled.
	e, root := newEndpointForTest(t, &synchronization.Configuration{
		WatchMode: synchronization.WatchMode_WatchModeNoWatch,
	})

	// Write a file and require a drained scan to observe it.
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0600); err != nil {
		t.Fatal("unable to create test file:", err)
	}
	snapshot, err, _ := e.Scan(context.Background(), nil, synchronization.ScanStrategyDrained)
	if err != nil {
		t.Fatal("drained scan failed:", err)
	}
	if names := contentNames(snapshot); !names["file"] {
		t.Fatalf("drained scan did not observe file, saw: %v", names)
	}
}

// TestReadOnlyEndpointDoesNotDrain verifies that a read-only endpoint has no
// drain path at all. Draining creates a sentinel file under the synchronization
// root, and a read-only endpoint is one that this process promises never to
// write to, so it re-scans in full instead.
func TestReadOnlyEndpointDoesNotDrain(t *testing.T) {
	// Create an alpha endpoint in a one-way-safe session, which is the
	// configuration that makes an endpoint read-only.
	e, root := newEndpointForTest(t, &synchronization.Configuration{
		SynchronizationMode: core.SynchronizationMode_SynchronizationModeOneWaySafe,
	})
	if !e.readOnly {
		t.Fatal("endpoint is not read-only")
	}

	// Require that the endpoint has no way to request a drain.
	if e.drainRequests != nil {
		t.Error("read-only endpoint accepts drain requests")
	}

	// Require that a drained scan still returns correct content.
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0600); err != nil {
		t.Fatal("unable to create test file:", err)
	}
	snapshot, err, _ := e.Scan(context.Background(), nil, synchronization.ScanStrategyDrained)
	if err != nil {
		t.Fatal("drained scan failed:", err)
	}
	if names := contentNames(snapshot); !names["file"] {
		t.Fatalf("drained scan did not observe file, saw: %v", names)
	}
}

// TestScanRejectsUnknownStrategy verifies that an unset or unrecognized scan
// strategy fails loudly rather than silently selecting a scan mode.
func TestScanRejectsUnknownStrategy(t *testing.T) {
	// Create an endpoint.
	e, _ := newEndpointForTest(t, &synchronization.Configuration{})

	// Require a scan with the zero-valued strategy to fail.
	if _, err, _ := e.Scan(context.Background(), nil, synchronization.ScanStrategy(0)); err == nil {
		t.Error("scan with unknown strategy succeeded")
	}
}
