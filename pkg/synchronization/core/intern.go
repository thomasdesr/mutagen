package core

// DigestInterner deduplicates file digests so that every entry and cache entry
// holding a given content digest references a single backing array. The same
// digest is otherwise stored separately in each of a session's trees (ancestor,
// alpha, and beta) and in its scan cache, because each of those arrives from a
// separate Protocol Buffers decode or hash computation.
//
// An interner is not safe for concurrent use and holds a table proportional in
// size to the number of distinct digests it has seen. It is designed to be
// created for a single interning pass and then dropped: the sharing that a pass
// establishes outlives the table that established it, so a long-lived table buys
// nothing while costing more per distinct digest than the duplicate storage it
// eliminates.
type DigestInterner struct {
	// canonical maps digest content to the backing array chosen for it.
	canonical map[string][]byte
}

// NewDigestInterner creates a new interner with an empty table. The capacity
// hint should be the expected number of distinct digests, or zero if it isn't
// known. Growing the table dominates the cost of an interning pass, so a rough
// hint is worth supplying: for a 150k-file tree, a good hint halves the pass
// duration and the transient memory it needs.
func NewDigestInterner(capacityHint int) *DigestInterner {
	return &DigestInterner{canonical: make(map[string][]byte, capacityHint)}
}

// Intern returns a digest with the same content as the specified digest, either
// the digest itself (if its content hasn't been seen before, in which case it
// becomes the canonical storage for that content) or the previously interned
// digest with the same content. Empty digests are returned as-is and aren't
// recorded, since they represent entries that carry no digest.
func (i *DigestInterner) Intern(digest []byte) []byte {
	// Ignore empty digests.
	if len(digest) == 0 {
		return digest
	}

	// Look for existing canonical storage for this content. Go avoids
	// allocating for the string conversion in a map index expression.
	if canonical, ok := i.canonical[string(digest)]; ok {
		return canonical
	}

	// Adopt this digest as the canonical storage for its content.
	i.canonical[string(digest)] = digest
	return digest
}

// InternEntries replaces the digests within an entry hierarchy with interned
// digests, recording any content not already in the table.
//
// Entries are immutable by convention, and this method breaks that convention:
// it replaces digest references with references to equal content, which is
// invisible to every reader of an entry but is still a write. It must therefore
// only be used on a hierarchy that the caller exclusively owns and hasn't yet
// published, such as one freshly decoded from an archive or from the wire. Use
// SeedFromEntries for hierarchies that other code can observe.
func (i *DigestInterner) InternEntries(root *Entry) {
	walkFileEntries(root, func(entry *Entry) {
		entry.Digest = i.Intern(entry.Digest)
	})
}

// InternCache replaces the digests within a cache with interned digests,
// recording any content not already in the table. As with InternEntries, the
// cache must be exclusively owned by the caller and not yet published, since
// caches are also treated as immutable.
func (i *DigestInterner) InternCache(cache *Cache) {
	for _, entry := range cache.GetEntries() {
		entry.Digest = i.Intern(entry.Digest)
	}
}

// SeedFromEntries records the digests within an entry hierarchy in the table
// without modifying the hierarchy. Digests interned after seeding adopt the
// seed's storage, which is how a freshly decoded tree comes to share storage
// with an already-live tree that the caller doesn't own.
func (i *DigestInterner) SeedFromEntries(root *Entry) {
	walkFileEntries(root, func(entry *Entry) {
		i.Intern(entry.Digest)
	})
}

// Len returns the number of distinct digests in the table.
func (i *DigestInterner) Len() int {
	return len(i.canonical)
}

// walkFileEntries invokes the specified callback on every file entry within an
// entry hierarchy. Only file entries carry digests.
func walkFileEntries(entry *Entry, visitor func(*Entry)) {
	if entry == nil {
		return
	} else if entry.Kind == EntryKind_File {
		visitor(entry)
		return
	}
	for _, child := range entry.Contents {
		walkFileEntries(child, visitor)
	}
}
