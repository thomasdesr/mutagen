# Independent memory analysis: mutagen daemon at ~1.9GB live heap

Scope: mutagen v0.17.5, darwin/arm64, Go 1.26, 4 two-way sessions
(2x ~703k-entry monorepo, 2x ~217k-entry worktree parent; counts from
read-only `mutagen sync list --long`), xxh128 digests, zstd transport,
FSEvents recursive watching (the "portable" watcher reifies to
`reifiedWatchModeRecursive` on darwin — the profile confirms via
`watchRecursive`), accelerated scans. Profiles: heap-initial (2262MB),
heap-post (1854MB, steady state). The initial->post diff shows shrinkage,
not growth: this is a stable working set, not a leak. The prior 3.5GB RSS
drift is consistent with default GOGC=100 doubling over a ~1.9-2.3GB live
heap plus allocation-burst peaks (see the Apply note below).

## Attribution

All numbers from `heap-post.pb.gz` (1854MB total, 20.2M objects) plus
synthetic footprint measurements in
`pkg/synchronization/core/memfootprint_analysis_test.go` run on this
machine (Go 1.26 runtime, arm64):

- Entry tree: **232.5 B/entry** live (703k-entry tree = 163MB, 217k = 50MB)
- Proto digest cache: **227 B/file** (Cache/CacheEntry/Timestamp/digest/path key)
- Full-tree dedupe pass (worst case, zero pointer sharing): **70ms**, reclaims
  the entire duplicate

### The heap decomposes into four families

**1. Entry-tree copies — ~1280MB (69%).** Each session retains ~3
independent copies of the same tree at steady state:

| Copy | Owner | Allocation site in profile |
|---|---|---|
| ancestor | `controller.run` -> archive load | `synchronize.LoadAndUnmarshalProtobuf.func9`: 398.5MB |
| local snapshot | local endpoint `e.snapshot`, aliased by controller alpha snapshot | `NewEndpoint.LoadAndUnmarshalProtobuf.func3` 316.5MB + scanner allocations under `watchRecursive`/`Scan` (453MB + 267.7MB, includes caches below) |
| remote snapshot | controller beta snapshot (unmarshaled in `endpointClient.Scan`) | 144.8MB under `proto.Unmarshal` |

Arithmetic: 3 copies x (2x163MB + 2x50MB) = 1278MB. The scanner-side flat
allocations (`scanner.file` 196MB, `scanner.directory` 195MB,
`reflect.unsafe_New` 591MB, `consumeBytesNoZero` 69MB,
`consumeStringValueValidateUTF8` 67MB, `mapassign` 121MB, `readdir` 49.5MB)
are these trees viewed by allocation site rather than owner. The copies are
never shared: archive load, local scan, and remote unmarshal each allocate
independently, even though at steady state ("Watching for changes") all
three trees are semantically equal.

**2. Path-keyed digest + ignore caches — ~430MB (23%).** Per local
endpoint: `Cache.Entries` (map[full-path]*CacheEntry, where each entry is a
96B proto + separate `timestamppb.Timestamp` (48B) + 16B digest allocation
+ ~70B path key + map overhead = measured 227 B/file) -> 2x142MB + 2x44MB =
372MB, plus `ignoreCache` maps (~60MB; their `IgnoreCacheKey` path strings
share data with cache keys — `Entry.walk`'s 139.5MB flat is these
path strings, allocated during baseline-reuse propagation in accelerated
scans and retained as cache/ignore-cache keys).

**3. Serialized rsync baseline — 144MB (8%).** `endpointClient` retains
`lastSnapshotBytes` — the full serialized snapshot — as the rsync baseline
for delta-encoded scan transmission (`rsync.(*Engine).PatchBytes` ->
`bytes.growSlice` 144MB). Arithmetic check: ~85 B/entry serialized x
(2x703k + 2x217k) ~= 145MB. This is byte-for-byte redundant with the
deserialized beta snapshot the controller already retains, because **both
sides already marshal with `proto.MarshalOptions{Deterministic: true}`**
(client.go:237, server.go:299) — the bytes can be regenerated on demand.

**4. zstd stream state — ~100MB (5%).** `sspl/pkg/compression/zstd` calls
`zstd.NewWriter`/`NewReader` with **no options**: default 8MB window,
GOMAXPROCS-way encoder concurrency. `fastBase.ensureHist` retains 64MB
(16MB/session encoder history) and `startStreamDecoder.func2` 36MB
(9MB/session decoder window) — for connections that sit idle at steady
state.

## Ranked interventions

### 1. Subtree dedupe across the three per-session tree copies — save ~500-720MB

Mechanism: `Entry` is documented immutable ("must not be modified",
entry.pb.go), so equal subtrees can be pointer-shared. Add a
`Dedupe(base, novel *Entry) *Entry` pass (recursive; pointer-equality
short-circuit; return `base` subtree when deeply equal) and apply it in
`controller.synchronize`: dedupe beta content against alpha content after
scans, and the new ancestor against alpha content after `Apply`. At steady
state all three trees collapse to ~1 plus divergence (directories
containing Untracked/Problematic entries won't share with the ancestor —
the ancestor is synchronizable-only — but their clean subdirectories still
share, since recursion shares children independently; exact-equality
sharing also means ancestor validity is preserved by construction).

