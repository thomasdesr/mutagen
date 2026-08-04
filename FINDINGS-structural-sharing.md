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

## Files

- `pkg/synchronization/core/intern.go` — new: `Interner`, weak table, sweep,
  hashing, `canonicalEqual`
- `pkg/synchronization/core/intern_test.go` — new: correctness, per-field
  discrimination, eviction, concurrency, retention
- `pkg/synchronization/core/intern_bench_test.go` — new: decode/intern/diff
  benchmarks
- `pkg/synchronization/core/testing_synthetic_test.go` — new: synthetic tree
  + retained-heap harness
- `pkg/synchronization/core/diff.go` — pointer-equality fast path (pays off
  without interning: the accelerated scanner and `Apply` already produce
  identical-pointer subtrees)

`go test ./pkg/synchronization/core/...` passes, including under `-race`.
