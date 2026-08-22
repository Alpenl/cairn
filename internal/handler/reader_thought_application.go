package handler

import (
	"context"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/service"
)

type readerThoughtApplicationRoutes struct {
	application *service.ReaderThoughtApplication
}

func NewReaderThoughtRoutes(application *service.ReaderThoughtApplication) ReaderThoughtRoutes {
	if application == nil {
		return nil
	}
	return &readerThoughtApplicationRoutes{application: application}
}

func (r *readerThoughtApplicationRoutes) PushThoughtOps(ctx context.Context, request dto.ReaderThoughtOpsRequest) ([]dto.ReaderThoughtAckResponse, error) {
	command := service.ReaderThoughtOpsCommand{Ops: make([]service.ReaderThoughtOpCommand, 0, len(request.Ops))}
	for _, input := range request.Ops {
		command.Ops = append(command.Ops, service.ReaderThoughtOpCommand{
			ContractVersion: input.ContractVersion,
			OpID:            input.OpID, DeviceID: input.DeviceID, LogicalClock: input.LogicalClock,
			OperationKind: input.OperationKind, AnnotationID: input.AnnotationID,
			HostKind: input.HostKind, HostID: input.HostID,
			Target: input.Target, Payload: input.Payload,
			RecoveryOf:               readerThoughtVersionKeyModel(input.RecoveryOf),
			ExpectedCurrentWinnerKey: readerThoughtVersionKeyModel(input.ExpectedCurrentWinnerKey),
		})
	}
	acks, err := r.application.PushThoughtOps(ctx, command)
	if err != nil {
		return nil, err
	}
	response := make([]dto.ReaderThoughtAckResponse, 0, len(acks))
	for _, ack := range acks {
		response = append(response, dto.ReaderThoughtAckResponse{
			ContractVersion: model.ReaderThoughtContractVersion,
			OpID:            ack.OpID, Sequence: ack.Sequence, Disposition: ack.Disposition,
			SubmittedKey:     readerThoughtVersionKeyResponse(ack.SubmittedKey),
			CurrentWinnerKey: readerThoughtVersionKeyResponse(ack.WinnerKey),
		})
	}
	return response, nil
}

func (r *readerThoughtApplicationRoutes) ListThoughts(ctx context.Context, query, after string, limit int) (dto.ReaderThoughtsResponse, error) {
	page, err := r.application.ListThoughts(ctx, query, after, limit)
	return readerThoughtPageResponse(page, err)
}

func (r *readerThoughtApplicationRoutes) ListThoughtHistory(ctx context.Context, after string, limit int) (dto.ReaderThoughtsResponse, error) {
	page, err := r.application.ListThoughtHistory(ctx, after, limit)
	return readerThoughtPageResponse(page, err)
}

func (r *readerThoughtApplicationRoutes) SyncThoughts(ctx context.Context, after string, limit int) (dto.ReaderThoughtsResponse, error) {
	page, err := r.application.SyncThoughts(ctx, after, limit)
	return readerThoughtPageResponse(page, err)
}

func (r *readerThoughtApplicationRoutes) ListThoughtConflicts(ctx context.Context, after string, limit int) (dto.ReaderThoughtConflictsResponse, error) {
	page, err := r.application.ListThoughtConflicts(ctx, after, limit)
	if err != nil {
		return dto.ReaderThoughtConflictsResponse{}, err
	}
	response := dto.ReaderThoughtConflictsResponse{
		ContractVersion: model.ReaderThoughtContractVersion,
		Items:           make([]dto.ReaderThoughtConflictResponse, 0, len(page.Items)),
		NextCursor:      page.NextCursor,
	}
	for _, item := range page.Items {
		response.Items = append(response.Items, dto.ReaderThoughtConflictResponse{
			Sequence: item.Sequence, AnnotationID: item.AnnotationID,
			WinnerAtDetection: readerThoughtConflictOperationResponse(item.Winner),
			Loser:             readerThoughtConflictOperationResponse(item.Loser),
		})
	}
	return response, nil
}

func (r *readerThoughtApplicationRoutes) GetThought(ctx context.Context, id string) (dto.ReaderThoughtResponse, error) {
	item, err := r.application.GetThought(ctx, id)
	if err != nil {
		return dto.ReaderThoughtResponse{}, err
	}
	return readerThoughtResponse(item), nil
}

func readerThoughtPageResponse(page service.ReaderThoughtPage, err error) (dto.ReaderThoughtsResponse, error) {
	if err != nil {
		return dto.ReaderThoughtsResponse{}, err
	}
	response := dto.ReaderThoughtsResponse{
		ContractVersion: model.ReaderThoughtContractVersion,
		Items:           make([]dto.ReaderThoughtResponse, 0, len(page.Items)),
		NextCursor:      page.NextCursor,
	}
	for _, item := range page.Items {
		response.Items = append(response.Items, readerThoughtResponse(item))
	}
	return response, nil
}

func readerThoughtConflictOperationResponse(item model.ReaderThoughtConflictOperation) dto.ReaderThoughtConflictOperationResponse {
	response := dto.ReaderThoughtConflictOperationResponse{
		ContractVersion: model.ReaderThoughtContractVersion,
		Sequence:        item.Sequence, OpID: item.OpID, DeviceID: item.DeviceID,
		LogicalClock: item.LogicalClock, OperationKind: item.OperationKind,
		AnnotationID: item.AnnotationID, HostKind: item.HostKind, HostID: item.HostID,
		Target: item.Target, Payload: item.Payload, CreatedAt: item.CreatedAt,
	}
	if item.RecoveryOf != nil {
		value := readerThoughtVersionKeyResponse(*item.RecoveryOf)
		response.RecoveryOf = &value
	}
	if item.ExpectedWinnerKey != nil {
		value := readerThoughtVersionKeyResponse(*item.ExpectedWinnerKey)
		response.ExpectedCurrentWinnerKey = &value
	}
	return response
}

func readerThoughtResponse(item model.ReaderThought) dto.ReaderThoughtResponse {
	lifecycleStatus := item.LifecycleStatus
	if lifecycleStatus == "" {
		lifecycleStatus = "active"
	}
	response := dto.ReaderThoughtResponse{
		ContractVersion: model.ReaderThoughtContractVersion,
		ID:              item.ID, HostKind: item.HostKind, HostID: item.HostID,
		Target: item.Target, Quote: item.Quote, Body: item.Body, Source: item.Source,
		Deleted: item.Deleted, LastSequence: item.LastSequence,
		WinnerKey: readerThoughtVersionKeyResponse(item.WinnerKey),
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		LifecycleStatus: lifecycleStatus, LifecycleReason: item.LifecycleReason,
		TombstonedAt: item.TombstonedAt, OriginalHostSnapshot: item.OriginalHostSnapshot,
	}
	if item.LinkID != nil {
		id := item.LinkID.String()
		response.LinkID = &id
	}
	return response
}

func readerThoughtVersionKeyModel(key *dto.ReaderThoughtVersionKeyResponse) *model.ReaderThoughtVersionKey {
	if key == nil {
		return nil
	}
	return &model.ReaderThoughtVersionKey{LogicalClock: key.LogicalClock, DeviceID: key.DeviceID, OpID: key.OpID}
}

func readerThoughtVersionKeyResponse(key model.ReaderThoughtVersionKey) dto.ReaderThoughtVersionKeyResponse {
	return dto.ReaderThoughtVersionKeyResponse{LogicalClock: key.LogicalClock, DeviceID: key.DeviceID, OpID: key.OpID}
}

var _ ReaderThoughtRoutes = (*readerThoughtApplicationRoutes)(nil)
