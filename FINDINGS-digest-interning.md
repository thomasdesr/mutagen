# Digest interning (Path 1)

Findings for `me/digest-interning`, branched from `memory-efficiency`. Path 1 of
`MEMORY-EFFICIENCY.md`: deduplicate the 16-byte file digests that
`consumeBytesNoZero` allocates 4.5M of in the live daemon.

Headline: interning works exactly as predicted mechanically, but the ceiling is
far below the 200-300MB the proposal estimated. Digest **bytes** account for only
~72MB of the 1.9GB live heap, so perfect deduplication can free at most ~56MB of
it, and what the wiring can safely reach is ~24MB / ~1.5M live objects. The
proposal's figure requires eliminating the 24-byte slice header per file entry
too, which means inlining digests as fixed arrays in a non-generated type.
Interning is still worth landing -- object count is what GC scan cost tracks, and
there's no wire format change -- but it is a single-digit-percent win, not the
path to the 700MB target.

## Measurement harness

`pkg/synchronization/core/intern_bench_test.go`:

- `buildSyntheticTree` generates a monorepo-shaped hierarchy: 150k files, 15,822
  directories, nesting 3-8 deep, basenames drawn from a repeated pool
  (`BUILD.bazel`, `__init__.py`, ...), 16-byte digests from a deterministic
  SplitMix64, and roughly a tenth of files sharing content with one of 64 shared
  digests. The corpus has 135,194 distinct digests across 150,000 files.
- `BenchmarkSnapshotRetention` decodes three independent copies of the tree plus
  a `Cache` covering it -- the ancestor/alpha/beta/cache shape of one session --
  and measures **retained** heap as `runtime.ReadMemStats` deltas across forced
  GCs. `testing.B`'s allocation counters measure churn, so they can't see this.
  Run it with `-benchtime=1x`; each iteration measures and then drops its own
  state, so iteration count doesn't multiply peak memory.
- `BenchmarkInternEntries` measures one interning pass, i.e. the per-cycle cost
  interning adds.

Retention numbers are reproducible to within a couple of objects across runs.

## Baseline and measured savings

`go test ./pkg/synchronization/core/ -run '^$' -bench BenchmarkSnapshotRetention -benchtime=1x`
on an M4 (darwin/arm64, Go 1.26.3):

| Intern table                  | MB retained | Live objects | B/file | vs baseline            |
|-------------------------------|-------------|--------------|--------|------------------------|
| none (baseline)               | 136.5       | 2,095,558    | 954.1  | --                     |
| fresh per decoded message     | 135.6       | 2,036,335    | 947.8  | -0.9MB, -2.8% objects  |
| one per session's decode pass | 129.4       | 1,630,754    | 904.5  | -7.1MB, -22.2% objects |

The object delta is exactly the arithmetic: 600,000 digest allocations (4 per
file) collapse to 135,194 (one per distinct digest), a reduction of 464,806
objects, and 464,806 x 16B = 7.4MB against 7.1MB measured. A per-message table
only catches intra-tree duplicates (14,806 per tree x 4 messages = 59,224
objects), which is why cross-tree scope is where the win lives.

Component sizes, for extrapolation: `Entry` is 120 bytes (128-byte size class),
`CacheEntry` 96, `timestamppb.Timestamp` 56. Digest storage is 4 x 16 = 64 of
the 954 bytes retained per file, so 6.7% of the total, and interning removes
three quarters of it.

Interning pass cost (`BenchmarkInternEntries`, 150k files):

| Table capacity hint | ns/op  | B/op       | allocs/op |
|---------------------|--------|------------|-----------|
| none                | 47-60M | 27,343,608 | 136,237   |
| exact               | 24-27M | 14,756,512 | 135,707   |

Table growth dominates, so callers pass a capacity hint. Scaled to a 630k-file
tree: ~110ms and ~62MB transient per pass, hinted. The remote client does two
passes per cycle (seed, then intern), so ~220ms per cycle for a monorepo tree,
against cycles that already take seconds. The allocation count is the 16-byte
map keys; an array-keyed table (see Open questions) would eliminate them.

## Approach

`pkg/synchronization/core/intern.go` adds `DigestInterner`: a
`map[string][]byte` from digest content to the backing array chosen for it, with
`Intern`, `InternEntries`, `InternCache`, `SeedFromEntries`, and `Len`.

Two design decisions carry the weight.

