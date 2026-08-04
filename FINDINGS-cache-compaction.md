# Cache/IgnoreCache compaction (Path 3)

Prototype of Path 3 from `MEMORY-EFFICIENCY.md`'s ranked list: compacting the
per-endpoint `Cache`/`IgnoreCache`/`lastReturnedScanCache` maps, which today
are keyed by full relative path and duplicate every directory prefix once per
file.

## What changed

Two of the three sub-options from the proposal are implemented, in the
proposal's own order of expected return:

1. **Inline modification time.** `ScanCacheEntry` (the new in-memory entry
   type) stores `modificationTimeSeconds int64` / `modificationTimeNanos
   int32` instead of a heap-allocated `*timestamppb.Timestamp`, comparing via
   `time.Time.Unix()`/`.Nanosecond()` instead of `timestamppb.AsTime().Equal()`.
   One fewer heap object per cached file.
2. **Directory sharding.** `Cache.Entries` (`map[string]*CacheEntry`) becomes
   `ScanCache` (`map[directory]map[name]*ScanCacheEntry`), and `IgnoreCache`
   (`map[IgnoreCacheKey]bool`) becomes `map[directory]map[nameAndDirFlag]bool`.
   Every file in the same directory now shares one copy of the directory
   string instead of each holding its own copy embedded in a flat key.

The third sub-option, basename interning, was evaluated but not implemented —
see below.

**Wire format is untouched.** `Cache` (the proto message) never crosses gRPC
and is the only one of these three structures persisted to disk. `ScanCache`
and the new `IgnoreCache`/`ScanCacheEntry` types are purely in-memory;
conversion happens only at the load/save boundary in
`pkg/synchronization/endpoint/local/endpoint.go`, via `NewScanCacheFromProto`
and `(*ScanCache).Proto()`.

## Measured savings — and a benchmark bug that inverted the first result

The benchmark (`pkg/synchronization/core/cache_memory_bench_test.go`)
generates monorepo-shaped synthetic paths (nested `top/mid/leaf/module`
directories, 60-100 char paths, repeated basenames) and measures retained
heap via `runtime.GC()` + `runtime.ReadMemStats` before/after construction.

The first version of this benchmark (committed as the `c91613d7` baseline,
46.76MB for 150k files) had a bug: its `module` index used a modulus
(`i%500`) out of sync with the `top`/`mid`/`leaf` nesting period (512),
giving an effective combined period of `lcm(512,500)=64,000` -- so 150,000
files landed in 64,000 distinct directories, averaging only ~2.3
files/directory. At that fan-out, sharding is a **net loss**: each of the
64,000 tiny per-directory inner maps pays Go's fixed per-map bucket-array
overhead, which outweighs the string-sharing benefit from so few files per
directory. The post-refactor run of that flawed benchmark showed retained
bytes roughly doubling (90.87MB) -- not a bug in `ScanCache`, but a
benchmark whose synthetic data didn't have the locality the compaction
targets. That original baseline number is **not comparable** to a correctly
directory-local measurement and should be disregarded.

The benchmark was rewritten to nest exactly 8 files (matching the basename
list length) under each directory, and to build both a flat (pre-compaction)
and sharded (post-compaction) cache/ignore-cache pair from the *same*
generated data, for a fair comparison:

| Shape | Retained bytes (150k files) | Bytes/file |
|---|---|---|
| Flat (pre-compaction) | 41,199,680-41,205,272 | 274.7 |
| Sharded (post-compaction) | 13,819,160-13,819,704 | 92.1 |

**-66% at 8 files/directory**, stable across 5 repeated samples (`go test
./pkg/synchronization/core/ -bench 'BenchmarkCacheMemoryFootprint(Flat|Sharded)' -benchtime=1x -count=5`).

## Central finding: sharding's payoff is conditional on directory fan-out

`BenchmarkCacheMemoryFootprintByFanout` sweeps average files/directory at a
fixed total file count (150k), comparing flat vs. sharded at each point:

| Files/directory | Flat bytes/file | Sharded bytes/file | Sharded vs. flat |
|---|---|---|---|
| 1 | 314.3 | 780.1 | +148% (worse) |
| 2 | 290.4 | 407.9 | +40% (worse) |
| 4 | 278.7 | 196.8 | -29% (better) |
| 8 | 274.7 | 92.1 | -66% (better) |

The crossover is between 2 and 4 files/directory. Below it, per-directory map
overhead (bucket array + hmap header, paid once per distinct inner map)
exceeds the savings from no longer duplicating the directory-prefix string
per file. This is the reason the original (buggy) benchmark showed a
regression -- not a flaw in the approach, but a demonstration that the
approach has a real, findable failure mode.

**Open question, not resolved in this session:** whether the actual ~630k-file
target trees clear this threshold. Source monorepos are typically well above
4 files/directory in aggregate, but the distribution matters more than the
mean -- leaf directories with 1-2 files (a common pattern: single-file Go
packages, per-resource Terraform modules, thin wrapper directories) would see
`ScanCache` cost *more* memory than today's flat map for exactly those
subtrees, even while directories with high fan-out save heavily. Before
shipping this, someone should sample the real target tree's per-directory
file-count distribution (not just its mean) against this ~4 files/directory
threshold.

