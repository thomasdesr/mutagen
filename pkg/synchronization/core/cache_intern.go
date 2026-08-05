package core

import (
	"bytes"
	"hash/maphash"
	"sync"
	"weak"
)

// CacheInterner collapses identical per-directory cache shards onto a single
// shared instance, for both ScanCache and IgnoreCache. It exists for the same
// reason as Interner: caches are immutable once published, so two shards with
// identical content are interchangeable and only one copy needs to exist.
//
// The daemon duplicates cache shards two ways at steady state: endpoints
// watching the same root each build identical caches from their own scans,
// and each endpoint holds up to two cache generations (its current cache and
// the one aliased by lastReturnedScanCache) that are rebuilt from scratch
// every scan and so share nothing structurally even where their content is
// identical. Interning shards against a shared CacheInterner collapses both.
//
// Shards are held weakly, exactly as Interner holds subtrees: nothing in the
// table keeps a shard alive, a shard's record disappears after its last
// referencing cache is collected, and callers signal end-of-life by sweeping.
// Canonical shards carry their content hash and a canonical marker, so
// re-introducing an already-canonical shard (the common case for unchanged
// directories under generational churn) is a constant-time pointer check
// rather than a re-hash.
//
// A CacheInterner is safe for concurrent use and its zero value is ready to
// use.
type CacheInterner struct {
	// shards partition the table by content hash. Locking is per shard and
	// acquired once per cache shard, so concurrent endpoints interleave
	// across locks instead of serializing behind one mutex.
	shards [internShardCount]cacheInternerShard
}

// cacheInternerShard is one lock-partitioned portion of a CacheInterner's
// table.
type cacheInternerShard struct {
	// lock guards the shard.
	lock sync.Mutex
	// scanBuckets maps shard content hash to the canonical ScanCache shards
	// carrying that hash. ScanCache and IgnoreCache shards are never
	// interchangeable, so they get separate hash spaces to keep bucket scans
	// from walking candidates they can't match.
	scanBuckets map[uint64][]weak.Pointer[scanCacheShard]
	// ignoreBuckets is the IgnoreCache equivalent of scanBuckets.
	ignoreBuckets map[uint64][]weak.Pointer[ignoreCacheShard]
	// insertionsSinceSweep counts insertions since the shard's last sweep.
	insertionsSinceSweep int
}

// sharedCacheInterner backs SharedCacheInterner.
var sharedCacheInterner CacheInterner

// SharedCacheInterner returns the process-wide cache interning table. Every
// site that constructs a long-lived ScanCache or IgnoreCache interns against
// this single table so that identical shards collapse across endpoints and
// generations, not just within one cache.
func SharedCacheInterner() *CacheInterner {
	return &sharedCacheInterner
}

// InternScanCache canonicalizes every shard of the provided cache against
// the table, rewriting the cache's directory map in place where a shard's
// identity changes, and returns the cache. The caller must have exclusive
// ownership of the cache's outer map; shards that are already canonical are
// recognized in constant time and never written. A nil cache is returned
// unchanged.
func (i *CacheInterner) InternScanCache(cache *ScanCache) *ScanCache {
	if cache == nil {
		return nil
	}
	for directory, shard := range cache.directories {
		if shard.interned {
			continue
		}
		canonical := i.canonicalizeScanShard(shard)
		if canonical != shard {
			cache.directories[directory] = canonical
		}
	}
	return cache
}

// InternIgnoreCache canonicalizes every shard of the provided cache against
// the table, with the same ownership requirements and semantics as
// InternScanCache. A nil cache is returned unchanged.
func (i *CacheInterner) InternIgnoreCache(cache IgnoreCache) IgnoreCache {
	if cache == nil {
		return nil
	}
	for directory, shard := range cache {
		if shard.interned {
			continue
		}
		canonical := i.canonicalizeIgnoreShard(shard)
		if canonical != shard {
			cache[directory] = canonical
		}
	}
	return cache
}

// Sweep drops the table's records of shards that have been collected. Like
// Interner.Sweep, it exists for the signal that insertion-scheduled sweeping
// can't see: a terminated session's caches become unreachable while nothing
// inserts on their behalf again.
func (i *CacheInterner) Sweep() {
	for s := range i.shards {
		shard := &i.shards[s]
		shard.lock.Lock()
		shard.sweep()
		shard.lock.Unlock()
	}
}

// canonicalizeScanShard returns the canonical shard equal to the provided
// one, registering it as canonical if no equal shard is known.
func (i *CacheInterner) canonicalizeScanShard(shard *scanCacheShard) *scanCacheShard {
	hash := scanShardHash(shard)

	tableShard := &i.shards[hash%internShardCount]
	tableShard.lock.Lock()
	defer tableShard.lock.Unlock()

	bucket := tableShard.scanBuckets[hash]
	live := bucket[:0]
	for _, candidate := range bucket {
		existing := candidate.Value()
		if existing == nil {
			continue
		}
		live = append(live, candidate)
		if scanShardEqual(existing, shard) {
			tableShard.scanBuckets[hash] = live
			return existing
		}
	}

	// No equal shard is known, so this one becomes canonical. The hash and
	// marker writes are safe because the shard is exclusively owned until its
	// cache is published.
	shard.internHash = hash
	shard.interned = true
	if tableShard.scanBuckets == nil {
		tableShard.scanBuckets = make(map[uint64][]weak.Pointer[scanCacheShard])
	}
	tableShard.scanBuckets[hash] = append(live, weak.Make(shard))
	tableShard.noteInsertion()
	return shard
}

