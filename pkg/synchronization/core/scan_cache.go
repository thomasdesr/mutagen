package core

import (
	"bytes"
	"errors"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// ScanCache is the in-memory representation of a Cache. It shards entries by
// directory (one scanCacheShard per directory) so that repeated
// directory-prefix strings across a large tree are stored once instead of
// once per file, and stores modification time as inline seconds/nanoseconds
// instead of a heap-allocated *timestamppb.Timestamp. It is never persisted
// directly: Proto/NewScanCacheFromProto convert to/from the wire-format
// Cache at the disk-persistence boundary in endpoint/local.
//
// ScanCache is treated as immutable once constructed (mirroring Cache), so a
// nil *ScanCache is a valid, empty cache and all methods are nil-receiver
// safe.
type ScanCache struct {
	// directories maps directory path to the shard of entries directly
	// within it.
	directories map[string]*scanCacheShard
}

// scanCacheShard holds the cache entries directly within one directory,
// keyed by base name. Shards are boxed (rather than stored as bare maps) so
// that a CacheInterner can hold them weakly and identical shards can be
// shared across caches, generations, and endpoints. A shard is immutable
// once its owning cache is published, exactly as the cache itself is.
type scanCacheShard struct {
	// entries maps base name to cache entry.
	entries map[string]*ScanCacheEntry
	// internHash is the shard's content hash, valid only when interned is
	// true. It is written only by CacheInterner.canonicalize, before the
	// shard is ever shared, so post-publication reads are safe.
	internHash uint64
	// interned indicates that this shard is the canonical instance recorded
	// by a CacheInterner, allowing repeat introductions to short-circuit.
	// Its write-safety argument matches internHash's.
	interned bool
}

// ScanCacheEntry is the in-memory representation of a CacheEntry.
type ScanCacheEntry struct {
	// mode is the value of CacheEntry.Mode.
	mode uint32
	// modificationTimeSeconds is CacheEntry.ModificationTime.Seconds.
	modificationTimeSeconds int64
	// modificationTimeNanos is CacheEntry.ModificationTime.Nanos.
	modificationTimeNanos int32
	// size is the value of CacheEntry.Size.
	size uint64
	// fileID is the value of CacheEntry.FileID.
	fileID uint64
	// digest is the value of CacheEntry.Digest.
	digest []byte
}

// NewScanCacheFromProto converts a wire-format Cache into a ScanCache. The
// provided cache must already satisfy EnsureValid (in particular, every
// entry's ModificationTime must be non-nil); this is the case for caches
// loaded from disk in endpoint/local, which validate before conversion.
func NewScanCacheFromProto(cache *Cache) *ScanCache {
	if cache == nil || len(cache.Entries) == 0 {
		return nil
	}
	// The entry count over-estimates the outer directory-map's eventual size
	// (multiple entries typically share a directory), but that's preferable
	// to under-allocating and it's a one-time cost at load.
	result := newScanCache(len(cache.Entries))
	for path, entry := range cache.Entries {
		directory, name := splitCachePath(path)
		result.set(directory, name, &ScanCacheEntry{
			mode:                    entry.Mode,
			modificationTimeSeconds: entry.ModificationTime.Seconds,
			modificationTimeNanos:   entry.ModificationTime.Nanos,
			size:                    entry.Size,
			fileID:                  entry.FileID,
			digest:                  entry.Digest,
		})
	}
	return result
}

// newScanCache creates an empty ScanCache with its outer (per-directory) map
// capacity pre-allocated. The returned cache is always non-nil, unlike a
// bare zero-value ScanCache, so it's safe to call set on it immediately.
func newScanCache(directoryCapacityHint int) *ScanCache {
	return &ScanCache{directories: make(map[string]*scanCacheShard, directoryCapacityHint)}
}

// Proto converts a ScanCache into a wire-format Cache.
func (c *ScanCache) Proto() *Cache {
	result := &Cache{Entries: make(map[string]*CacheEntry, c.len())}
	if c == nil {
		return result
	}
	for directory, shard := range c.directories {
		for name, entry := range shard.entries {
			path := name
			if directory != "" {
				path = directory + "/" + name
			}
			result.Entries[path] = &CacheEntry{
				Mode: entry.mode,
				ModificationTime: &timestamppb.Timestamp{
					Seconds: entry.modificationTimeSeconds,
					Nanos:   entry.modificationTimeNanos,
				},
				Size:   entry.size,
				FileID: entry.fileID,
				Digest: entry.digest,
			}
		}
	}
	return result
}

// get looks up the entry for path, returning nil if it's not present.
func (c *ScanCache) get(path string) *ScanCacheEntry {
	if c == nil {
		return nil
	}
	directory, name := splitCachePath(path)
	shard := c.directories[directory]
	if shard == nil {
		return nil
	}
	return shard.entries[name]
}

// set stores the entry for the file named name within directory. It
// allocates the per-directory shard on first use.
func (c *ScanCache) set(directory, name string, entry *ScanCacheEntry) {
	shard := c.directories[directory]
	if shard == nil {
		shard = &scanCacheShard{entries: make(map[string]*ScanCacheEntry)}
		c.directories[directory] = shard
	}
	shard.entries[name] = entry
}

// len returns the total number of entries in the cache.
func (c *ScanCache) len() int {
	if c == nil {
		return 0
	}
	var total int
	for _, shard := range c.directories {
		total += len(shard.entries)
	}
	return total
}

// Equal determines whether or not another cache is equal to this one. It is
// designed specifically for tests, though it is exported so that it can be
// used by scan_bench.
func (c *ScanCache) Equal(other *ScanCache) bool {
	// Verify non-nilness. We don't consider nil caches valid, so we don't
	// consider them equal.
	if c == nil || other == nil {
		return false
	}

	// Handle equivalence fast paths.
	if c == other {
		return true
	}

	// Check lengths.
	if c.len() != other.len() {
		return false
	}

	// Check contents. Shards shared by pointer (via interning) are equal by
	// construction.
	for directory, shard := range c.directories {
		otherShard := other.directories[directory]
		if otherShard == nil {
			return false
		} else if otherShard == shard {
			continue
		}
		for name, entry := range shard.entries {
			otherEntry := otherShard.entries[name]
			if otherEntry == nil {
				return false
			}

			// Verify equivalence.
			equivalent := otherEntry.mode == entry.mode &&
				otherEntry.modificationTimeSeconds == entry.modificationTimeSeconds &&
				otherEntry.modificationTimeNanos == entry.modificationTimeNanos &&
				otherEntry.size == entry.size &&
				otherEntry.fileID == entry.fileID &&
				bytes.Equal(otherEntry.digest, entry.digest)
			if !equivalent {
				return false
			}
		}
	}

	// Success.
	return true
}

// GenerateReverseLookupMap creates a reverse lookup map from a cache.
func (c *ScanCache) GenerateReverseLookupMap() (*ReverseLookupMap, error) {
	// A nil cache has no entries, so it just needs an empty lookup map.
	if c == nil {
		return &ReverseLookupMap{&emptyByteLookupMap{}}, nil
	}

	// Create a placeholder for the map that we're going to initialize.
	var lookupMap byteLookupMap

	// Track the digest size and ensure it's consistent.
	digestSize := -1

	// Loop over entries.
	for directory, shard := range c.directories {
		for name, entry := range shard.entries {
			path := name
			if directory != "" {
				path = directory + "/" + name
			}

			// Compute and validate the digest size and allocate the map.
			if digestSize == -1 {
				digestSize = len(entry.digest)
				if digestSize == 20 {
					lookupMap = make(byteLookupMap20, c.len())
				} else if digestSize == 32 {
					lookupMap = make(byteLookupMap32, c.len())
				} else if digestSize == 16 {
					lookupMap = make(byteLookupMap16, c.len())
				} else {
					return nil, errors.New("unsupported digest size")
				}
			} else if len(entry.digest) != digestSize {
				return nil, errors.New("inconsistent digest sizes")
			}

			// Insert the entry.
			lookupMap.insert(entry.digest, path)
		}
	}

	// If there are no entries, then we'll still need a lookup map.
	if c.len() == 0 {
		lookupMap = &emptyByteLookupMap{}
	}

	// Success.
	return &ReverseLookupMap{lookupMap}, nil
}
