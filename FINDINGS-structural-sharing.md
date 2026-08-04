# Findings: structural sharing of Entry subtrees

## Immutability audit: CLEAR

The contract holds, and mutagen already relies on subtree pointer-sharing in
production, so interning adds no new class of hazard.

Nine non-test sites assign to a field of an existing `*Entry`. Every one
mutates a tree the enclosing function itself just allocated:

| Site | Why safe |
|---|---|
| `apply.go:51,54,56` | `parent` descends from `result` = `base.Copy(true)` (`apply.go:23`) |
| `transition.go:456,468` | callers pass nodes of `entryCopy` = `Copy(true)` (`:526`) |
| `transition.go:831,867,873,879` | `created` = fresh `target.Copy(false)` (`:807`) |
| `executability.go:68,83,109` | `target` is a node of `result` = `Copy(true)` (`:126`) |

`Copy` **aliases `Digest`** rather than cloning — safe only because nothing
writes through a digest slice (`scan.go:222` uses `Sum(nil)`; no
`copy(entry.Digest, …)` exists anywhere).

Existing sharing: `scan.go:503` splices previous-snapshot subtrees into new
snapshots; `scan.go:756` returns the previous snapshot itself; `entry.go:383`
(`synchronizable()`) builds alias-holding parents; `diff.go`/`reconcile.go`
embed raw subtree pointers in `Change`; `apply.go:12-20` returns `base` by
pointer.

Verified separately because cross-session sharing is new: concurrent
`proto.Marshal` + traversal of one shared tree from 8 goroutines is
race-clean.

**Two risky sites — for cost, not correctness:** `PropagateExecutability`
(`executability.go:126`) and `Apply` (`apply.go:23`) each deep-copy an entire
tree every cycle, destroying all sharing.

## Measured: baseline vs interned

162,500-entry synthetic monorepo tree; baseline calibrates at **200 B and
~3.0 heap objects per entry**, which scales to ~756 MB across six 630k-entry
copies — consistent with the ~850 MB attribution in `MEMORY-EFFICIENCY.md`.

| Copies | Baseline | Table kept | Table dropped |
|---|---|---|---|
| 1 | 31.1 MB / 490k obj | 38.7 MB (**0.80x**) | 27.8 MB (1.12x) |
| 3 | 93.1 MB / 1,470k | 38.7 MB (2.40x) | 27.8 MB (3.35x) |
| 6 | 186.2 MB / 2,941k | 38.7 MB (**4.81x**) | 27.8 MB (**6.70x**) |

Baseline is exactly linear (zero sharing today); interned retention is
**flat**. Table cost: **10.9 MB / 124,950 canonical nodes ≈ 91 B per
canonical node**. At one copy the persistent table is a 25% *regression* —
no real session has one tree, but it bounds the cost honestly. The 23.1%
intra-tree collapse is mostly an artifact of the 25% duplicate-digest input
setting — treat as assumption, not finding.

## Pass cost (21,667-entry tree; noisy laptop, median of 5×50)

| Benchmark | Per tree | Per entry |
|---|---|---|
| Decode | 9.04 ms | 417 ns |
| Intern cold | 9.06 ms | 418 ns |
| Intern warm | 7.99 ms | 369 ns |
| Diff identical, unshared | 8.29 ms | 383 ns |
| **Diff identical, shared** | **15.8 ns** | — |

Interning costs ~one protobuf decode per tree. Profiling attributes it to map
operations (inherent to map-of-message), not hashing — replacing
`maphash.Hash` per node with direct mixing roughly halved the warm pass. Diff
of identical shared trees is ~500,000x faster.

## Key design decisions

- **Hash-then-verify, bottom-up.** Children canonicalized first, so the
  parent hash is a function of child *hashes* and `canonicalEqual` compares
  children by **pointer** — exact, O(children). Correctness never rests on
  hash quality.
- **Pointers are never hashed** — that would depend on Go's non-moving GC,
  which isn't guaranteed.
- **Weak values + sweep, not `runtime.AddCleanup`.** `AddCleanup` was
  implemented first and rejected: the closure captures the `Interner`, so
  every interned node keeps the whole table alive. Shipped design holds
  `weak.Pointer[Entry]`, reclaims opportunistically on lookup plus a full
  sweep on an insertion schedule. `TestInternEvictsCollectedSubtrees` caught
  the gap that an idle table never sweeps — closed by an exported `Sweep()`
  the daemon calls on session termination (positive signal, not inferred from
  silence).
- **In-place rewriting** makes the warm pass allocation-free, at the price of
  an ownership precondition.

## Blockers / risks

1. **Sharing dies every cycle** to the two deep copies. Interning must re-run
   downstream of both, on every tree, every cycle — ~230 ms per 630k tree.
   This is the main threat to the "no cycle-latency regression" goal.
   Highest-value follow-up: convert `Apply` and `PropagateExecutability` to
   path-copying, worth doing on its own merits.
2. **Ownership precondition is unenforced.** `Intern` rewrites content maps
   in place; calling it on a published tree would rewrite nodes another
   goroutine sees. Only a doc comment prevents it. Alternative: build fresh
   maps (one map alloc per directory, precondition gone).
3. **The immutability contract is convention.** Intact today, but upheld by
   nine hand-written copy-first sites. A future violation currently corrupts
   one session; with a daemon-wide table it silently corrupts every sharer.
4. **Lock contention unmeasured** — one mutex per node; 4 concurrent sessions
   not benchmarked. Hash-sharding is the fix if it bites.
