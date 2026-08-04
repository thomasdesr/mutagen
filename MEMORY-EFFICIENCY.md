# Memory efficiency investigation

Working notes for the `memory-efficiency` branch. Baseline measured against a
live daemon (v0.17.5 + the pprof patch on this branch) syncing four sessions —
two full monorepo trees (~630k files each, and both sessions share the same
local directory) and two worktree-parent trees (~193k files each) to two
remote hosts.

## Measured baseline

- ~1.9GB live heap / 20.2M live objects at steady state (GOMEMLIMIT=2GiB).
- ~92 bytes per object on average — this is a small-object problem, and GC
  scan cost scales with the pointer count, not just bytes.
- Attribution (inuse_space / inuse_objects):
  - ~850MB, 13.3M objects: protobuf-decoded long-lived state — `Entry` trees,
    `Cache` entries, per-entry `Timestamp` messages, digest byte slices
    (`reflect.unsafe_New` 6.3M objects, `consumeBytesNoZero` 4.5M,
    `consumeStringValidateUTF8` 2.5M).
  - ~720MB: scanner-built snapshots and caches (`scanner.directory`,
    `scanner.file`, plus 1.9M retained strings originating in `os.readdir`).
  - ~350MB: tree-walk copies, serialization buffers, zstd windows.

## Why the state is this large

Per synced file, the daemon retains several parallel structures:

1. `Entry` nodes in up to three trees per session (ancestor, alpha snapshot,
   beta snapshot), each a live protobuf message: message header overhead, a
   separately heap-allocated 16-byte xxh128 digest slice, and membership in the
   parent directory's `map[string]*Entry` (string key + bucket overhead).
2. A `Cache` entry keyed by **full relative path** with a heap-allocated
   `*Timestamp` message and another digest slice.
3. An `IgnoreCache` entry, also keyed by full path.

Nothing is shared: not between the three trees of one session (identical at
steady state), not between sessions syncing the same local directory (we have
two), not between the cache/tree digest copies of the same file, and not
between the thousands of repeated name strings (`__init__.py`, `BUILD.bazel`).
`Entry` is documented immutable ("must not be modified"), which is exactly the
contract structural sharing needs — the immutability is currently paid for but
never exploited.

Upstream `master`/`v019-development` contain no overlapping work in
`pkg/synchronization/core` (proto regens, Go version bump, ignore semantics).

> **2026-08-04:** The ranking below has been tested. See "Measured results and
> revised plan" at the end of this document — it supersedes the estimates here.

## Proposed paths, ranked by expected value

### 1. Digest interning / inlining (highest certainty)

4.5M live objects are fixed-size 16-byte digests, each carrying slice-header
and allocation overhead, duplicated across ancestor/alpha/beta trees and the
cache for every unchanged file. Intern them after decode and after scan (the
same content digest appears 4–6 times today), or go further and store them
inline as fixed arrays in a non-generated in-memory type. No wire-format
change; upstreamable. Rough estimate: 200–300MB plus a large GC-pointer
reduction.

### 2. Structural sharing of identical subtrees (largest win)

Content-address `Entry` subtrees and intern them daemon-wide: at decode time,
after each scan, and after each transition, identical subtrees collapse to one
shared node. At steady state ancestor == alpha == beta within a session, and
our two monorepo sessions share the alpha tree entirely — roughly six copies
of the same ~630k-entry tree collapse toward one, and diffs gain a
pointer-equality fast path. Requires a weak-valued intern table (trees are
acyclic; eviction on session termination) and relies on the existing
immutability contract. Rough estimate: 50–70% of tree memory at steady state.

### 3. Cache and IgnoreCache compaction

Three full-path-keyed flat maps per endpoint duplicate every directory prefix
in every key. Options, roughly in order of return: replace the per-entry
`*Timestamp` message with inline integers in an in-memory type (one fewer
heap object per file); shard the maps by directory so keys are basenames and
directory prefixes are stored once (the trie idea, applied where the flat keys
actually live — `Entry` trees are already trie-shaped); intern basename
strings against the same pool the `Entry` map keys use, so `readdir` output
stops being retained N times. Rough estimate: 150–300MB.

### 4. Protobuf codegen modernization (enabler, not a direct win)

vtprotobuf-style generated marshal/unmarshal cuts decode CPU and transient
garbage, but the resident structs stay the same size — as a standalone change
it does not reduce steady-state memory. Its value here is as an enabler for
paths 1–2: a custom decode path can intern digests, names, and subtrees
*during* unmarshal, so duplicates are never allocated (lower peaks, which is
what GOMEMLIMIT pressure responds to), instead of allocate-then-intern.

### 5. Arena/compact snapshot representation (endgame, invasive)

