package core

import (
	"errors"
	"strings"
)

// Apply applies a series of changes to a base entry. It ignores the Old value
// for changes and only fails if the path to a change can't be resolved. The
// returned entry shares unmodified subtrees with the base (and with the New
// entries of the applied changes) by pointer, relying on the immutability of
// Entry objects: only the nodes along the paths to changes are freshly
// allocated, so the cost of Apply scales with the change list, not the tree.
func Apply(base *Entry, changes []*Change) (*Entry, error) {
	// If there are no changes, then we can just return the base unmodified.
	if len(changes) == 0 {
		return base, nil
	}

	// If there's only a single change and it's a root replacement, then we can
	// just return the new entry.
	if len(changes) == 1 && changes[0].Path == "" {
		return changes[0].New, nil
	}

	// Track the nodes allocated by this call. These are the only nodes that
	// may be mutated: every other node is shared with the base or with a
	// change's New entry and is immutable.
	fresh := make(map[*Entry]bool)

	// copyNode creates a mutable shallow copy of a node, with a fresh contents
	// map that shares the children by pointer.
	copyNode := func(entry *Entry) *Entry {
		result := shallowCopyEntry(entry)
		fresh[result] = true
		return result
	}

	// Apply changes.
	result := base
	for _, change := range changes {
		// Handle the special case of a root replacement. The new root is
		// shared by pointer: if any subsequent change descends into it, the
		// spine-copying below will allocate fresh nodes as needed.
		if change.Path == "" {
			result = change.New
			continue
		}

		// Crawl down the tree until we reach the parent of the target
		// location, making each node along the spine fresh (and thus mutable)
		// before descending below it.
		if result == nil {
			return nil, errors.New("unable to resolve parent path")
		}
		if !fresh[result] {
			result = copyNode(result)
		}
		parent := result
		components := strings.Split(change.Path, "/")
		for len(components) > 1 {
			child, ok := parent.Contents[components[0]]
			if !ok {
				return nil, errors.New("unable to resolve parent path")
			}
			if !fresh[child] {
				child = copyNode(child)
				parent.Contents[components[0]] = child
			}
			parent = child
			components = components[1:]
		}

		// Depending on the new value, either set or remove the entry. New
		// entries are shared by pointer: any subsequent change that descends
		// into one will spine-copy within it rather than mutate it.
		if change.New == nil {
			delete(parent.Contents, components[0])
		} else {
			if parent.Contents == nil {
				parent.Contents = make(map[string]*Entry)
			}
			parent.Contents[components[0]] = change.New
		}
	}

	// Done.
	return result, nil
}
