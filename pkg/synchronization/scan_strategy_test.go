package synchronization

import "testing"

// TestScanStrategyForCycle verifies the mapping from a cycle's triggering flush
// request, if any, to the scan strategy that endpoints are given. The rest of
// the synchronization loop needs a live session to exercise, but this decision
// is the part of it that the flush flag actually changes.
func TestScanStrategyForCycle(t *testing.T) {
	tests := []struct {
		name     string
		request  *flushRequest
		expected ScanStrategy
	}{
		{"untriggered cycle", nil, ScanStrategyAccelerated},
		{"flush", &flushRequest{}, ScanStrategyDrained},
		{"flush forcing a rescan", &flushRequest{forceRescan: true}, ScanStrategyFull},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if strategy := scanStrategyForCycle(test.request); strategy != test.expected {
				t.Errorf("strategy %d does not match expected %d", strategy, test.expected)
			}
		})
	}
}
