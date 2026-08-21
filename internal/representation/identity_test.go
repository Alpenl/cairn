package representation_test

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/representation"
)

func TestRepresentationContractIsV3(t *testing.T) {
	t.Parallel()
	if representation.Contract != "v3" {
		t.Fatalf("representation contract = %q, want v3", representation.Contract)
	}
}

func TestClientIdentityDerivationIsStableAndNamespaceOnly(t *testing.T) {
	t.Parallel()
	installation := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	identity, err := representation.NewClientIdentity(installation)
	if err != nil {
		t.Fatalf("NewClientIdentity() error = %v", err)
	}
	if identity.RepresentationNamespace != installation {
		t.Fatalf("representation namespace = %s, want %s", identity.RepresentationNamespace, installation)
	}
	const wantNamespace = "2HxUPnKG6geqoeBnKUkzVP8bd_aYcg3vseL8scuNm2U"
	if identity.ClientDataNamespace != wantNamespace {
		t.Fatalf("client namespace = %q, want protocol vector %q", identity.ClientDataNamespace, wantNamespace)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(identity.ClientDataNamespace)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("client namespace must be an untruncated SHA-256 digest: bytes=%d err=%v", len(decoded), err)
	}

	other, err := representation.NewClientIdentity(uuid.MustParse("22222222-2222-2222-2222-222222222222"))
	if err != nil {
		t.Fatalf("other identity: %v", err)
	}
	if other.ClientDataNamespace == identity.ClientDataNamespace {
		t.Fatal("different installations shared a client data namespace")
	}
}

func TestNewClientIdentityRejectsNilNamespace(t *testing.T) {
	t.Parallel()
	_, err := representation.NewClientIdentity(uuid.Nil)
	if !errors.Is(err, representation.ErrInvalidIdentity) {
		t.Fatalf("NewClientIdentity(nil) error = %v, want ErrInvalidIdentity", err)
	}
}

func TestClientIdentityContextRejectsInvalidAndPreservesIdentity(t *testing.T) {
	t.Parallel()
	identity, err := representation.NewClientIdentity(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	if err != nil {
		t.Fatalf("NewClientIdentity: %v", err)
	}
	ctx := representation.WithClientIdentity(context.Background(), identity)
	got, ok := representation.ClientIdentityFromContext(ctx)
	if !ok || got != identity {
		t.Fatalf("context identity = %#v, ok=%v; want %#v", got, ok, identity)
	}
	if _, ok := representation.ClientIdentityFromContext(
		representation.WithClientIdentity(context.Background(), representation.ClientIdentity{}),
	); ok {
		t.Fatal("invalid identity was installed in context")
	}
}
