package handler

import (
	"context"
	"encoding/json"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
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
		op, err := readerThoughtOpCommand(input)
		if err != nil {
			return nil, err
		}
		command.Ops = append(command.Ops, op)
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

func readerThoughtOpCommand(input dto.ReaderThoughtOpRequest) (service.ReaderThoughtOpCommand, error) {
	command := service.ReaderThoughtOpCommand{
		ContractVersion: input.ContractVersion,
		OpID:            input.OpID, DeviceID: input.DeviceID, LogicalClock: input.LogicalClock,
		OperationKind: input.OperationKind, AnnotationID: input.AnnotationID,
		HostKind: input.HostKind, HostID: input.HostID,
		TargetJSON: append([]byte(nil), input.Target...), PayloadJSON: append([]byte(nil), input.Payload...),
		RecoveryOf:               readerThoughtVersionKeyModel(input.RecoveryOf),
		ExpectedCurrentWinnerKey: readerThoughtVersionKeyModel(input.ExpectedCurrentWinnerKey),
	}
	if !json.Valid(input.Target) || !json.Valid(input.Payload) {
		return service.ReaderThoughtOpCommand{}, problem.NewWithCode(problem.Invalid, "invalid_thought_payload", "thought target and payload must be JSON")
	}
	target, err := readerThoughtTargetCommand(input)
	if err != nil {
		return service.ReaderThoughtOpCommand{}, err
	}
	payload, err := readerThoughtPayloadCommand(input, target)
	if err != nil {
		return service.ReaderThoughtOpCommand{}, err
	}
	command.Target, command.Payload = target, payload
	return command, nil
}

type readerThoughtTargetPayload struct {
	Kind    string `json:"kind"`
	HostID  string `json:"host_id"`
	Version struct {
		ContentRevision  int64  `json:"content_revision"`
		SourceHash       string `json:"source_hash"`
		NoteRevision     int64  `json:"note_revision"`
		MetadataRevision int64  `json:"metadata_revision"`
	} `json:"version"`
}

func readerThoughtTargetCommand(input dto.ReaderThoughtOpRequest) (service.ReaderThoughtTarget, error) {
	var target readerThoughtTargetPayload
	if err := json.Unmarshal(input.Target, &target); err != nil {
		return service.ReaderThoughtTarget{}, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "thought target must be an object")
	}
	return service.ReaderThoughtTarget{
		Kind:             target.Kind,
		HostID:           target.HostID,
		ContentRevision:  target.Version.ContentRevision,
		SourceHash:       target.Version.SourceHash,
		NoteRevision:     target.Version.NoteRevision,
		MetadataRevision: target.Version.MetadataRevision,
	}, nil
}

func invalidThoughtReattachPayload() error {
	return problem.NewWithCode(problem.Invalid, "invalid_thought_payload", "reattach operation payload is invalid")
}

func readerThoughtPayloadCommand(input dto.ReaderThoughtOpRequest, target service.ReaderThoughtTarget) (service.ReaderThoughtPayload, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input.Payload, &fields); err != nil {
		return service.ReaderThoughtPayload{}, invalidThoughtReattachPayload()
	}
	if raw, ok := fields["reattach"]; ok {
		reattach, err := parseThoughtReattachCommand(raw)
		if err != nil {
			return service.ReaderThoughtPayload{}, err
		}
		if input.OperationKind != "update" || len(fields) != 1 ||
			input.RecoveryOf != nil || input.ExpectedCurrentWinnerKey != nil {
			return service.ReaderThoughtPayload{}, invalidThoughtReattachPayload()
		}
		if !thoughtReattachTargetMatches(input, target, reattach.ExpectedHostRevision) {
			return service.ReaderThoughtPayload{}, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "reattach target must match the destination host revision")
		}
		return service.ReaderThoughtPayload{Reattach: &reattach, ReattachOnly: true}, nil
	}
	var payload struct {
		Quote json.RawMessage `json:"quote"`
	}
	if err := json.Unmarshal(input.Payload, &payload); err != nil {
		return service.ReaderThoughtPayload{}, problem.NewWithCode(problem.Invalid, "invalid_thought_payload", "thought payload must be an object")
	}
	if len(payload.Quote) == 0 {
		if input.OperationKind == "delete" {
			return service.ReaderThoughtPayload{}, nil
		}
		return service.ReaderThoughtPayload{}, problem.NewWithCode(problem.Invalid, "invalid_thought_payload", "thought payload requires a JSON quote")
	}
	if !json.Valid(payload.Quote) {
		return service.ReaderThoughtPayload{}, problem.NewWithCode(problem.Invalid, "invalid_thought_payload", "thought payload quote must be valid JSON")
	}
	return service.ReaderThoughtPayload{HasQuote: true}, nil
}

func parseThoughtReattachCommand(raw json.RawMessage) (model.ReaderThoughtReattachOperation, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 2 {
		return model.ReaderThoughtReattachOperation{}, invalidThoughtReattachPayload()
	}
	rawSequence, hasSequence := fields["expected_last_sequence"]
	rawRevision, hasRevision := fields["expected_host_revision"]
	if !hasSequence || !hasRevision {
		return model.ReaderThoughtReattachOperation{}, invalidThoughtReattachPayload()
	}
	var command model.ReaderThoughtReattachOperation
	if json.Unmarshal(rawSequence, &command.ExpectedLastSequence) != nil ||
		json.Unmarshal(rawRevision, &command.ExpectedHostRevision) != nil ||
		command.ExpectedLastSequence < 0 || command.ExpectedHostRevision <= 0 {
		return model.ReaderThoughtReattachOperation{}, invalidThoughtReattachPayload()
	}
	return command, nil
}

func thoughtReattachTargetMatches(input dto.ReaderThoughtOpRequest, target service.ReaderThoughtTarget, revision int64) bool {
	switch input.HostKind {
	case "link":
		return target.Kind == "saved-content" && target.ContentRevision == revision
	case "note":
		return target.Kind == "note" && target.NoteRevision == revision
	case "inbox":
		return target.Kind == "inbox" && target.MetadataRevision == revision
	default:
		return false
	}
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
