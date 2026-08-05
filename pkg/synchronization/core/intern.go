package core

import (
	"bytes"
	"hash/maphash"
	"sync"
	"weak"
)

// Interner collapses structurally identical Entry subtrees onto a single shared
// instance. It exists to exploit the immutability contract that Entry already
// documents ("Entry objects should be considered immutable and must not be
// modified"): because no code may modify an entry once it is published, two
// subtrees with identical content are interchangeable and only one copy needs to
// exist in memory.
//
// The daemon retains several structurally identical copies of the same tree at
// steady state — per session an ancestor, an alpha snapshot, and a beta snapshot
// decoded from the remote, all equal when the session is fully synchronized —
// and sessions that watch the same local directory duplicate that again.
// Interning those trees against a shared Interner collapses them to a single
// object graph.
//
// Interned subtrees are held weakly, so a subtree survives only as long as some
// tree still references it. A session terminating (or simply an unchanged
// subtree being replaced by a changed one) drops the last reference, the
// canonical node is collected, and the table's record of it is removed by the
// next sweep without any explicit bookkeeping by the caller. Nothing in the
// table keeps an entry alive and no interned entry keeps the table alive, so a
// caller that wants sharing without a persistent table can discard the Interner
// as soon as it has finished a batch of trees.
//
// An Interner is safe for concurrent use and its zero value is ready to use.
type Interner struct {
	// shards partition the table by subtree hash. Locking is per shard and
	// acquired once per node, so concurrent sessions' full-tree passes
	// interleave across shards instead of serializing behind one mutex
	// (which measurement showed dominating scheduling delay during
	// concurrent rescans).
	shards [internShardCount]internerShard
}

// internerShard is one lock-partitioned portion of an Interner's table.
type internerShard struct {
	// lock guards the shard.
	lock sync.Mutex
	// buckets maps a subtree hash to the canonical entries carrying that
	// hash. A bucket holds more than one entry only on hash collision.
	buckets map[uint64][]weak.Pointer[Entry]
	// insertionsSinceSweep counts insertions since the shard's last sweep.
	insertionsSinceSweep int
}

// internShardCount is the number of lock shards per interning table. Power
// of two so that routing is a mask.
const internShardCount = 64

// sharedInterner backs SharedInterner.
var sharedInterner Interner

// SharedInterner returns the process-wide interning table. Every site that
// decodes, scans, or constructs a long-lived Entry tree interns against this
// single table so that identical trees collapse across sessions, not just
// within one.
func SharedInterner() *Interner {
	return &sharedInterner
}

// Intern returns an entry hierarchy equivalent to root in which every subtree
// has been replaced by a canonical instance shared with all previously interned
// equal subtrees.
//
// The caller must have exclusive ownership of root's non-canonical nodes:
// Intern rewrites content maps in place where a child's identity changes, so
// root must not yet be visible to any other goroutine. Subtrees within root
// that are already canonical (previously published by an interning site, then
// spliced into this tree) are traversed write-free and are safe to share with
// concurrent readers. Freshly decoded and freshly scanned trees satisfy this,
// which is why those are the intended call sites. The returned tree is shared
// and must be treated as immutable, exactly as the Entry contract already
// requires.
func (i *Interner) Intern(root *Entry) *Entry {
	entry, _ := i.internSubtree(root)
	return entry
}

// Sweep drops the table's records of subtrees that have been collected. Interning
// sweeps on its own insertion schedule, which tracks ongoing churn well because a
// changed subtree both retires an old node and inserts a new one. It does not
// cover a table that stops being written to entirely, which is what a terminating
// session produces: the session's trees become unreachable and nothing inserts
// again. Callers should call Sweep on that signal.
func (i *Interner) Sweep() {
	for s := range i.shards {
		shard := &i.shards[s]
		shard.lock.Lock()
		shard.sweep()
		shard.lock.Unlock()
	}
}

// internSubtree interns a single subtree bottom-up, returning the canonical
// instance and its subtree hash.
func (i *Interner) internSubtree(e *Entry) (*Entry, uint64) {
	// A nil entry represents an absence of content. There is nothing to share
	// and no allocation to save, but it still needs a hash that distinguishes it
	// from any real entry so that parents hashing it don't collide.
	if e == nil {
		return nil, nilEntryHash
	}

	// Intern children first so that this entry's children are canonical by the
	// time we hash and compare it. This is what allows both the hash and the
	// equality check to treat children as opaque identities instead of
	// re-walking their subtrees.
	hash := e.selfHash()
	for name, child := range e.Contents {
		canonicalChild, childHash := i.internSubtree(child)
		// Write only when the child actually changes identity. This keeps the
		// pass write-free over subtrees that are already canonical, which is
		// what makes it safe to intern trees that splice in previously
		// published (and thus concurrently readable) subtrees, as accelerated
		// scans do: published subtrees are canonical inductively, provided
		// every publish site interns.
		if canonicalChild != child {
			e.Contents[name] = canonicalChild
		}
		// Child contributions are summed rather than sequenced because map
		// iteration order is randomized and the hash must not depend on it.
		hash += mixChild(name, childHash)
	}

	return i.canonicalize(e, hash), hash
}

