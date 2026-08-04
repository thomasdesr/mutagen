package core

import (
	"bytes"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestDigestInternerCanonicalizesEqualDigests verifies that equal digests are
// collapsed onto a single backing array and that unequal digests aren't.
func TestDigestInternerCanonicalizesEqualDigests(t *testing.T) {
	// Create the interner.
	interner := NewDigestInterner(0)

	// Intern two copies of one digest and one copy of another.
	first := interner.Intern([]byte{1, 2, 3, 4})
	second := interner.Intern([]byte{1, 2, 3, 4})
	other := interner.Intern([]byte{1, 2, 3, 5})

	// Verify that values are preserved.
	if !bytes.Equal(first, []byte{1, 2, 3, 4}) {
		t.Error("first digest value not preserved:", first)
	} else if !bytes.Equal(second, []byte{1, 2, 3, 4}) {
		t.Error("second digest value not preserved:", second)
	} else if !bytes.Equal(other, []byte{1, 2, 3, 5}) {
		t.Error("other digest value not preserved:", other)
	}

	// Verify sharing.
	if !sharesBackingArray(first, second) {
		t.Error("equal digests not collapsed onto a single backing array")
	} else if sharesBackingArray(first, other) {
		t.Error("unequal digests collapsed onto a single backing array")
	}

	// Verify the table size.
	if interner.Len() != 2 {
		t.Error("unexpected intern table size:", interner.Len())
	}
}

// TestDigestInternerDistinguishesDigestLengths verifies that digests of
// different lengths aren't collapsed, even when one is a prefix of the other.
func TestDigestInternerDistinguishesDigestLengths(t *testing.T) {
	// Create the interner.
	interner := NewDigestInterner(0)

	// Intern a digest and a longer digest with the same prefix.
	short := interner.Intern([]byte{9, 9})
	long := interner.Intern([]byte{9, 9, 9, 9})

	// Verify that neither value was altered.
	if !bytes.Equal(short, []byte{9, 9}) {
		t.Error("short digest value not preserved:", short)
	} else if !bytes.Equal(long, []byte{9, 9, 9, 9}) {
		t.Error("long digest value not preserved:", long)
	}
}

// TestDigestInternerEmptyDigests verifies that empty and nil digests survive
// interning unchanged, since they mark non-file entries.
func TestDigestInternerEmptyDigests(t *testing.T) {
	// Create the interner.
	interner := NewDigestInterner(0)

	// Verify that a nil digest interns to a nil digest.
	if interner.Intern(nil) != nil {
		t.Error("nil digest not preserved")
	}

	// Verify that an empty digest interns to an empty digest.
	if empty := interner.Intern([]byte{}); len(empty) != 0 {
		t.Error("empty digest not preserved:", empty)
	}

	// Verify that neither was recorded in the table.
	if interner.Len() != 0 {
		t.Error("empty digests recorded in intern table:", interner.Len())
	}
}

// TestDigestInternerEntries verifies that interning an entry hierarchy preserves
// its contents while collapsing duplicate digests, both within one hierarchy and
// across hierarchies interned with the same table.
func TestDigestInternerEntries(t *testing.T) {
	// Create two identical hierarchies with independent digest storage. The
	// hierarchy contains two files with identical content ("duplicate" and
	// "nested/duplicate"), one file with unique content, and unsynchronizable
	// content that must be left alone.
	first := internTestHierarchy()
	second := internTestHierarchy()
	expected := internTestHierarchy()

	// Intern both hierarchies with a single table.
	interner := NewDigestInterner(0)
	interner.InternEntries(first)
	interner.InternEntries(second)

	// Verify that neither hierarchy's contents changed.
	if !first.Equal(expected, true) {
		t.Error("interning altered the first hierarchy")
	} else if !second.Equal(expected, true) {
		t.Error("interning altered the second hierarchy")
	}

	// Verify that the table contains only the distinct digests.
	if interner.Len() != 2 {
		t.Error("unexpected intern table size:", interner.Len())
	}

	// Verify that duplicate content within a hierarchy shares storage.
	duplicate := first.Contents["duplicate"].Digest
	nestedDuplicate := first.Contents["nested"].Contents["duplicate"].Digest
	if !sharesBackingArray(duplicate, nestedDuplicate) {
		t.Error("duplicate content within a hierarchy not collapsed")
	}

	// Verify that identical content across hierarchies shares storage.
	if !sharesBackingArray(duplicate, second.Contents["duplicate"].Digest) {
		t.Error("duplicate content across hierarchies not collapsed")
	}
	if !sharesBackingArray(first.Contents["unique"].Digest, second.Contents["unique"].Digest) {
		t.Error("unique content across hierarchies not collapsed")
	}

	// Verify that unsynchronizable content still has no digest.
	if first.Contents["untracked"].Digest != nil {
		t.Error("digest added to untracked content")
	}
}

// TestDigestInternerSeedFromEntries verifies that seeding registers a
// hierarchy's digests without modifying the hierarchy, so that a subsequently
// interned hierarchy adopts the seed's storage.
func TestDigestInternerSeedFromEntries(t *testing.T) {
	// Create a seed hierarchy and a hierarchy to intern.
	seed := internTestHierarchy()
	target := internTestHierarchy()

	// Record the seed's digest storage.
	seedDigest := seed.Contents["unique"].Digest

	// Seed the interner and then intern the target.
	interner := NewDigestInterner(0)
	interner.SeedFromEntries(seed)
	interner.InternEntries(target)

	// Verify that the seed's storage is unchanged and now shared.
	if !sharesBackingArray(seed.Contents["unique"].Digest, seedDigest) {
		t.Error("seeding replaced the seed hierarchy's digest storage")
	}
	if !sharesBackingArray(target.Contents["unique"].Digest, seedDigest) {
		t.Error("interned hierarchy did not adopt the seed's digest storage")
	}
}

// TestDigestInternerCache verifies that interning a cache preserves its contents
// while collapsing duplicate digests, and that a cache and an entry hierarchy
// interned with the same table share digest storage.
func TestDigestInternerCache(t *testing.T) {
	// Create a hierarchy and a cache covering it.
	hierarchy := internTestHierarchy()
	cache := &Cache{
		Entries: map[string]*CacheEntry{
			"duplicate":        internTestCacheEntry([]byte{1, 1, 1, 1}),
			"nested/duplicate": internTestCacheEntry([]byte{1, 1, 1, 1}),
			"unique":           internTestCacheEntry([]byte{2, 2, 2, 2}),
		},
	}
	expected := &Cache{
		Entries: map[string]*CacheEntry{
			"duplicate":        internTestCacheEntry([]byte{1, 1, 1, 1}),
			"nested/duplicate": internTestCacheEntry([]byte{1, 1, 1, 1}),
			"unique":           internTestCacheEntry([]byte{2, 2, 2, 2}),
		},
	}

	// Intern the hierarchy and then the cache with a single table.
	interner := NewDigestInterner(0)
	interner.InternEntries(hierarchy)
	interner.InternCache(cache)

	// Verify that the cache's contents didn't change.
	if !cache.Equal(expected) {
		t.Error("interning altered the cache")
	}

	// Verify that duplicate content within the cache shares storage.
	if !sharesBackingArray(cache.Entries["duplicate"].Digest, cache.Entries["nested/duplicate"].Digest) {
		t.Error("duplicate content within the cache not collapsed")
	}

	// Verify that the cache adopted the hierarchy's digest storage.
	if !sharesBackingArray(cache.Entries["unique"].Digest, hierarchy.Contents["unique"].Digest) {
		t.Error("cache did not adopt the hierarchy's digest storage")
	}
}

// TestDigestInternerNilTargets verifies that interning and seeding tolerate
// absent content, which occurs for empty synchronization roots.
func TestDigestInternerNilTargets(t *testing.T) {
	interner := NewDigestInterner(0)
	interner.InternEntries(nil)
	interner.SeedFromEntries(nil)
	interner.InternCache(nil)
	interner.InternCache(&Cache{})
	if interner.Len() != 0 {
		t.Error("unexpected intern table size:", interner.Len())
	}
}

// internTestHierarchy creates the entry hierarchy used by interning tests. Each
// call allocates independent digest storage.
func internTestHierarchy() *Entry {
	return &Entry{
		Contents: map[string]*Entry{
			"duplicate": {
				Kind:   EntryKind_File,
				Digest: []byte{1, 1, 1, 1},
			},
			"unique": {
				Kind:   EntryKind_File,
				Digest: []byte{2, 2, 2, 2},
			},
			"nested": {
				Contents: map[string]*Entry{
					"duplicate": {
						Kind:   EntryKind_File,
						Digest: []byte{1, 1, 1, 1},
					},
					"link": {
						Kind:   EntryKind_SymbolicLink,
						Target: "../unique",
					},
				},
			},
			"untracked": {
				Kind: EntryKind_Untracked,
			},
		},
	}
}

// internTestCacheEntry creates a cache entry with the specified digest.
func internTestCacheEntry(digest []byte) *CacheEntry {
	return &CacheEntry{
		Mode:             0644,
		ModificationTime: timestamppb.New(time.Unix(1600000000, 0)),
		Size:             uint64(len(digest)),
		FileID:           1,
		Digest:           digest,
	}
}

// sharesBackingArray returns whether or not two byte slices are backed by the
// same array at the same offset.
func sharesBackingArray(first, second []byte) bool {
	if len(first) != len(second) {
		return false
	} else if len(first) == 0 {
		return true
	}
	return &first[0] == &second[0]
}
