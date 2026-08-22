package service

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"webtag/internal/model"
)

type ReaderTrashPage struct {
	Items      []model.ReaderTrashItem
	Count      int
	NextCursor string
}

func (s *ReaderHostApplication) RestoreHost(
	ctx context.Context,
	kind model.ReaderHostKind,
	id uuid.UUID,
) (model.ReaderHostLifecycleResult, error) {
	if s.hostRestores == nil {
		return model.ReaderHostLifecycleResult{}, errors.New("reader host restore commands are not configured")
	}
	result, err := s.hostRestores.RestoreHost(ctx, kind, id)
	if err != nil {
		return model.ReaderHostLifecycleResult{}, mapReaderError(err)
	}
	return result, nil
}

func (s *ReaderHostApplication) PurgeHost(
	ctx context.Context,
	kind model.ReaderHostKind,
	id uuid.UUID,
	operationID uuid.UUID,
) error {
	return mapReaderError(s.hosts.PurgeHost(ctx, kind, id, operationID))
}

func (s *ReaderHostApplication) ListTrash(
	ctx context.Context,
	kind *model.ReaderHostKind,
	after string,
	limit int,
) (ReaderTrashPage, error) {
	items, count, next, err := s.hosts.ListTrash(ctx, kind, after, limit)
	if err != nil {
		return ReaderTrashPage{}, mapReaderError(err)
	}
	return ReaderTrashPage{Items: items, Count: count, NextCursor: next}, nil
}
