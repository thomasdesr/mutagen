// This file holds the remote endpoint's wiring contract: what interning in
// endpointClient.Scan must achieve. Its harness (connectedEndpoint,
// isolatedDataDirectory, populatedRoot) lives in scan_roundtrip_test.go: a real
// endpointClient talking to a real ServeEndpoint over net.Pipe. Nothing here
// stubs the thing under test.
package remote

import (
	"context"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/synchronization"
)

// TestScanCollapsesRepeatedCyclesPending is RED: it pins the per-cycle half of
// the remote endpoint wiring. Nothing in endpointClient.Scan interns today, so
// each cycle's snapshot is a fresh object graph.
//
// Two scans of an unchanged root decode two structurally identical trees. Once
// Scan interns its freshly decoded snapshot against an Interner that outlives the
// cycle, the second decode collapses onto the first and the roots are the same
// object. Root identity is the whole point: it is what makes core.Diff of an
// unchanged tree a pointer comparison instead of a full walk.
//
// This test is written so that it does not presume where the Interner comes from:
// any wiring in which the client holds or is given a table that survives across
// cycles satisfies it.
func TestScanCollapsesRepeatedCyclesPending(t *testing.T) {
	isolatedDataDirectory(t)
	client := connectedEndpoint(t, populatedRoot(t), "session-repeated-cycles")

	first, err, _ := client.Scan(context.Background(), nil, synchronization.ScanStrategyFull)
	if err != nil {
		t.Fatal("unable to perform first scan:", err)
	}
	second, err, _ := client.Scan(context.Background(), nil, synchronization.ScanStrategyFull)
	if err != nil {
		t.Fatal("unable to perform second scan:", err)
	}

	if !second.Content.Equal(first.Content, true) {
		t.Fatal("scans of an unchanged root returned unequal content")
	}
	if second.Content != first.Content {
		t.Error("the second cycle's snapshot did not collapse onto the first cycle's")
	}
}

// TestScanCollapsesAcrossSessionsPending is RED: it pins the cross-session half
// of the wiring, which is the claim that motivates one daemon-wide Interner
// rather than one per endpoint.
//
// Two independent client/server pairs over the same root are two sessions
// synchronizing the same directory — the shape the live daemon runs, where the
// duplicate trees are the bulk of the retained heap. They collapse only if both
// clients intern against the same table.
//
// It fails today because there is no interning at all. It will also fail after a
// wiring that gives each endpoint its own table, which is the point: passing
// requires the table to be reachable from both endpoints. See "Wiring test
// contract" in FINDINGS-structural-sharing.md for the seam that admits this.
func TestScanCollapsesAcrossSessionsPending(t *testing.T) {
	isolatedDataDirectory(t)
	root := populatedRoot(t)

	first, err, _ := connectedEndpoint(t, root, "session-one").Scan(context.Background(), nil, synchronization.ScanStrategyFull)
	if err != nil {
		t.Fatal("unable to scan from the first session:", err)
	}
	second, err, _ := connectedEndpoint(t, root, "session-two").Scan(context.Background(), nil, synchronization.ScanStrategyFull)
	if err != nil {
		t.Fatal("unable to scan from the second session:", err)
	}

	if !second.Content.Equal(first.Content, true) {
		t.Fatal("two sessions scanning one root returned unequal content")
	}
	if second.Content != first.Content {
		t.Error("the second session's snapshot did not collapse onto the first session's")
	}
}
