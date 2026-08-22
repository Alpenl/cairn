package service

import (
	"context"
	"strings"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
)

// RestoreHost restores any supported polymorphic Reader host. Existing Note
// and Inbox methods delegate to the same store capability; this generic seam
// supplies Link restore and keeps lifecycle-specific transport code narrow.
func (s *ReaderVNextService) RestoreHost(ctx context.Context, rawKind, rawID string) (dto.ReaderHostLifecycleResponse, error) {
	kind, err := parseReaderHostKind(rawKind)
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	id, err := readerUUID(rawID, string(kind)+"_id")
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	result, err := s.hosts.RestoreHost(ctx, kind, id)
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, mapReaderError(err)
	}
	return readerHostLifecycleResponse(result), nil
}

func (s *ReaderVNextService) PurgeHost(ctx context.Context, rawKind, rawID string, request dto.ReaderHostPurgeRequest) error {
	kind, err := parseReaderHostKind(rawKind)
	if err != nil {
		return err
	}
	id, err := readerUUID(rawID, string(kind)+"_id")
	if err != nil {
		return err
	}
	operationID, err := readerUUID(request.OperationID, "operation_id")
	if err != nil {
		return err
	}
	return mapReaderError(s.hosts.PurgeHost(ctx, kind, id, operationID))
}

func (s *ReaderVNextService) ListTrash(ctx context.Context, rawKind, after string, limit int) (dto.ReaderTrashResponse, error) {
	var kind *model.ReaderHostKind
	if strings.TrimSpace(rawKind) != "" {
		parsed, err := parseReaderHostKind(rawKind)
		if err != nil {
			return dto.ReaderTrashResponse{}, err
		}
		kind = &parsed
	}
	items, count, next, err := s.hosts.ListTrash(ctx, kind, after, limit)
	if err != nil {
		return dto.ReaderTrashResponse{}, mapReaderError(err)
	}
	response := dto.ReaderTrashResponse{Items: make([]dto.ReaderTrashItemResponse, 0, len(items)), Count: count, NextCursor: next}
	for _, item := range items {
		response.Items = append(response.Items, dto.ReaderTrashItemResponse{
			HostKind: string(item.HostKind), HostID: item.HostID.String(), Title: item.Title, URL: item.URL, TrashedAt: item.TrashedAt,
		})
	}
	return response, nil
}

func parseReaderHostKind(raw string) (model.ReaderHostKind, error) {
	kind := model.ReaderHostKind(strings.TrimSpace(raw))
	if !kind.Valid() {
		return "", mapReaderError(repository.ErrInvalidReaderHostKind)
	}
	return kind, nil
}

func readerHostLifecycleResponse(result model.ReaderHostLifecycleResult) dto.ReaderHostLifecycleResponse {
	return dto.ReaderHostLifecycleResponse{
		HostKind: string(result.HostKind), HostID: result.HostID.String(), State: string(result.State), Changed: result.Changed,
	}
}
