// Package representation defines the installation identity shared by
// authenticated requests and client-side data partitioning.
package representation

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"github.com/google/uuid"
)

const (
	// Contract identifies the single-installation representation wire contract.
	Contract = "v3"
	// DataNamespaceHeader binds responses to the installation data partition.
	DataNamespaceHeader   = "X-WebTag-Data-Namespace"
	clientNamespaceDomain = "cairn-installation-data-v1"
)

// ErrInvalidIdentity means that an installation namespace cannot safely be
// used to partition client data.
var ErrInvalidIdentity = errors.New("representation: invalid installation identity")

// ClientIdentity identifies the one persistent data namespace owned by this
// installation. Authentication credentials deliberately do not participate in
// its derivation, so static-token, session, and explicitly open requests share
// the same client cache and data partition.
type ClientIdentity struct {
	ClientDataNamespace     string
	RepresentationNamespace uuid.UUID
}

type clientIdentityContextKey struct{}

// NewClientIdentity derives the stable client namespace from the database-owned
// installation namespace.
func NewClientIdentity(representationNamespace uuid.UUID) (ClientIdentity, error) {
	if representationNamespace == uuid.Nil {
		return ClientIdentity{}, ErrInvalidIdentity
	}
	return ClientIdentity{
		ClientDataNamespace:     deriveClientDataNamespace(representationNamespace),
		RepresentationNamespace: representationNamespace,
	}, nil
}

func (i ClientIdentity) Valid() bool {
	return i.RepresentationNamespace != uuid.Nil && validClientDataNamespace(i.ClientDataNamespace)
}

func WithClientIdentity(ctx context.Context, identity ClientIdentity) context.Context {
	if !identity.Valid() {
		return ctx
	}
	return context.WithValue(ctx, clientIdentityContextKey{}, identity)
}

func ClientIdentityFromContext(ctx context.Context) (ClientIdentity, bool) {
	identity, ok := ctx.Value(clientIdentityContextKey{}).(ClientIdentity)
	return identity, ok && identity.Valid()
}

func deriveClientDataNamespace(representationNamespace uuid.UUID) string {
	digest := sha256.Sum256([]byte(clientNamespaceDomain + "\x00" + representationNamespace.String()))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func validClientDataNamespace(namespace string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(namespace)
	return err == nil && len(decoded) == sha256.Size
}
