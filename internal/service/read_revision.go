package service

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sync/singleflight"

	"webtag/internal/representation"
)

// InstallationIdentityStore reads the one immutable namespace owned by the
// installation. It deliberately has no component or revision vocabulary:
// those values are not part of any current client contract.
type InstallationIdentityStore interface {
	Current(context.Context) (representation.ClientIdentity, error)
}

// InstallationIdentityService coalesces overlapping namespace reads without
// retaining a stale result across requests.
type InstallationIdentityService struct {
	store InstallationIdentityStore
	group singleflight.Group
}

const installationIdentityLoadTimeout = 30 * time.Second

func NewInstallationIdentityService(store InstallationIdentityStore) *InstallationIdentityService {
	return &InstallationIdentityService{store: store}
}

func (s *InstallationIdentityService) Current(ctx context.Context) (representation.ClientIdentity, error) {
	if s == nil || s.store == nil {
		return representation.ClientIdentity{}, errors.New("installation identity store is unavailable")
	}

	shared := s.group.DoChan("installation", func() (any, error) {
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), installationIdentityLoadTimeout)
		defer cancel()
		identity, err := s.store.Current(loadCtx)
		if err != nil {
			return nil, err
		}
		if !identity.Valid() {
			return nil, representation.ErrInvalidIdentity
		}
		return identity, nil
	})

	select {
	case <-ctx.Done():
		return representation.ClientIdentity{}, ctx.Err()
	case result := <-shared:
		if result.Err != nil {
			return representation.ClientIdentity{}, result.Err
		}
		identity, ok := result.Val.(representation.ClientIdentity)
		if !ok || !identity.Valid() {
			return representation.ClientIdentity{}, representation.ErrInvalidIdentity
		}
		return identity, nil
	}
}
