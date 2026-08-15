package representation

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
)

const (
	// Contract identifies the single-installation representation wire contract.
	Contract = "v3"
	// DataNamespaceHeader binds responses to the installation data partition.
	DataNamespaceHeader   = "X-WebTag-Data-Namespace"
	clientNamespaceDomain = "cairn-installation-data-v1"
)

var ErrInvalidVersion = errors.New("representation: invalid version")

// Component names one independently versioned representation dependency.
type Component struct {
	Name     ComponentName `json:"name"`
	Revision int64         `json:"revision"`
}

// ClientIdentity identifies the one persistent data namespace owned by this
// installation. Authentication credentials deliberately do not participate in
// its derivation, so static-token, session, and explicitly open requests share
// the same client cache and data partition.
type ClientIdentity struct {
	ClientDataNamespace     string
	RepresentationNamespace uuid.UUID
}

// RepresentationVersion is the complete data-version input for validators and
// response caches.
type RepresentationVersion struct {
	ClientDataNamespace string      `json:"client_data_namespace"`
	Components          []Component `json:"components"`
}

type versionContextKey struct{}
type clientIdentityContextKey struct{}

// NewClientIdentity derives the installation namespace from an identity-only
// base. Bases containing route-specific components are rejected.
func NewClientIdentity(base VersionBase) (ClientIdentity, error) {
	if !base.ValidFor(ComponentSet{}) {
		return ClientIdentity{}, fmt.Errorf("%w: identity base", ErrInvalidVersion)
	}
	return ClientIdentity{
		ClientDataNamespace:     deriveClientDataNamespace(base.RepresentationNamespace),
		RepresentationNamespace: base.RepresentationNamespace,
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

func WithVersion(ctx context.Context, version RepresentationVersion) context.Context {
	if !version.Valid() {
		return ctx
	}
	return context.WithValue(ctx, versionContextKey{}, cloneVersion(version))
}

func FromContext(ctx context.Context) (RepresentationVersion, bool) {
	version, ok := ctx.Value(versionContextKey{}).(RepresentationVersion)
	if !ok || !version.Valid() {
		return RepresentationVersion{}, false
	}
	return cloneVersion(version), true
}

func cloneVersion(version RepresentationVersion) RepresentationVersion {
	version.Components = slices.Clone(version.Components)
	return version
}

func (v RepresentationVersion) Valid() bool {
	return v.validate() == nil
}

// NewVersion combines the installation namespace with exactly the component
// vector selected by route policy.
func NewVersion(base VersionBase, selected ComponentSet, identity ClientIdentity) (RepresentationVersion, error) {
	if selected.Key() == "" || !identity.Valid() || !base.ValidFor(selected) ||
		base.RepresentationNamespace != identity.RepresentationNamespace {
		return RepresentationVersion{}, ErrInvalidVersion
	}
	version := RepresentationVersion{
		ClientDataNamespace: identity.ClientDataNamespace,
		Components:          slices.Clone(base.Components),
	}
	if err := version.validate(); err != nil {
		return RepresentationVersion{}, err
	}
	return version, nil
}

// NewLibraryVersion attaches the authoritative library revision to the stable
// installation namespace.
func NewLibraryVersion(base LibraryBase) (RepresentationVersion, error) {
	if !base.Valid() {
		return RepresentationVersion{}, fmt.Errorf("%w: library base", ErrInvalidVersion)
	}
	clientIdentity, err := NewClientIdentity(VersionBase{RepresentationNamespace: base.RepresentationNamespace})
	if err != nil {
		return RepresentationVersion{}, err
	}
	librarySet, err := NewComponentSet(LibraryComponent)
	if err != nil {
		return RepresentationVersion{}, err
	}
	return NewVersion(VersionBase{
		RepresentationNamespace: base.RepresentationNamespace,
		Components: []Component{{
			Name:     LibraryComponent,
			Revision: base.Revision,
		}},
	}, librarySet, clientIdentity)
}

func deriveClientDataNamespace(representationNamespace uuid.UUID) string {
	digest := sha256.Sum256([]byte(clientNamespaceDomain + "\x00" + representationNamespace.String()))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (v RepresentationVersion) CanonicalJSON() ([]byte, error) {
	if err := v.validate(); err != nil {
		return nil, err
	}
	canonical := RepresentationVersion{
		ClientDataNamespace: v.ClientDataNamespace,
		Components:          slices.Clone(v.Components),
	}
	slices.SortFunc(canonical.Components, func(left, right Component) int {
		return strings.Compare(string(left.Name), string(right.Name))
	})
	return json.Marshal(canonical)
}

func (v RepresentationVersion) validate() error {
	if !validClientDataNamespace(v.ClientDataNamespace) {
		return fmt.Errorf("%w: client data namespace", ErrInvalidVersion)
	}
	if len(v.Components) == 0 {
		return fmt.Errorf("%w: no components", ErrInvalidVersion)
	}
	seen := make(map[ComponentName]struct{}, len(v.Components))
	for _, component := range v.Components {
		if !component.Name.Valid() {
			return fmt.Errorf("%w: invalid component name %q", ErrInvalidVersion, component.Name)
		}
		if component.Revision < 0 {
			return fmt.Errorf("%w: negative %s revision", ErrInvalidVersion, component.Name)
		}
		if _, exists := seen[component.Name]; exists {
			return fmt.Errorf("%w: duplicate component %q", ErrInvalidVersion, component.Name)
		}
		seen[component.Name] = struct{}{}
	}
	return nil
}

func validClientDataNamespace(namespace string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(namespace)
	return err == nil && len(decoded) == sha256.Size
}
