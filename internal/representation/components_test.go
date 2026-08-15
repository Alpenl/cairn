package representation_test

import (
	"slices"
	"testing"

	"webtag/internal/representation"
)

func TestComponentSetCanonicalizesKnownNames(t *testing.T) {
	t.Parallel()

	set, err := representation.NewComponentSet(
		representation.LibraryComponent,
		representation.GlobalComponent,
		representation.LibraryComponent,
		representation.FeedComponent,
	)
	if err != nil {
		t.Fatalf("NewComponentSet() error = %v", err)
	}
	want := []representation.ComponentName{
		representation.FeedComponent,
		representation.GlobalComponent,
		representation.LibraryComponent,
	}
	if got := set.Names(); !slices.Equal(got, want) {
		t.Fatalf("Names() = %#v, want %#v", got, want)
	}
	if got := set.Key(); got != "feed,global,library" {
		t.Fatalf("Key() = %q, want feed,global,library", got)
	}

	names := set.Names()
	names[0] = representation.LibraryComponent
	if got := set.Names(); !slices.Equal(got, want) {
		t.Fatalf("caller mutation changed component set to %#v", got)
	}
}

func TestComponentSetRejectsUnknownName(t *testing.T) {
	t.Parallel()
	if _, err := representation.NewComponentSet(representation.ComponentName("tenant")); err == nil {
		t.Fatal("NewComponentSet(tenant) error = nil after tenant vocabulary removal")
	}
}
