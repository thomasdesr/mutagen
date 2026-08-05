package synchronization

// ScanStrategy specifies how an endpoint should perform a single scan. It is
// chosen per-cycle by the controller, in contrast with ScanMode, which is a
// session-wide configuration setting bounding what the endpoint may do.
type ScanStrategy uint8

const (
	// ScanStrategyAccelerated indicates that the endpoint should use whatever
	// scan acceleration is available to it. It is the strategy for ordinary
	// synchronization cycles.
	ScanStrategyAccelerated ScanStrategy = iota + 1
	// ScanStrategyDrained indicates that the endpoint should first establish
	// that its filesystem watcher has reported every change completed before the
	// scan was requested, and then scan with acceleration. Endpoints that can't
	// establish that must perform a full scan instead. It is the strategy for
	// flushes, whose contract is that writes preceding the flush are
	// synchronized by the time it returns.
	ScanStrategyDrained
	// ScanStrategyFull indicates that the endpoint should perform a full (but
	// still warm) scan, bypassing any acceleration. It is the strategy for
	// flushes that explicitly request a rescan.
	ScanStrategyFull
)
