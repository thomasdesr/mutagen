package core

import (
	"errors"
	"fmt"
)

// EnsureValid ensures that Cache's invariants are respected.
func (c *Cache) EnsureValid() error {
	// A nil cache is considered valid (though obviously that requires using
	// the GetEntries accessor).
	if c == nil {
		return errors.New("nil cache")
	}

	// Technically we could validate each path, but that's error prone,
	// expensive, and not really needed for memory safety. Also note that an
	// empty path is valid when the synchronization root is a file.

	// Nil cache entries are invalid.
	for _, e := range c.Entries {
		if e == nil {
			return errors.New("nil cache entry detected")
		} else if e.ModificationTime == nil {
			return errors.New("cache entry with nil modification time detected")
		} else if err := e.ModificationTime.CheckValid(); err != nil {
			return fmt.Errorf("cache entry modification time invalid: %w", err)
		}
	}

	// Success.
	return nil
}

// byteLookupMap is the interface implemented by all byteLookupMap types.
type byteLookupMap interface {
	// length returns the length of the map.
	length() int
	// insert adds a key-value pair to the map.
	insert(k []byte, v string)
	// find looks for a key in the map, returning the associated value
	// (defaulting to an empty string if the key was not present) and whether or
	// not the key was found.
	find(k []byte) (string, bool)
}

// ReverseLookupMap provides facilities for doing reverse lookups to avoid
// expensive staging operations in the case of renames and copies.
type ReverseLookupMap struct {
	// lookupMap is the underlying map.
	lookupMap byteLookupMap
}

// Length returns the number of entries in the map.
func (m *ReverseLookupMap) Length() int {
	return m.lookupMap.length()
}

// Lookup attempts a lookup in the map.
func (m *ReverseLookupMap) Lookup(digest []byte) (string, bool) {
	return m.lookupMap.find(digest)
}
