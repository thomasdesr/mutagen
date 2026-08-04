package core

import (
	"errors"
	"fmt"
	pathpkg "path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// ignorePattern represents a single parsed ignore pattern.
type ignorePattern struct {
	// negated indicates whether or not the pattern is negated.
	negated bool
	// directoryOnly indicates whether or not the pattern should only match
	// directories.
	directoryOnly bool
	// matchLeaf indicates whether or not the pattern should be matched against
	// a path's base name in addition to the whole path.
	matchLeaf bool
	// pattern is the pattern to use in matching.
	pattern string
}

// newIgnorePattern validates and parses a user-provided ignore pattern.
func newIgnorePattern(pattern string) (*ignorePattern, error) {
	// Check for invalid patterns, or at least those that would leave us with an
	// empty string after parsing. Obviously we can't perform general complete
	// validation for all patterns, but if they pass this parsing, they should
	// be sane enough to at least try to match.
	if pattern == "" || pattern == "!" {
		return nil, errors.New("empty pattern")
	} else if pattern == "/" || pattern == "!/" {
		return nil, errors.New("root pattern")
	} else if pattern == "//" || pattern == "!//" {
		return nil, errors.New("root directory pattern")
	}

	// Check if this is a negated pattern. If so, remove the exclamation point
	// prefix, since it won't enter into pattern matching.
	negated := false
	if pattern[0] == '!' {
		negated = true
		pattern = pattern[1:]
	}

	// Check if this is an absolute pattern. If so, remove the forward slash
	// prefix, since it won't enter into pattern matching.
	absolute := false
	if pattern[0] == '/' {
		absolute = true
		pattern = pattern[1:]
	}

	// Check if this is a directory-only pattern. If so, remove the trailing
	// slash, since it won't enter into pattern matching.
	directoryOnly := false
	if pattern[len(pattern)-1] == '/' {
		directoryOnly = true
		pattern = pattern[:len(pattern)-1]
	}

	// Determine whether or not the pattern contains a slash.
	containsSlash := strings.IndexByte(pattern, '/') >= 0

	// Attempt to do a match with the pattern to ensure validity. We have to
	// match against a non-empty path (we choose something simple), otherwise
	// bad pattern errors won't be detected.
	if _, err := doublestar.Match(pattern, "a"); err != nil {
		return nil, fmt.Errorf("unable to validate pattern: %w", err)
	}

	// Success.
	return &ignorePattern{
		negated:       negated,
		directoryOnly: directoryOnly,
		matchLeaf:     (!absolute && !containsSlash),
		pattern:       pattern,
	}, nil
}

// matches indicates whether or not the ignore pattern matches the specified
// path and metadata.
func (i *ignorePattern) matches(path string, directory bool) (bool, bool) {
	// If this pattern only applies to directories and this is not a directory,
	// then this is not a match.
	if i.directoryOnly && !directory {
		return false, false
	}

	// Check if there is a direct match. Since we've already validated the
	// pattern in the constructor, we know match can't fail with an error (it's
	// only return code is on bad patterns).
	if match, _ := doublestar.Match(i.pattern, path); match {
		return true, i.negated
	}

	// If it makes sense, attempt to match on the last component of the path,
	// assuming the path is non-empty (non-root).
	if i.matchLeaf && path != "" {
		if match, _ := doublestar.Match(i.pattern, pathpkg.Base(path)); match {
			return true, i.negated
		}
	}

	// No match.
	return false, false
}

// ValidIgnorePattern checks whether or not a given pattern is a valid ignore
// specification.
func ValidIgnorePattern(pattern string) bool {
	// Verify that we can parse the ignore.
	_, err := newIgnorePattern(pattern)
	return err == nil
}

// ignorer is a collection of parsed ignore patterns.
type ignorer struct {
	// patterns are the underlying ignore patterns.
	patterns []*ignorePattern
}

// newIgnorer creates a new ignorer given a list of user-provided ignore
// patterns.
func newIgnorer(patterns []string) (*ignorer, error) {
	// Parse patterns.
	ignorePatterns := make([]*ignorePattern, len(patterns))
	for i, p := range patterns {
		if ip, err := newIgnorePattern(p); err != nil {
			return nil, fmt.Errorf("unable to parse pattern: %w", err)
		} else {
			ignorePatterns[i] = ip
		}
	}

	// Success.
	return &ignorer{ignorePatterns}, nil
}

// ignored determines whether or not the specified path should be ignored based
// on all provided ignore patterns and their order.
func (i *ignorer) ignored(path string, directory bool) bool {
	// Nothing is initially ignored.
	ignored := false

	// Run through patterns, keeping track of the ignored state as we reach more
	// specific rules.
	for _, p := range i.patterns {
		if match, negated := p.matches(path, directory); !match {
			continue
		} else {
			ignored = !negated
		}
	}

	// Done.
	return ignored
}

// IgnoreCache provides an efficient mechanism to avoid recomputing ignores.
// It shards entries by directory (one ignoreCacheShard per directory) so
// that repeated directory-prefix strings across a large tree are stored once
// instead of once per checked path.
type IgnoreCache map[string]*ignoreCacheShard

// ignoreCacheShard holds the ignore determinations for the paths directly
// within one directory. Shards are boxed (rather than stored as bare maps)
// so that a CacheInterner can hold them weakly and identical shards can be
// shared across caches, generations, and endpoints. A shard is immutable
// once its owning cache is published, exactly as the cache itself is.
type ignoreCacheShard struct {
	// entries maps within-directory keys to ignore determinations.
	entries map[ignoreCacheEntryKey]bool
	// internHash is the shard's content hash, valid only when interned is
	// true. It is written only by CacheInterner.canonicalize, before the
	// shard is ever shared, so post-publication reads are safe.
	internHash uint64
	// interned indicates that this shard is the canonical instance recorded
	// by a CacheInterner, allowing repeat introductions to short-circuit.
	// Its write-safety argument matches internHash's.
	interned bool
}

// ignoreCacheEntryKey represents the within-directory portion of a key in an
// IgnoreCache.
type ignoreCacheEntryKey struct {
	// name is the base name used for testing ignore status.
	name string
	// directory is whether or not that path was a directory.
	directory bool
}

// get looks up whether path (with the given directory-ness) is ignored,
// returning ok as false if there's no cached answer.
func (c IgnoreCache) get(path string, directory bool) (ignored, ok bool) {
	dir, name := splitCachePath(path)
	shard := c[dir]
	if shard == nil {
		return false, false
	}
	ignored, ok = shard.entries[ignoreCacheEntryKey{name, directory}]
	return
}

// set records whether path (with the given directory-ness) is ignored. The
// receiver must be non-nil.
func (c IgnoreCache) set(path string, directory bool, ignored bool) {
	dir, name := splitCachePath(path)
	shard := c[dir]
	if shard == nil {
		shard = &ignoreCacheShard{entries: make(map[ignoreCacheEntryKey]bool)}
		c[dir] = shard
	}
	shard.entries[ignoreCacheEntryKey{name, directory}] = ignored
}

// Len returns the total number of entries in the cache.
func (c IgnoreCache) Len() int {
	var total int
	for _, shard := range c {
		total += len(shard.entries)
	}
	return total
}

// Equal determines whether or not another ignore cache is equal to this one.
// It is designed specifically for tests.
func (c IgnoreCache) Equal(other IgnoreCache) bool {
	if c.Len() != other.Len() {
		return false
	}
	for directory, shard := range c {
		otherShard := other[directory]
		if otherShard == nil {
			return false
		} else if otherShard == shard {
			continue
		}
		for key, value := range shard.entries {
			if otherValue, ok := otherShard.entries[key]; !ok || otherValue != value {
				return false
			}
		}
	}
	return true
}

// AcceleratedSubsetOf verifies that an accelerated ignore cache (the
// receiver) is a subset of original, excluding the presence of a root path
// key in the accelerated case.
func (accelerated IgnoreCache) AcceleratedSubsetOf(original IgnoreCache) bool {
	for directory, shard := range accelerated {
		originalShard := original[directory]
		for key, value := range shard.entries {
			if directory == "" && key.name == "" {
				continue
			} else if originalShard == nil {
				return false
			} else if otherValue, ok := originalShard.entries[key]; !ok || otherValue != value {
				return false
			}
		}
	}
	return true
}

// IntersectionEqual verifies that two ignore caches agree on every key
// present in both of them, ignoring keys present in only one.
func (c IgnoreCache) IntersectionEqual(other IgnoreCache) bool {
	for directory, shard := range c {
		otherShard := other[directory]
		if otherShard == nil {
			continue
		}
		for key, value := range shard.entries {
			if otherValue, ok := otherShard.entries[key]; ok && otherValue != value {
				return false
			}
		}
	}
	for directory, shard := range other {
		cShard := c[directory]
		if cShard == nil {
			continue
		}
		for key, value := range shard.entries {
			if cValue, ok := cShard.entries[key]; ok && cValue != value {
				return false
			}
		}
	}
	return true
}
