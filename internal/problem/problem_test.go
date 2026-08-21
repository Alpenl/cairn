package problem

import (
	"errors"
	"testing"
)

func TestWrapPreservesCauseAndPublicMessage(t *testing.T) {
	t.Parallel()
	cause := errors.New("provider secret")
	err := Wrap(Upstream, "provider_failed", "provider unavailable", cause)
	if !errors.Is(err, cause) || err.Error() != "provider unavailable" || err.Code() != "provider_failed" || err.Kind() != Upstream {
		t.Fatalf("Wrap() = %#v", err)
	}
}

func TestConflictIdentityReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()
	revision := int64(3)
	err := NewWithCodeAndCurrentIdentity(Conflict, "stale", "changed", ConflictIdentity{ContentRevision: &revision})
	first, ok := err.CurrentIdentity()
	if !ok || first.ContentRevision == nil {
		t.Fatalf("CurrentIdentity() = %+v/%v", first, ok)
	}
	*first.ContentRevision = 99
	second, _ := err.CurrentIdentity()
	if second.ContentRevision == nil || *second.ContentRevision != 3 {
		t.Fatalf("stored identity mutated through returned pointer: %+v", second)
	}
}