Arithmetic: up to 2/3 x 1278MB = 852MB; discounting untracked-content
divergence and mid-change windows, 500-720MB. Measured cost: 70ms per
worst-case full pass on the 703k tree, and after the first pass pointer
short-circuits make subsequent passes far cheaper (accelerated scans
already reuse baseline subtree pointers between consecutive snapshots).

Cost/risk: moderate. The invariant to audit is that nothing mutates a
shared `Entry` in place: `core.Apply` already deep-copies before mutating
(safe, but it re-forks the entire ancestor each cycle — see below), and
`Transition`/scanner treat entries as read-only. Related churn fix:
`Apply` runs `base.Copy(true)` — a full 163MB deep copy of the big tree
for *any* non-empty change list, every sync cycle — and deep-copies each
`change.New`. A copy-on-write apply (copy only nodes along change paths,
share `change.New` directly since it's immutable) makes ancestor sharing
persist across cycles instead of being destroyed and re-established, and
removes a per-cycle allocation burst that inflates RSS peaks during active
development.

Validation: heap profile of the live daemon before/after; property test
that `Reconcile(Dedupe(...))` == `Reconcile(...)`; existing core test suite.

### 2. Compact in-memory digest cache — save ~180MB

Mechanism: keep the `Cache` proto only at the disk (de)serialization
boundary; in memory use `map[string]compactCacheEntry` with a value struct
(mode/mtime/size/fileID inline, `[16]byte` digest — measured 118 B/file vs
227). Eliminates three heap objects per file (CacheEntry, Timestamp, digest
slice) and 60% of cache bytes: 372MB -> ~193MB.

Cost/risk: medium — the cache type threads through `core.Scan`,
`Transition`, and the endpoint's save loop, so it's a mechanical but wide
refactor. Digest width: xxh128 is 16B, but SHA-1/SHA-256 configs need up
to 32B — use a fixed `[32]byte` + length byte (still wins: 16 extra inline
bytes vs 40+ per-allocation overhead). No wire/disk format change.

Validation: `TestCacheFootprint` numbers above; scan correctness covered by
existing tests; benchmark accelerated-scan latency to confirm value-copy
cost is negligible.

### 3. Drop `lastSnapshotBytes`, re-marshal on demand — save ~144MB

Mechanism: marshaling is already deterministic on both sides, so retain the
`*core.Snapshot` pointer (which, after intervention 1, aliases the shared
tree — near-zero marginal bytes) and regenerate baseline bytes at the start
of the next `Scan` call. Even in the worst case, a baseline byte mismatch
only degrades rsync delta efficiency for one transmission — correctness is
signature-based — and deterministic marshaling makes the bytes identical in
practice. CPU cost: one ~60MB marshal per sync cycle per big session,
only when changes occur. Fallback flavor if re-marshal is unpalatable:
zstd-compress the retained bytes (~3x on snapshot protos -> save ~95MB).

Cost/risk: low; ~30 lines in `endpoint/remote/client.go`.

Validation: sync a change end-to-end, check the client debug log's
"Snapshot delta yielded N bytes" stays small (delta efficiency preserved).

### 4. Configure zstd streams — save ~85-90MB

Mechanism: in `sspl/pkg/compression/zstd/zstd.go`, pass
`WithEncoderConcurrency(1)`, `WithWindowSize(1MiB)` to `NewWriter` and
`WithDecoderConcurrency(1)`, `WithDecoderLowmem(true)` to `NewReader`.
~100MB -> ~10MB across 4 sessions. Transport streams are per-session and
long-lived, so this is retained memory, not churn. Tradeoff: modest
compression-ratio loss for >1MB match distances during large stagings;
sync payloads are dominated by rsync-delta'd file content where an 8MB
window buys little. The remote agent gets the same savings for free.

Cost/risk: trivial. Validate with a large-file sync + before/after
compression-ratio comparison from transfer logs.

## What I'd do first

Order by ROI: **#4 and #3 first** (an afternoon combined, ~230MB, near-zero
risk), then **#1** (the structural fix — biggest single win and it also
removes the per-cycle deep-copy churn once paired with COW `Apply`), then
**#2**. Combined estimate: ~900-1130MB off a 1854MB steady state ->
~700-950MB live heap, which at default GOGC puts RSS well under the current
2GiB GOMEMLIMIT without relying on it.

Not pursued but noted: the two monorepo sessions sync near-identical
content from two local checkouts, so a *global* content-addressed interner
could halve the remaining tree memory again (~150-300MB); it needs
cross-session lifecycle management (weak refs or epoch rebuilds) and only
pays off after #1, so it's an extension, not a starting point.

## Surprises

- The rsync baseline (`lastSnapshotBytes`) duplicates a tree the daemon
  already holds deserialized, and the deterministic-marshaling precondition
  for eliminating it is already met on both sides.
- `core.Apply` deep-copies the entire ancestor tree for any non-empty
  change list — one changed file in the monorepo session costs a 163MB
  allocation burst.
- Default `zstd.NewWriter` (GOMAXPROCS concurrency, 8MB window) retains
  ~25MB per idle SSH session.
- "Portable 10-second poll watcher" is actually FSEvents recursive watching
  on darwin; the 10s poll never runs.
