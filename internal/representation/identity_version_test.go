package representation_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/representation"
)

func TestVersionConstructionSeparatesIdentityFromSelectedComponents(t *testing.T) {
	t.Parallel()

	librarySet, err := representation.NewComponentSet(representation.LibraryComponent)
	if err != nil {
		t.Fatalf("NewComponentSet(library) error = %v", err)
	}
	identityBase := representation.VersionBase{
		RepresentationNamespace: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
	}
	clientIdentity, err := representation.NewClientIdentity(identityBase)
	if err != nil {
		t.Fatalf("NewClientIdentity() error = %v", err)
	}

	version, err := representation.NewVersion(representation.VersionBase{
		RepresentationNamespace: identityBase.RepresentationNamespace,
		Components: []representation.Component{{
			Name:     representation.LibraryComponent,
			Revision: 7,
		}},
	}, librarySet, clientIdentity)
	if err != nil {
		t.Fatalf("NewVersion() error = %v", err)
	}
	if version.ClientDataNamespace != clientIdentity.ClientDataNamespace {
		t.Fatalf("version namespace = %q, want %q", version.ClientDataNamespace, clientIdentity.ClientDataNamespace)
	}
	if len(version.Components) != 1 || version.Components[0].Name != representation.LibraryComponent || version.Components[0].Revision != 7 {
		t.Fatalf("version components = %#v, want library revision 7", version.Components)
	}

	globalSet, err := representation.NewComponentSet(representation.GlobalComponent)
	if err != nil {
		t.Fatalf("NewComponentSet(global) error = %v", err)
	}
	_, err = representation.NewVersion(representation.VersionBase{
		RepresentationNamespace: identityBase.RepresentationNamespace,
		Components: []representation.Component{{
			Name:     representation.LibraryComponent,
			Revision: 7,
		}},
	}, globalSet, clientIdentity)
	if !errors.Is(err, representation.ErrInvalidVersion) {
		t.Fatalf("NewVersion(mismatched set) error = %v, want ErrInvalidVersion", err)
	}
}

func TestVersionRejectsDifferentInstallationIdentity(t *testing.T) {
	t.Parallel()
	librarySet, _ := representation.NewComponentSet(representation.LibraryComponent)
	identity, err := representation.NewClientIdentity(representation.VersionBase{
		RepresentationNamespace: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
	})
	if err != nil {
		t.Fatalf("NewClientIdentity: %v", err)
	}
	_, err = representation.NewVersion(representation.VersionBase{
		RepresentationNamespace: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Components:              []representation.Component{{Name: representation.LibraryComponent, Revision: 1}},
	}, librarySet, identity)
	if !errors.Is(err, representation.ErrInvalidVersion) {
		t.Fatalf("error = %v, want ErrInvalidVersion", err)
	}
}
