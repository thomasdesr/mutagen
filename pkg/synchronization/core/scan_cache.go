package core

import (
	"bytes"
	"errors"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// ScanCache is the in-memory representation of a Cache. It shards entries by
// directory (map[directory]map[name]*ScanCacheEntry) so that repeated
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
	// directories maps directory path to the entries directly within it,
	// keyed by base name.
	directories map[string]map[string]*ScanCacheEntry
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
	return &ScanCache{directories: make(map[string]map[string]*ScanCacheEntry, directoryCapacityHint)}
}

// Proto converts a ScanCache into a wire-format Cache.
func (c *ScanCache) Proto() *Cache {
	result := &Cache{Entries: make(map[string]*CacheEntry, c.len())}
	if c == nil {
		return result
	}
	for directory, names := range c.directories {
		for name, entry := range names {
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
	return c.directories[directory][name]
}

// set stores the entry for the file named name within directory. It
// allocates the inner per-directory map on first use.
func (c *ScanCache) set(directory, name string, entry *ScanCacheEntry) {
	names := c.directories[directory]
	if names == nil {
		names = make(map[string]*ScanCacheEntry)
		c.directories[directory] = names
	}
	names[name] = entry
}

// len returns the total number of entries in the cache.
func (c *ScanCache) len() int {
	if c == nil {
		return 0
	}
	var total int
	for _, names := range c.directories {
		total += len(names)
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

	// Check contents.
	for directory, names := range c.directories {
		otherNames := other.directories[directory]
		for name, entry := range names {
			otherEntry := otherNames[name]
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
	for directory, names := range c.directories {
		for name, entry := range names {
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