## Basename interning (option c): evaluated, not implemented

Plumbing check: in `pkg/synchronization/core/scan.go`, `contentName` (used to
build the path later split into `ScanCache`'s directory/name key) originates
fresh from `filesystem.Directory.ReadContentNames()`
(`pkg/filesystem/directory_posix.go`) on every scan -- a syscall-backed
allocation, not a shared literal. `MEMORY-EFFICIENCY.md`'s own profiling notes
call out `__init__.py` and `BUILD.bazel` as basenames repeated thousands of
times across a real monorepo. This confirms the scenario the proposal
describes is real, and that interning is pluggable at the same call sites
already touched by this change (`s.newCache.set` / `s.newIgnoreCache.set` in
`scanner.file`/`scanner.directory`), via a `map[string]string` pool consulted
before each `.set` call.

It was not implemented or benchmarked in this session because the synthetic
benchmark can't measure it honestly: its 8 basenames are Go string literals,
which the compiler already deduplicates at compile time, so an
interning-pool experiment against this synthetic data would show a
misleadingly large (or zero, depending on how the experiment is wired)
result that says nothing about production behavior. A trustworthy measurement
needs either a real tree's basename-frequency distribution or a synthetic
generator that forces fresh heap allocations per basename occurrence
(e.g. `strings.Clone`) with a distinct-name cardinality estimated from a real
target tree -- which wasn't available in this session. Order-of-magnitude
reasoning only: interning saves one allocation's backing-array cost per
*duplicate* occurrence (not per occurrence -- the string header is stored in
the map regardless), so the win scales with `(total files - distinct
basenames) x (average basename length, rounded to a malloc size class)` and
is bounded well below the 150-300MB the proposal estimates for all of Path 3
combined, most of which this session's sharding work already captures.

**Recommendation:** worth prototyping only after (1) the fan-out question
above is resolved (since interning stacks on top of sharding, not on the flat
map), and (2) real basename cardinality is sampled from a target tree to
replace the order-of-magnitude estimate with a real one.

## Test status

- `go vet ./pkg/... ./tools/scan_bench/...`: clean.
- `goimports -l` on every touched/new file: clean.
- `go test ./pkg/synchronization/core/...`: all 76 tests pass.
- `go test ./pkg/synchronization/endpoint/local/...`: fails to *link*
  (`ld: library not found for -lresolv`), confirmed via `go vet` (passes,
  i.e. the package type-checks) and a controlled `git stash`/`git stash pop`
  A/B test showing the identical link failure on the unmodified tree. This is
  a pre-existing environment limitation (nix-provided clang/cctools toolchain
  missing a resolver library on this darwin/arm64 sandbox), not a regression
  from this change.
- Full binary (`go build ./...`) was not attempted, per task scope; the same
  `-lresolv` issue affects `cmd/mutagen`, `cmd/mutagen-agent`, and
  `tools/watch_demo` independent of this change.

## Risks

- **Fan-out regression risk** (see above): any real subtree with <=2-3
  files/directory gets measurably worse under this change. Not a correctness
  risk, but a memory-regression risk for a specific, plausible directory
  shape.
- `ScanCache`'s outer map is now sized by directory count instead of entry
  count (`NewScanCacheFromProto`, `Scan`'s `newCache` allocation) -- this is a
  one-time estimate at load/scan-start, not itself a source of ongoing
  overhead, but it's a second place (besides the sharding threshold above)
  where the change's benefit depends on the real tree's shape rather than
  being unconditional.
- No behavior change to persisted state or the sync protocol; risk is
  confined to in-memory footprint and CPU cost of the extra map layer
  (not measured in this session -- only retained-heap was benchmarked, not
  scan-loop CPU/latency, which `MEMORY-EFFICIENCY.md`'s success metric also
  requires "no regression in cycle latency").

## Files changed

- `pkg/synchronization/core/scan_cache.go` (new) -- `ScanCache`/`ScanCacheEntry` types, proto conversion, nil-safe accessors.
- `pkg/synchronization/core/ignore.go` -- `IgnoreCache` reshaped to shard by directory.
- `pkg/synchronization/core/path.go` -- `splitCachePath` helper (allocation-free directory/name split).
- `pkg/synchronization/core/cache.go` -- removed methods migrated to `scan_cache.go` (`Equal`, `GenerateReverseLookupMap`), now operating on `*ScanCache`.
- `pkg/synchronization/core/scan.go`, `transition.go` -- updated to the new types.
- `pkg/synchronization/core/scan_test.go` -- updated call sites for the renamed `IgnoreCache` helper methods.
- `pkg/synchronization/core/cache_memory_bench_test.go` -- corrected synthetic generator, added flat-vs-sharded and fan-out-sweep benchmarks.
- `pkg/synchronization/endpoint/local/endpoint.go` -- converts `Cache` <-> `ScanCache` at the disk load/save boundary.
- `tools/scan_bench/main.go` -- adapted to the new API (`.Len()`, `.IntersectionEqual()`, `.Proto()`).