5. **Nothing is wired into the daemon.** Hook points identified:
   `endpoint/local/endpoint.go:961`, `endpoint/remote/client.go:343-355`,
   `controller.go:872`, `controller.go:1367`.

Open questions: whether path-copying `Apply`/`PropagateExecutability` lands
first; whether to add `pgregory.net/rapid` (not currently a dep) for the
biconditional property test; whether the production trees actually coincide
as `MEMORY-EFFICIENCY.md` claims — confirmable by logging `Interner.size()`
against summed `Count()` once wired.

## Wiring test contract

Tests for the daemon wiring exist ahead of the wiring. The RED ones are behind
`//go:build wiring_pending` so the branch stays green:

```
go test -tags wiring_pending -race -run Pending ./pkg/synchronization/core/
go test -tags wiring_pending -run Pending ./pkg/synchronization/endpoint/remote/
```

### Pinned (already true)

- `TestInternConcurrentUse` — a shared table, eight goroutines decoding one
  identical serialized snapshot, converging on one root pointer. This is the
  cross-session collapse property; it needed no new test.
- `TestInternDoesNotMutatePublishedTree` (new) — interning the next cycle's
  freshly decoded tree collapses it onto the published tree **and** rewrites no
  edge reachable from that published tree, with a reader walking it throughout.
  This is what makes any wiring site safe against `watchPoll`. Verified
  non-vacuous: with `canonicalize` mutated never to return an existing entry, it
  fails.
- `TestScanRoundTripDecodesSnapshot` (new) — a real `endpointClient` against a
  real `ServeEndpoint` over `net.Pipe`, scanning a temporary root. Nothing is
  stubbed, so the RED tests below inherit a transport that is exercised on every
  untagged run.

### RED (awaiting wiring)

- `TestScanCollapsesRepeatedCyclesPending` — two scans of an unchanged root
  through one client return the same root pointer. Fails: *the second cycle's
  snapshot did not collapse onto the first cycle's*. Satisfied by any table that
  survives across cycles.
- `TestScanCollapsesAcrossSessionsPending` — two client/server pairs over one
  root return the same root pointer. Fails the same way; its `Equal` assertion
  passes first, so the premise (two sessions on one directory really do produce
  deep-equal trees) is confirmed, not assumed. Satisfied only by a table both
  endpoints reach.
- `TestInternIsWriteFreeForAlreadyCanonicalTreesPending` — re-interning an
  already-canonical tree while a reader walks it. Fails under `-race`: write at
  `intern.go:91` (`e.Contents[name] = canonicalChild`) against a read in
  `Entry.Count`.

### Seams the implementer must create

1. **Write-free interning of already-canonical subtrees.** Required by the local
   endpoint site: an accelerated scan splices unmodified subtrees of the
   *published* snapshot into the new one (`scan.go:503`), so the tree reaching
   `Intern` is not exclusively owned however early the hook is placed. Guarding
   the assignment (`if canonicalChild != child`) is sufficient and cheap — the
   spliced nodes' children are canonical whenever the previous snapshot was
   interned, which holds inductively once the site publishes interned trees. Do
   not weaken the ownership doc comment instead; the race is real, not
   theoretical.
2. **An `Interner` reachable at each decode point.** The two remote RED tests
   construct endpoints directly, so they pass only if the table reaches
   `endpointClient.Scan` without being threaded through
   `synchronization.ProtocolHandler.Connect` (`connect.go:17`, implemented by
   `ssh`, `docker`, `local`, and the integration `netpipe` handler). A
   package-level accessor in `core` satisfies them as written. If injection
   through `Connect` is chosen instead, both tests must be rewritten to inject —
   the contract is one daemon-wide table, not the mechanism.
3. **`Sweep()` on session termination.** Already exported and tested
   (`TestInternEvictsCollectedSubtrees`); nothing but a call site is missing.

### Untested seams (need integration coverage)

- **The local endpoint hook** (`endpoint/local/endpoint.go:961`, before
  `e.snapshot = snapshot`). Reaching it needs a watcher and a scan cache, so only
  the mechanism it depends on is covered, by seam 1's RED test.
- **Both controller ancestor sites** (`controller.go:872`, freshly loaded from
  the archive, and `controller.go:1364`, freshly returned by `core.Apply`). Both
  trees are exclusively owned, so they need no new seam, but reaching them needs
  a session archive and a running controller.
- **Cycle-latency cost of re-interning after the `Apply` /
  `PropagateExecutability` deep copies.** Benchmarked per tree above, never
  measured per cycle in the daemon.

## Files

- `pkg/synchronization/core/intern.go` — new: `Interner`, weak table, sweep,
  hashing, `canonicalEqual`
- `pkg/synchronization/core/intern_test.go` — new: correctness, per-field
  discrimination, eviction, concurrency, retention
- `pkg/synchronization/core/intern_bench_test.go` — new: decode/intern/diff
  benchmarks
- `pkg/synchronization/core/testing_synthetic_test.go` — new: synthetic tree
  + retained-heap harness
- `pkg/synchronization/core/intern_wiring_test.go` — new: publish-safety
- `pkg/synchronization/core/intern_published_pending_test.go` — new, RED
- `pkg/synchronization/endpoint/remote/scan_roundtrip_test.go` — new:
  client/server transport harness
- `pkg/synchronization/endpoint/remote/intern_wiring_test.go` — new, RED
- `pkg/synchronization/core/diff.go` — pointer-equality fast path (pays off
  without interning: the accelerated scanner and `Apply` already produce
  identical-pointer subtrees)

`go test ./pkg/synchronization/core/...` passes, including under `-race`.