**Tables are per-pass, never long-lived.** The sharing a pass establishes
outlives the table that established it, so a persistent table buys nothing and
costs more than the duplicates it removes: a `map[string][]byte` entry costs
roughly 68 bytes per distinct digest (16-byte string header + 24-byte slice
header + bucket overhead + the key's own 16-byte allocation) while each
duplicate eliminated saves 16. A daemon-wide table would be a net loss on bytes
at any plausible duplicate factor.

**Interning happens in the producer, on a hierarchy the producer exclusively
owns.** Rewriting `Entry.Digest` replaces a reference with one to equal content,
which is invisible to every reader -- but it is still a write, and entries are
shared far more aggressively than the "immutable" contract suggests:

- `scanner.directory` splices unmodified baseline subtrees straight into a new
  snapshot (`contents[contentName] = directoryBaseline` in `scan.go`), so a new
  snapshot contains entries from the previously published one.
- `endpoint.watchPoll` copies `e.snapshot`, **releases `scanLock`**, and then
  calls `snapshot.Equal(previous)` and `core.Diff(...)`, both of which read
  `Digest`, concurrently with the controller's use of the same tree.

So a single controller-level "intern ancestor, alpha, and beta together" pass --
the shape that would have collapsed everything onto one array set -- is a data
race against that watcher goroutine. Instead each producer interns its own
output before publishing it, and `SeedFromEntries` provides the cross-tree
effect by *reading* a tree the caller doesn't own to seed the table:

| Site | What it interns | Table sizing |
|------|-----------------|--------------|
| `core.scanner.file` | each digest as the entry is created (compute and cache-hit paths) | `len(cache.Entries)` |
| `remote.endpointClient.Scan` | freshly decoded snapshot, seeded from the ancestor | previous cycle's `Len()` |
| `synchronization.controller.synchronize` | ancestor, right after archive load | `ancestor.Count()` |
| `local.NewEndpoint` | cache, right after load from disk | `len(cache.Entries)` |

The remote client is the highest-value site: it decodes a full tree every cycle,
and at steady state nearly every digest in it duplicates one the ancestor
already holds, so seeding from the ancestor removes a whole copy of the
session's digests per cycle -- both retained bytes and churn.

## Projection to the live daemon

Count the live digest arrays per file per session in the daemon's actual
configuration (alpha local, beta remote):

- the scan cache holds one, and the alpha tree aliases it, because the scanner
  already assigns `cached.Digest` straight into the entry on a cache hit;
- the ancestor holds one, established at archive load and preserved across
  cycles because `Apply` deep-copies entries while copying digest *references*;
- the decoded beta snapshot holds one, freshly allocated every cycle.

Three per file, consistent with the profile's 4.5M objects over ~1.65M files
(2.7, allowing for files absent from some trees). This change removes the third:
beta adopts the ancestor's arrays. Projection: **~1.5M fewer live objects (~7% of
the 20.2M in the profile) and ~24MB less live heap (~1.3% of 1.9GB)**, plus
whatever intra-cache duplicate content collapses at load time (a tenth of files
in the synthetic corpus; unmeasured in the real trees).

The benchmark's -22% object result is the mechanism's ceiling -- four independent
decodes collapsing onto one array set -- not what the wiring achieves. The wiring
is bounded by the two array sets it can't safely merge.

Closing that gap means making the cache and the ancestor share, which needs one
of:

- interning the cache against the ancestor inside `endpoint.Scan` under
  `scanLock`. The local endpoint currently discards its ancestor argument
  (`func (e *endpoint) Scan(ctx context.Context, _ *core.Entry, full bool)`), so
  it would have to start using it, and rewriting already-published `CacheEntry`
  objects would rest on `Stage`/`Transition` never running concurrently with
  `Scan` -- a subtle invariant to hang a mutation on.
- inlining digests, which removes the question entirely.

Even at that ceiling interning tops out near 45MB of 1.9GB, not the proposal's
200-300MB. That figure is only reachable by removing the slice header as well:
4.5M x 24B = 108MB of headers, on top of ~56MB of duplicate digest bytes. Inlining digests as `[16]byte` (or a length-tagged fixed
array) in a non-generated in-memory `Entry` collects both, and makes interning
redundant for equal-content files that aren't equal *entries*. It also means the
core algorithms stop traversing generated protobuf types, which is the Path 5
rewrite. Interning is the cheap, upstreamable, no-wire-change subset; it does
not substitute for Path 2 (subtree sharing), which subsumes it entirely at
steady state.

## Risks

1. **In-place rewriting.** The `InternEntries`/`InternCache` doc comments state
   the exclusive-ownership requirement, but nothing enforces it. A future caller
   that interns a published snapshot introduces a silent, rare data race on a
   slice header. If that risk isn't acceptable, the alternative is interning
   during a custom unmarshal (Path 4), where the tree provably isn't shared yet.