Pack each snapshot into node arrays with index references, a shared string
pool, and inline digests: per-node allocation disappears, GC scan cost
approaches zero, and total size drops several-fold. Every core algorithm
(diff, reconcile, transition) currently traverses `Contents` maps on the
generated proto type, so this means either an adapter layer or a rewrite of
`core` — worth it only if paths 1–3 fall short. Not realistically
upstreamable.

## Measurement harness

- `tools/scan_bench` (in-repo) measures scan memory/CPU over a real tree —
  use the monorepo checkout as the corpus for A/B runs.
- Live A/B: the nix overlay builds this branch; the daemon exposes pprof via
  `MUTAGEN_PPROF_ADDR`. Baseline profiles are saved from 2026-08-03/04.
- Success metric: steady-state live heap for the current four-session setup,
  target under ~700MB (from 1.9GB), with no regression in cycle latency.

## Sequencing (superseded — see below)

1 → 2 → 3 in separate commits, measuring after each; 4 folded in where it
makes 1–2 cheaper at peak; 5 only on evidence that 1–3 plateau above target.

## Measured results and revised plan (2026-08-04)

Four parallel investigations: three prototype branches plus an independent,
unanchored attribution pass (`explore/independent`, done without reading this
document — it converged on the same two dominant structures and found three
things this document missed).

### What each track measured

**Structural sharing (`me/structural-sharing`) — confirmed, the main event.**
Six identical decoded copies of a 162k-entry tree: 186MB baseline → 27.8MB
interned (6.7×), flat in the number of copies. Immutability audit CLEAR — all
nine Entry-mutation sites write only to freshly-copied trees, and mutagen
already pointer-shares subtrees in production (scanner baseline splicing).
Intern pass costs about one proto-decode per tree; diff of shared identical
trees drops from 8.3ms to 16ns via a pointer-equality fast path (committed;
pays off even without interning). Not yet wired into the daemon.

**Digest interning (`me/digest-interning`) — demoted, estimate corrected.**
This document's 200–300MB estimate was wrong: digest bytes total only ~72MB
of heap; interning's honest ceiling is ~24MB / 1.5M objects (~7% of live
objects, ~1.3% of bytes). Prototype works and is measured to ±2 objects, but
ships only if the GC-pressure win justifies it; digest *inlining* (part of a
compact representation) is worth ~2× interning and subsumes it. Critical side
finding: `endpoint.watchPoll` reads the published snapshot after releasing
`scanLock`, and the scanner splices published subtrees into new snapshots —
so no pass may rewrite trees it doesn't exclusively own. This constrains
where the subtree interner can hook in (producer-owned sites only).

**Cache compaction (`me/cache-compaction`) — confirmed, threshold resolved.**
Directory-sharded maps + inline mod-times: −66% bytes/file at fan-out 8,
regression below fan-out ~4. The real 630k-file tree has mean fan-out 8.7,
and the 52% of directories below the threshold hold only 13% of files —
clear aggregate win on the cache family (~430MB of heap per the independent
attribution). Wire format unchanged.

**Independent attribution (`explore/independent`) — three new needles.**
(1) `lastSnapshotBytes`: each remote endpoint client retains the serialized
snapshot as an rsync baseline — ~144MB of pure redundancy, regenerable on
demand because both sides already marshal deterministically. (2) zstd stream
state on idle connections: ~85–90MB, fixable with encoder-concurrency /
window-size options. (3) `core.Apply` deep-copies the entire ancestor for any
non-empty change list — a ~163MB allocation burst per changed file per
monorepo session, independently flagged by the sharing track
(`PropagateExecutability` too), and the explanation for RSS peaks well above
live heap.

### Revised sequence

1. **Trivial wins (~230MB, no structural risk):** drop `lastSnapshotBytes`
   (regenerate on demand), configure zstd streams. Analysis-only so far — 
   needs implementation.
2. **Path-copying `Apply` and `PropagateExecutability`:** kills the per-cycle
   full-tree copy bursts on its own merits, and is the precondition for
   sharing to survive across cycles.
3. **Wire the subtree interner** at producer-owned hook points (respecting
   the watchPoll ownership constraint), with `Interner.size()` telemetry to
   confirm the production trees actually coincide. Expected: the dominant
   share of the ~1.28GB tree memory.
4. **Cache compaction** (`me/cache-compaction` merge).
5. **Live A/B after each step** via the nix overlay + pprof + GOMEMLIMIT
   harness; the digest branch's findings file contains the peak-vs-limit
   experiment design.

Integration note: `me/digest-interning` and `me/structural-sharing` both
added `pkg/synchronization/core/intern.go` and overlapping bench harnesses —
they conflict textually and the digest one is likely not merged anyway; mine
its findings, not its diff.

Revised end-state estimate: ~1.9GB → 600–900MB steady state, with cycle-peak
allocation reduced by path-copying rather than increased by interning passes.