// canonicalize returns the canonical entry equal to e, registering e as the
// canonical instance if no equal entry is known. The children of e must already
// be canonical.
func (i *Interner) canonicalize(e *Entry, hash uint64) *Entry {
	shard := &i.shards[hash%internShardCount]
	shard.lock.Lock()
	defer shard.lock.Unlock()

	// Scan the bucket for an equal entry, compacting away any entries that have
	// been collected since we last looked. Opportunistic compaction here keeps
	// buckets short; removal of buckets that are never looked up again is handled
	// by sweeping.
	bucket := shard.buckets[hash]
	live := bucket[:0]
	for _, candidate := range bucket {
		existing := candidate.Value()
		if existing == nil {
			continue
		}
		live = append(live, candidate)
		if canonicalEqual(existing, e) {
			// Preserve the compaction we just performed before returning.
			shard.buckets[hash] = live
			return existing
		}
	}

	// No equal entry is known, so e becomes canonical.
	if shard.buckets == nil {
		shard.buckets = make(map[uint64][]weak.Pointer[Entry])
	}
	shard.buckets[hash] = append(live, weak.Make(e))

	// Buckets that are never looked up again are never compacted above, so their
	// map entries would otherwise accumulate for every subtree the daemon has
	// ever seen. Sweep periodically to bound that.
	shard.insertionsSinceSweep++
	if shard.insertionsSinceSweep > len(shard.buckets)/2+minimumSweepInterval/internShardCount {
		shard.sweep()
	}

	return e
}

// sweep drops collected entries throughout the shard, along with any bucket
// left empty. It is called with the shard's lock held, on an insertion
// schedule that keeps its amortized cost per insertion constant.
func (s *internerShard) sweep() {
	s.insertionsSinceSweep = 0
	for hash, bucket := range s.buckets {
		live := bucket[:0]
		for _, candidate := range bucket {
			if candidate.Value() != nil {
				live = append(live, candidate)
			}
		}
		if len(live) == 0 {
			delete(s.buckets, hash)
			continue
		}
		s.buckets[hash] = live
	}
}

// minimumSweepInterval is the smallest number of insertions between sweeps. It
// keeps small tables from sweeping on nearly every insertion.
const minimumSweepInterval = 1024

// size returns the number of buckets currently held by the table. It exists for
// tests and diagnostics: because entries are held weakly, the table's size is
// the only externally visible evidence that eviction is working.
func (i *Interner) size() int {
	var total int
	for s := range i.shards {
		shard := &i.shards[s]
		shard.lock.Lock()
		total += len(shard.buckets)
		shard.lock.Unlock()
	}
	return total
}

// canonicalEqual reports whether two entries are equal, assuming that the
// children of both are already canonical. Under that assumption children can be
// compared by pointer identity, which makes the comparison shallow and exact:
// two canonical children are equal if and only if they are the same object.
func canonicalEqual(a, b *Entry) bool {
	if a == b {
		return true
	}

	propertiesEqual := a.Kind == b.Kind &&
		a.Executable == b.Executable &&
		a.Target == b.Target &&
		a.Problem == b.Problem &&
		bytes.Equal(a.Digest, b.Digest)
	if !propertiesEqual {
		return false
	}

	if len(a.Contents) != len(b.Contents) {
		return false
	}
	for name, child := range a.Contents {
		if b.Contents[name] != child {
			return false
		}
	}

	return true
}

// selfHash returns a hash of the entry's own properties, excluding its contents.
// Every field that canonicalEqual compares is also hashed here, so two entries
// that hash equally and compare equal are genuinely interchangeable. Only hash
// quality depends on the mixing below; correctness does not, because
// canonicalEqual compares the fields exactly.
func (e *Entry) selfHash() uint64 {
	// Kind and executability are small enough to fold in directly. The variable
	// length fields go through maphash, each combined with a distinct multiplier
	// so that the same bytes appearing in a different field produce a different
	// contribution.
	h := uint64(e.Kind) << 1
	if e.Executable {
		h |= 1
	}
	if len(e.Digest) > 0 {
		h = mix64(h ^ maphash.Bytes(internHashSeed, e.Digest))
	}
	if e.Target != "" {
		h = mix64(h*3 ^ maphash.String(internHashSeed, e.Target))
	}
	if e.Problem != "" {
		h = mix64(h*5 ^ maphash.String(internHashSeed, e.Problem))
	}
	return mix64(h)
}

// mixChild returns a child's contribution to its parent's subtree hash. The
// result is a function of the child's name and subtree hash, scrambled so that
// summing contributions across children doesn't cause unrelated name and hash
// pairs to cancel out.
func mixChild(name string, childHash uint64) uint64 {
	return mix64(maphash.String(internHashSeed, name) ^ (childHash * 0x9e3779b97f4a7c15))
}

// mix64 avalanches a 64-bit value so that any input bit affects every output bit.
// It is the finalizer from MurmurHash3.
func mix64(h uint64) uint64 {
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// internHashSeed is the process-wide seed for subtree hashing. Intern tables
// never outlive the process, so a per-process random seed is sufficient and
// keeps subtree hashes from being externally predictable.
var internHashSeed = maphash.MakeSeed()

// nilEntryHash is the subtree hash of a nil entry. Its value is arbitrary; it
// only needs to be stable and unlikely to coincide with a real entry's hash.
const nilEntryHash = 0x9ae16a3b2f90404f
