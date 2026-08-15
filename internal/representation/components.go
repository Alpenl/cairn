package representation

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ComponentName identifies one independently versioned representation dependency.
type ComponentName string

const (
	// LibraryComponent versions installation-owned library data.
	LibraryComponent ComponentName = "library"
	// GlobalComponent versions shared concept/display data.
	GlobalComponent ComponentName = "global"
	// FeedComponent versions feed projections independently from the library.
	FeedComponent ComponentName = "feed"
)

// ErrInvalidComponentSet reports an unknown representation dependency name.
var ErrInvalidComponentSet = errors.New("representation: invalid component set")

// Valid reports whether the component name belongs to the v2 vocabulary.
func (n ComponentName) Valid() bool {
	switch n {
	case LibraryComponent, GlobalComponent, FeedComponent:
		return true
	default:
		return false
	}
}

// ComponentSet is an immutable, canonical set of representation dependencies.
// Its zero value is the empty set used for identity namespace lookups.
type ComponentSet struct {
	names []ComponentName
}

// NewComponentSet validates, sorts, and deduplicates component names.
func NewComponentSet(names ...ComponentName) (ComponentSet, error) {
	canonical := slices.Clone(names)
	for _, name := range canonical {
		if !name.Valid() {
			return ComponentSet{}, fmt.Errorf("%w: %q", ErrInvalidComponentSet, name)
		}
	}
	slices.Sort(canonical)
	canonical = slices.Compact(canonical)
	return ComponentSet{names: canonical}, nil
}

// Names returns a caller-owned canonical component-name slice.
func (s ComponentSet) Names() []ComponentName {
	return slices.Clone(s.names)
}

// Key returns the stable component-set key used by caches and repositories.
func (s ComponentSet) Key() string {
	names := make([]string, len(s.names))
	for i, name := range s.names {
		names[i] = string(name)
	}
	return strings.Join(names, ",")
}

func (s ComponentSet) matches(components []Component) bool {
	if len(components) != len(s.names) {
		return false
	}
	canonical := slices.Clone(components)
	slices.SortFunc(canonical, func(left, right Component) int {
		return strings.Compare(string(left.Name), string(right.Name))
	})
	for i, component := range canonical {
		if !component.Name.Valid() || component.Revision < 0 || component.Name != s.names[i] {
			return false
		}
	}
	return true
}
