package repository

import (
	"testing"

	"github.com/google/uuid"

	"webtag/internal/model"
)

func TestSupersededTranslationAttemptCarriesWholeSourceIdentity(t *testing.T) {
	t.Parallel()

	revision := int64(17)
	riverJobID := int64(731)
	item := &model.LinkTranslation{
		ID:                    uuid.New(),
		SourceHash:            "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SourceContentRevision: &revision,
		Status:                model.TranslationStatusProcessing,
		AttemptGeneration:     9,
		CurrentRiverJobID:     &riverJobID,
	}

	got := supersededTranslationAttempt(item)
	if got == nil || got.TranslationID != item.ID ||
		got.AttemptGeneration != item.AttemptGeneration || got.RiverJobID != riverJobID ||
		got.SourceHash != item.SourceHash || got.SourceContentRevision == nil ||
		*got.SourceContentRevision != revision {
		t.Fatalf("supersededTranslationAttempt() = %+v, want complete identity from %+v", got, item)
	}
}