2. **Cache entry reuse never converges in-process.** The scanner reuses
   `CacheEntry` objects from the previous cache verbatim, and it must not
   rewrite them (the cache-saving goroutine marshals them). So cache-hit digests
   keep whichever array they were first decoded with; duplicate content in the
   cache only collapses when the cache is loaded fresh from disk. The
   consequence is that the alpha-side saving arrives one daemon restart late.
3. **Transient allocation lands at the cycle peak.** Interning adds ~62MB of
   garbage per pass for a 630k-file tree, at the same moment the decoded
   snapshot, rsync buffers, and zstd windows are live. Under `GOMEMLIMIT=2GiB`
   pressure the extra peak could cost more GC work than the retained savings
   buy back. This is the one measurement the benchmark can't answer.
4. **Digest length mixing** is safe: the table key is the digest content as a
   string, so a 16-byte digest and a 32-byte digest sharing a prefix cannot
   collide (covered by `TestDigestInternerDistinguishesDigestLengths`).

## Verification

- `go test ./pkg/synchronization/core/...` passes.
- `CGO_ENABLED=0 go test ./pkg/synchronization/...` passes in full.
- `CGO_ENABLED=0 go test ./pkg/...` passes except `pkg/agent` (needs a built
  agent bundle under `build/`) and `pkg/daemon` + `pkg/integration` (need the
  daemon lock, held by the running daemon). Both are pre-existing and unrelated.
- `CGO_ENABLED=0` is required on this machine: the nix clang wrapper's SDK has
  no `libresolv`, so linking any cgo test binary fails. Unrelated to this work.
- The interner tests were verified to fail when `Intern` is mutated into a no-op.

## Open questions

1. **Does the beta-from-ancestor seeding hold at steady state?** It relies on the
   ancestor's digests being the same arrays the previous cycle's snapshots used,
   which follows from `Apply` copying digest references rather than bytes, but
   only a live profile confirms it.
2. **Is the per-cycle transient cost acceptable** under `GOMEMLIMIT` pressure?
   See the live A/B below.
3. **Array-keyed tables.** `cache_maps.go` already has the pattern
   (`map[[20]byte]string`, `map[[32]byte]string`) for digest-keyed maps. A
   length-tagged fixed-array key would cut the 135k transient allocations per
   pass to nearly zero and speed up hashing, at the cost of a size dispatch.
   Worth doing only if the transient cost turns out to matter.
4. **Cross-session sharing.** Two sessions synchronizing the same local
   directory still keep entirely separate digest arrays. Sharing them needs a
   daemon-wide table with weak references (`weak.Pointer` plus
   `runtime.AddCleanup`, Go 1.24+); `go.mod` currently declares `go 1.19`, so
   this needs a language version bump as well as eviction design.
5. **Should `Entry.Copy` intern?** It shares digest references already, so there
   is nothing to gain; noted only because the task listed it as a candidate.

## Live A/B experiment (not run -- needs coordination)

Per the working constraints, this wasn't executed against the running daemon.

1. Build this branch through the nix overlay and restart the daemon with
   `MUTAGEN_PPROF_ADDR` set and `GOMEMLIMIT=2GiB`, with the same four sessions
   (two ~630k-file monorepo trees sharing one local directory, two ~193k-file
   worktree-parent trees).
2. Wait for all four sessions to report `Watching` and to have completed at
   least two full cycles each, then let it settle ~10 minutes so the ancestor,
   alpha, and beta trees are all at steady state.
3. `curl -s http://localhost:6060/debug/pprof/heap > heap-interned.pb.gz`.
4. Compare against the saved baseline:
   `go tool pprof -inuse_objects -top -diff_base heap-post.pb.gz heap-interned.pb.gz`
   and the same with `-inuse_space`.
5. Read cycle durations for ~10 cycles per session from debug-level daemon logs,
   before and after, to confirm no latency regression.

Expected: `consumeBytesNoZero` live objects fall from ~4.5M to ~3.0M, total live
heap drops 20-30MB, cycle duration unchanged within noise.

Falsification: if `consumeBytesNoZero` objects drop by less than ~1M, the
beta-from-ancestor seeding isn't taking effect -- check that the beta endpoints
are remote (so the snapshot really is decoded in the daemon) and that the
ancestor is non-empty at the time of the receive. A drop much larger than 1.5M
means the baseline had more than three copies per file, and the extra copies
should be identified before trusting the number.