// canonicalizeIgnoreShard is the IgnoreCache equivalent of
// canonicalizeScanShard.
func (i *CacheInterner) canonicalizeIgnoreShard(shard *ignoreCacheShard) *ignoreCacheShard {
	hash := ignoreShardHash(shard)

	tableShard := &i.shards[hash%internShardCount]
	tableShard.lock.Lock()
	defer tableShard.lock.Unlock()

	bucket := tableShard.ignoreBuckets[hash]
	live := bucket[:0]
	for _, candidate := range bucket {
		existing := candidate.Value()
		if existing == nil {
			continue
		}
		live = append(live, candidate)
		if ignoreShardEqual(existing, shard) {
			tableShard.ignoreBuckets[hash] = live
			return existing
		}
	}

	shard.internHash = hash
	shard.interned = true
	if tableShard.ignoreBuckets == nil {
		tableShard.ignoreBuckets = make(map[uint64][]weak.Pointer[ignoreCacheShard])
	}
	tableShard.ignoreBuckets[hash] = append(live, weak.Make(shard))
	tableShard.noteInsertion()
	return shard
}

// noteInsertion updates the insertion-scheduled sweep counter and sweeps when
// due. It must be called with the shard's lock held.
func (s *cacheInternerShard) noteInsertion() {
	s.insertionsSinceSweep++
	if s.insertionsSinceSweep > (len(s.scanBuckets)+len(s.ignoreBuckets))/2+minimumSweepInterval/internShardCount {
		s.sweep()
	}
}

// sweep drops collected shards throughout both of the shard's tables, along
// with any bucket left empty. It must be called with the shard's lock held.
func (s *cacheInternerShard) sweep() {
	s.insertionsSinceSweep = 0
	for hash, bucket := range s.scanBuckets {
		live := bucket[:0]
		for _, candidate := range bucket {
			if candidate.Value() != nil {
				live = append(live, candidate)
			}
		}
		if len(live) == 0 {
			delete(s.scanBuckets, hash)
			continue
		}
		s.scanBuckets[hash] = live
	}
	for hash, bucket := range s.ignoreBuckets {
		live := bucket[:0]
		for _, candidate := range bucket {
			if candidate.Value() != nil {
				live = append(live, candidate)
			}
		}
		if len(live) == 0 {
			delete(s.ignoreBuckets, hash)
			continue
		}
		s.ignoreBuckets[hash] = live
	}
}

// size returns the number of buckets currently held across both tables. It
// exists for tests and diagnostics: because shards are held weakly, the
// table's size is the only externally visible evidence that eviction is
// working.
func (i *CacheInterner) size() int {
	var total int
	for s := range i.shards {
		shard := &i.shards[s]
		shard.lock.Lock()
		total += len(shard.scanBuckets) + len(shard.ignoreBuckets)
		shard.lock.Unlock()
	}
	return total
}

// scanShardEqual reports whether two ScanCache shards hold identical content.
func scanShardEqual(a, b *scanCacheShard) bool {
	if a == b {
		return true
	}
	if len(a.entries) != len(b.entries) {
		return false
	}
	for name, entry := range a.entries {
		other := b.entries[name]
		if other == nil {
			return false
		}
		if other != entry {
			equivalent := other.mode == entry.mode &&
				other.modificationTimeSeconds == entry.modificationTimeSeconds &&
				other.modificationTimeNanos == entry.modificationTimeNanos &&
				other.size == entry.size &&
				other.fileID == entry.fileID &&
				bytes.Equal(other.digest, entry.digest)
			if !equivalent {
				return false
			}
		}
	}
	return true
}

// ignoreShardEqual reports whether two IgnoreCache shards hold identical
// content.
func ignoreShardEqual(a, b *ignoreCacheShard) bool {
	if a == b {
		return true
	}
	if len(a.entries) != len(b.entries) {
		return false
	}
	for key, value := range a.entries {
		if other, ok := b.entries[key]; !ok || other != value {
			return false
		}
	}
	return true
}

// scanShardHash computes a content hash for a ScanCache shard. Entry
// contributions are summed rather than sequenced because map iteration order
// is randomized and the hash must not depend on it. Only hash quality depends
// on the mixing; correctness rests on scanShardEqual.
func scanShardHash(shard *scanCacheShard) uint64 {
	var hash uint64
	for name, entry := range shard.entries {
		h := maphash.String(internHashSeed, name)
		h = mix64(h ^ uint64(entry.mode))
		h = mix64(h ^ uint64(entry.modificationTimeSeconds))
		h = mix64(h ^ uint64(uint32(entry.modificationTimeNanos)))
		h = mix64(h ^ entry.size)
		h = mix64(h ^ entry.fileID)
		if len(entry.digest) > 0 {
			h = mix64(h ^ maphash.Bytes(internHashSeed, entry.digest))
		}
		hash += h
	}
	return mix64(hash)
}

// ignoreShardHash computes a content hash for an IgnoreCache shard, with the
// same ordering and correctness properties as scanShardHash.
func ignoreShardHash(shard *ignoreCacheShard) uint64 {
	var hash uint64
	for key, value := range shard.entries {
		h := maphash.String(internHashSeed, key.name)
		if key.directory {
			h = mix64(h * 3)
		}
		if value {
			h = mix64(h * 5)
		}
		hash += mix64(h)
	}
	return mix64(hash)
}
