package handler

import (
	"context"
	"strings"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/service"
)

type readerHostApplicationRoutes struct {
	application *service.ReaderHostApplication
}

func NewReaderHostRoutes(application *service.ReaderHostApplication) ReaderHostRoutes {
	if application == nil {
		return nil
	}
	return &readerHostApplicationRoutes{application: application}
}

func (r *readerHostApplicationRoutes) RestoreHost(ctx context.Context, rawKind, rawID string) (dto.ReaderHostLifecycleResponse, error) {
	kind, err := parseReaderHostKind(rawKind)
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	id, err := parseReaderUUID(rawID, string(kind)+"_id")
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	result, err := r.application.RestoreHost(ctx, kind, id)
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	return readerHostLifecycleResponse(result), nil
}

func (r *readerHostApplicationRoutes) PurgeHost(ctx context.Context, rawKind, rawID string, request dto.ReaderHostPurgeRequest) error {
	kind, err := parseReaderHostKind(rawKind)
	if err != nil {
		return err
	}
	id, err := parseReaderUUID(rawID, string(kind)+"_id")
	if err != nil {
		return err
	}
	operationID, err := parseReaderUUID(request.OperationID, "operation_id")
	if err != nil {
		return err
	}
	return r.application.PurgeHost(ctx, kind, id, operationID)
}

func (r *readerHostApplicationRoutes) ListTrash(ctx context.Context, rawKind, after string, limit int) (dto.ReaderTrashResponse, error) {
	var kind *model.ReaderHostKind
	if strings.TrimSpace(rawKind) != "" {
		parsed, err := parseReaderHostKind(rawKind)
		if err != nil {
			return dto.ReaderTrashResponse{}, err
		}
		kind = &parsed
	}
	page, err := r.application.ListTrash(ctx, kind, after, limit)
	if err != nil {
		return dto.ReaderTrashResponse{}, err
	}
	response := dto.ReaderTrashResponse{Items: make([]dto.ReaderTrashItemResponse, 0, len(page.Items)), Count: page.Count, NextCursor: page.NextCursor}
	for _, item := range page.Items {
		response.Items = append(response.Items, dto.ReaderTrashItemResponse{
			HostKind: string(item.HostKind), HostID: item.HostID.String(), Title: item.Title,
			URL: item.URL, TrashedAt: item.TrashedAt,
		})
	}
	return response, nil
}

func parseReaderHostKind(raw string) (model.ReaderHostKind, error) {
	kind := model.ReaderHostKind(strings.TrimSpace(raw))
	if !kind.Valid() {
		return "", problem.NewWithCode(problem.Invalid, "invalid_host_kind", "host_kind must be link, inbox, or note")
	}
	return kind, nil
}

func readerHostLifecycleResponse(result model.ReaderHostLifecycleResult) dto.ReaderHostLifecycleResponse {
	return dto.ReaderHostLifecycleResponse{
		HostKind: string(result.HostKind), HostID: result.HostID.String(), State: string(result.State), Changed: result.Changed,
	}
}

var _ ReaderHostRoutes = (*readerHostApplicationRoutes)(nil)
