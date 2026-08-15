package representation_test

import (
	"context"
	"encoding/base64"
	"slices"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/representation"
)

func protocolBase(revision int64) representation.LibraryBase {
	return representation.LibraryBase{
		RepresentationNamespace: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Revision:                revision,
	}
}

func TestRepresentationContractIsV3(t *testing.T) {
	t.Parallel()
	if representation.Contract != "v3" {
		t.Fatalf("representation contract = %q, want v3", representation.Contract)
	}
}

func TestRepresentationVersionContextRoundTripRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	version, err := representation.NewLibraryVersion(protocolBase(3))
	if err != nil {
		t.Fatalf("NewLibraryVersion: %v", err)
	}
	ctx := representation.WithVersion(context.Background(), version)
	version.Components[0].Revision = 99
	got, ok := representation.FromContext(ctx)
	if !ok || got.ClientDataNamespace != version.ClientDataNamespace {
		t.Fatalf("context version = %#v, ok=%v; want namespace %q", got, ok, version.ClientDataNamespace)
	}
	if got.Components[0].Revision != 3 {
		t.Fatalf("caller mutation changed stored context revision to %d", got.Components[0].Revision)
	}
	got.Components[0].Revision = 100
	again, ok := representation.FromContext(ctx)
	if !ok || again.Components[0].Revision != 3 {
		t.Fatalf("returned-slice mutation changed context version to %#v", again)
	}
	if _, ok := representation.FromContext(representation.WithVersion(context.Background(), representation.RepresentationVersion{})); ok {
		t.Fatal("invalid representation version was installed in context")
	}
}

func TestInstallationNamespaceProtocolVector(t *testing.T) {
	t.Parallel()
	version, err := representation.NewLibraryVersion(protocolBase(7))
	if err != nil {
		t.Fatalf("NewLibraryVersion: %v", err)
	}
	const wantNamespace = "2HxUPnKG6geqoeBnKUkzVP8bd_aYcg3vseL8scuNm2U"
	if version.ClientDataNamespace != wantNamespace {
		t.Fatalf("client namespace = %q, want protocol vector %q", version.ClientDataNamespace, wantNamespace)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(version.ClientDataNamespace)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("client namespace must be an untruncated SHA-256 digest: bytes=%d err=%v", len(decoded), err)
	}
	wantComponents := []representation.Component{{Name: representation.LibraryComponent, Revision: 7}}
	if !slices.Equal(version.Components, wantComponents) {
		t.Fatalf("components = %#v, want %#v", version.Components, wantComponents)
	}
}

func TestClientDataNamespaceDependsOnlyOnInstallation(t *testing.T) {
	t.Parallel()
	baseline, err := representation.NewLibraryVersion(protocolBase(0))
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	revisionOnly, err := representation.NewLibraryVersion(protocolBase(99))
	if err != nil {
		t.Fatalf("revision-only version: %v", err)
	}
	if revisionOnly.ClientDataNamespace != baseline.ClientDataNamespace {
		t.Fatal("library revision changed the stable installation namespace")
	}
	if revisionOnly.Components[0].Revision == baseline.Components[0].Revision {
		t.Fatal("library revision did not change the representation component")
	}

	other := protocolBase(0)
	other.RepresentationNamespace = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	otherVersion, err := representation.NewLibraryVersion(other)
	if err != nil {
		t.Fatalf("other installation version: %v", err)
	}
	if otherVersion.ClientDataNamespace == baseline.ClientDataNamespace {
		t.Fatal("different installations shared a client data namespace")
	}
}

func TestRepresentationVersionCanonicalJSONSortsComponentsWithoutMutation(t *testing.T) {
	t.Parallel()
	components := []representation.Component{
		{Name: representation.LibraryComponent, Revision: 7},
		{Name: representation.FeedComponent, Revision: 2},
		{Name: representation.GlobalComponent, Revision: 4},
	}
	version := representation.RepresentationVersion{
		ClientDataNamespace: "2HxUPnKG6geqoeBnKUkzVP8bd_aYcg3vseL8scuNm2U",
		Components:          components,
	}
	encoded, err := version.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	const want = `{"client_data_namespace":"2HxUPnKG6geqoeBnKUkzVP8bd_aYcg3vseL8scuNm2U","components":[{"name":"feed","revision":2},{"name":"global","revision":4},{"name":"library","revision":7}]}`
	if string(encoded) != want {
		t.Fatalf("canonical JSON = %s, want %s", encoded, want)
	}
	if components[0].Name != representation.LibraryComponent || components[1].Name != representation.FeedComponent {
		t.Fatalf("CanonicalJSON mutated caller components: %#v", components)
	}
}

func TestRepresentationVersionRejectsInvalidOrAmbiguousInputs(t *testing.T) {
	t.Parallel()
	validNamespace := "2HxUPnKG6geqoeBnKUkzVP8bd_aYcg3vseL8scuNm2U"
	for _, tc := range []struct {
		name    string
		version representation.RepresentationVersion
	}{
		{name: "empty namespace", version: representation.RepresentationVersion{Components: []representation.Component{{Name: representation.LibraryComponent}}}},
		{name: "truncated namespace", version: representation.RepresentationVersion{ClientDataNamespace: "short", Components: []representation.Component{{Name: representation.LibraryComponent}}}},
		{name: "empty components", version: representation.RepresentationVersion{ClientDataNamespace: validNamespace}},
		{name: "tenant component", version: representation.RepresentationVersion{ClientDataNamespace: validNamespace, Components: []representation.Component{{Name: "tenant"}}}},
		{name: "negative revision", version: representation.RepresentationVersion{ClientDataNamespace: validNamespace, Components: []representation.Component{{Name: representation.LibraryComponent, Revision: -1}}}},
		{name: "duplicate component", version: representation.RepresentationVersion{ClientDataNamespace: validNamespace, Components: []representation.Component{{Name: representation.LibraryComponent}, {Name: representation.LibraryComponent, Revision: 1}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tc.version.CanonicalJSON(); err == nil {
				t.Fatal("CanonicalJSON accepted invalid representation version")
			}
		})
	}
}
