package service

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
)

func (s *ReaderVNextService) PushThoughtOps(ctx context.Context, request dto.ReaderThoughtOpsRequest) ([]dto.ReaderThoughtAckResponse, error) {
	if len(request.Ops) == 0 || len(request.Ops) > 200 {
		return nil, problem.NewWithCode(problem.Invalid, "invalid_thought_ops", "thought operation batch must contain between 1 and 200 operations")
	}
	ops := make([]model.ReaderThoughtOp, 0, len(request.Ops))
	for _, input := range request.Ops {
		if err := validateThoughtWire(input); err != nil {
			return nil, err
		}
		reattach, err := readerThoughtReattachOperation(input)
		if err != nil {
			return nil, err
		}
		ops = append(ops, model.ReaderThoughtOp{
			OpID: input.OpID, DeviceID: input.DeviceID, LogicalClock: input.LogicalClock,
			OperationKind: input.OperationKind, AnnotationID: input.AnnotationID,
			HostKind: input.HostKind, HostID: input.HostID, Target: input.Target,
			Payload: input.Payload, RecoveryOf: thoughtVersionKeyFromRequest(input.RecoveryOf),
			ExpectedWinnerKey: thoughtVersionKeyFromRequest(input.ExpectedCurrentWinnerKey),
			Reattach:          reattach,
			CreatedAt:         s.now(),
		})
	}
	acks, err := s.thoughts.AppendThoughtOps(ctx, ops)
	if err != nil {
		return nil, mapReaderError(err)
	}
	out := make([]dto.ReaderThoughtAckResponse, 0, len(acks))
	for _, ack := range acks {
		out = append(out, dto.ReaderThoughtAckResponse{
			ContractVersion:  model.ReaderThoughtContractVersion,
			OpID:             ack.OpID,
			Sequence:         ack.Sequence,
			Disposition:      ack.Disposition,
			SubmittedKey:     thoughtVersionKeyResponse(ack.SubmittedKey),
			CurrentWinnerKey: thoughtVersionKeyResponse(ack.WinnerKey),
		})
	}
	return out, nil
}

// validateThoughtBoundedField rejects an identifier that is blank or longer
// than the column it lands in.
func validateThoughtBoundedField(value string, max int, message string) error {
	if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || len(value) > max {
		return problem.NewWithCode(problem.Invalid, "invalid_thought_op", message)
	}
	return nil
}

// validateThoughtEnvelope checks the operation envelope. Field order matters:
// it decides which message a request violating several rules gets back.
func validateThoughtEnvelope(input dto.ReaderThoughtOpRequest) error {
	if err := validateThoughtContractVersion(input.ContractVersion); err != nil {
		return err
	}
	if err := validateThoughtEnvelopeFields([]thoughtEnvelopeField{
		{input.OpID, 128, "op_id is required and must be at most 128 bytes"},
		{input.DeviceID, 128, "device_id is required and must be at most 128 bytes"},
	}); err != nil {
		return err
	}
	if err := validateThoughtLogicalClock(input.LogicalClock); err != nil {
		return err
	}
	if err := validateThoughtOperationKind(input.OperationKind); err != nil {
		return err
	}
	if err := validateThoughtEnvelopeFields([]thoughtEnvelopeField{
		{input.AnnotationID, 256, "annotation_id is required and must be at most 256 bytes"},
		{input.HostKind, 32, "host_kind is required and must be at most 32 bytes"},
		{input.HostID, 256, "host_id is required and must be at most 256 bytes"},
	}); err != nil {
		return err
	}
	if !json.Valid(input.Target) || !json.Valid(input.Payload) {
		return problem.NewWithCode(problem.Invalid, "invalid_thought_payload", "thought target and payload must be JSON")
	}
	return nil
}

type thoughtEnvelopeField struct {
	value   string
	max     int
	message string
}

func validateThoughtEnvelopeFields(fields []thoughtEnvelopeField) error {
	for _, field := range fields {
		if err := validateThoughtBoundedField(field.value, field.max, field.message); err != nil {
			return err
		}
	}
	return nil
}

func validateThoughtContractVersion(contractVersion int) error {
	if contractVersion != model.ReaderThoughtContractVersion {
		return problem.NewWithCode(problem.Invalid, "unsupported_thought_contract", "unsupported thought contract_version")
	}
	return nil
}

func validateThoughtLogicalClock(logicalClock int64) error {
	if logicalClock < 1 || logicalClock > model.ReaderThoughtMaxLogicalClock {
		return problem.NewWithCode(problem.Invalid, "invalid_thought_clock", "logical_clock must be a safe positive integer")
	}
	return nil
}

func validateThoughtOperationKind(operationKind string) error {
	if operationKind != "add" && operationKind != "update" && operationKind != "delete" {
		return problem.NewWithCode(problem.Invalid, "invalid_thought_op", "unsupported thought operation")
	}
	return nil
}

// validateThoughtTarget requires the anchor to name a supported host revision,
// so a thought can never be attached to an unversioned target.
type readerThoughtTargetWire struct {
	Kind    string `json:"kind"`
	HostID  string `json:"host_id"`
	Version struct {
		ContentRevision  int64  `json:"content_revision"`
		SourceHash       string `json:"source_hash"`
		NoteRevision     int64  `json:"note_revision"`
		MetadataRevision int64  `json:"metadata_revision"`
	} `json:"version"`
}

func readerThoughtTarget(input dto.ReaderThoughtOpRequest) (readerThoughtTargetWire, error) {
	var target readerThoughtTargetWire
	if err := json.Unmarshal(input.Target, &target); err != nil {
		return readerThoughtTargetWire{}, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "thought target must be an object")
	}
	if strings.TrimSpace(target.HostID) == "" || target.HostID != input.HostID {
		return readerThoughtTargetWire{}, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "thought target host_id must match host_id")
	}
	switch target.Kind {
	case "saved-content":
		if target.Version.ContentRevision <= 0 {
			return readerThoughtTargetWire{}, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "saved-content target requires content_revision")
		}
	case "summary":
		if strings.TrimSpace(target.Version.SourceHash) == "" {
			return readerThoughtTargetWire{}, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "summary target requires source_hash")
		}
	case "note":
		if target.Version.NoteRevision <= 0 {
			return readerThoughtTargetWire{}, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "note target requires note_revision")
		}
	case "inbox":
		if target.Version.MetadataRevision <= 0 {
			return readerThoughtTargetWire{}, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "inbox target requires metadata_revision")
		}
	default:
		return readerThoughtTargetWire{}, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "unsupported thought target kind")
	}
	return target, nil
}

func validateThoughtTarget(input dto.ReaderThoughtOpRequest) error {
	_, err := readerThoughtTarget(input)
	return err
}

func invalidThoughtReattachPayload() error {
	return problem.NewWithCode(problem.Invalid, "invalid_thought_payload", "reattach operation payload is invalid")
}

func thoughtReattachTargetMatches(input dto.ReaderThoughtOpRequest, target readerThoughtTargetWire, revision int64) bool {
	switch input.HostKind {
	case "link":
		return target.Kind == "saved-content" && target.Version.ContentRevision == revision
	case "note":
		return target.Kind == "note" && target.Version.NoteRevision == revision
	case "inbox":
		return target.Kind == "inbox" && target.Version.MetadataRevision == revision
	default:
		return false
	}
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

// readerThoughtReattachOperation recognizes the narrow command written by the
// durable history outbox. Frozen body, quote, and source remain server-owned.
func readerThoughtReattachOperation(input dto.ReaderThoughtOpRequest) (*model.ReaderThoughtReattachOperation, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(input.Payload, &payload); err != nil {
		return nil, invalidThoughtReattachPayload()
	}
	raw, ok := payload["reattach"]
	if !ok {
		return nil, nil
	}
	if input.OperationKind != "update" || len(payload) != 1 || input.RecoveryOf != nil || input.ExpectedCurrentWinnerKey != nil {
		return nil, invalidThoughtReattachPayload()
	}
	command, err := parseThoughtReattachCommand(raw)
	if err != nil {
		return nil, err
	}
	target, err := readerThoughtTarget(input)
	if err != nil {
		return nil, err
	}
	if !thoughtReattachTargetMatches(input, target, command.ExpectedHostRevision) {
		return nil, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "reattach target must match the destination host revision")
	}
	return &command, nil
}

// validateThoughtPayload requires a JSON quote except on delete, where there is
// nothing left to anchor.
func validateThoughtPayload(input dto.ReaderThoughtOpRequest) error {
	reattach, err := readerThoughtReattachOperation(input)
	if err != nil {
		return err
	}
	if reattach != nil {
		return nil
	}
	var payload struct {
		Body  string          `json:"body"`
		Quote json.RawMessage `json:"quote"`
	}
	if err := json.Unmarshal(input.Payload, &payload); err != nil {
		return problem.NewWithCode(problem.Invalid, "invalid_thought_payload", "thought payload must be an object")
	}
	if len(payload.Quote) == 0 {
		if input.OperationKind == "delete" {
			return nil
		}
		return problem.NewWithCode(problem.Invalid, "invalid_thought_payload", "thought payload requires a JSON quote")
	}
	if !json.Valid(payload.Quote) {
		return problem.NewWithCode(problem.Invalid, "invalid_thought_payload", "thought payload quote must be valid JSON")
	}
	return nil
}

func validateThoughtWire(input dto.ReaderThoughtOpRequest) error {
	if err := validateThoughtEnvelope(input); err != nil {
		return err
	}
	if err := validateThoughtTarget(input); err != nil {
		return err
	}
	if err := validateThoughtPayload(input); err != nil {
		return err
	}
	return validateThoughtRecovery(input)
}

func thoughtVersionKeyFromRequest(value *dto.ReaderThoughtVersionKeyResponse) *model.ReaderThoughtVersionKey {
	if value == nil {
		return nil
	}
	return &model.ReaderThoughtVersionKey{
		LogicalClock: value.LogicalClock,
		DeviceID:     value.DeviceID,
		OpID:         value.OpID,
	}
}

func validateThoughtRecovery(input dto.ReaderThoughtOpRequest) error {
	if input.RecoveryOf == nil && input.ExpectedCurrentWinnerKey == nil {
		return nil
	}
	if input.RecoveryOf == nil || input.ExpectedCurrentWinnerKey == nil {
		return problem.NewWithCode(problem.Invalid, "invalid_thought_recovery", "recovery_of and expected_current_winner_key must appear together")
	}
	for _, key := range []*dto.ReaderThoughtVersionKeyResponse{input.RecoveryOf, input.ExpectedCurrentWinnerKey} {
		if key.LogicalClock < 0 || key.LogicalClock > model.ReaderThoughtMaxLogicalClock ||
			validateThoughtBoundedField(key.DeviceID, 128, "thought recovery device_id is invalid") != nil ||
			validateThoughtBoundedField(key.OpID, 128, "thought recovery op_id is invalid") != nil {
			return problem.NewWithCode(problem.Invalid, "invalid_thought_recovery", "thought recovery keys are invalid")
		}
	}
	return nil
}

func (s *ReaderVNextService) ListThoughts(ctx context.Context, query, after string, limit int) (dto.ReaderThoughtsResponse, error) {
	// 与 read_search / library_search 一致地夹住 ?q=：这条路径直接落到
	// content_text 上的 ILIKE 全表模式匹配，不设上限时一个几 KB 的
	// %_%_%_… 就能打满一个核。
	if len([]rune(query)) > maxListQueryLen {
		return dto.ReaderThoughtsResponse{}, problem.NewWithCode(problem.Invalid, problem.CodeQueryTooLong, "search query too long")
	}
	items, next, err := s.thoughts.ListThoughts(ctx, query, after, limit)
	if err != nil {
		return dto.ReaderThoughtsResponse{}, mapReaderError(err)
	}
	out := dto.ReaderThoughtsResponse{ContractVersion: model.ReaderThoughtContractVersion, Items: make([]dto.ReaderThoughtResponse, 0, len(items)), NextCursor: next}
	for _, item := range items {
		out.Items = append(out.Items, thoughtResponse(item))
	}
	return out, nil
}

func (s *ReaderVNextService) ListThoughtHistory(ctx context.Context, after string, limit int) (dto.ReaderThoughtsResponse, error) {
	items, next, err := s.thoughts.ListThoughtHistory(ctx, after, limit)
	if err != nil {
		return dto.ReaderThoughtsResponse{}, mapReaderError(err)
	}
	out := dto.ReaderThoughtsResponse{ContractVersion: model.ReaderThoughtContractVersion, Items: make([]dto.ReaderThoughtResponse, 0, len(items)), NextCursor: next}
	for _, item := range items {
		out.Items = append(out.Items, thoughtResponse(item))
	}
	return out, nil
}

func (s *ReaderVNextService) SyncThoughts(ctx context.Context, after string, limit int) (dto.ReaderThoughtsResponse, error) {
	items, next, err := s.thoughts.ListThoughtsSince(ctx, after, limit)
	if err != nil {
		return dto.ReaderThoughtsResponse{}, mapReaderError(err)
	}
	out := dto.ReaderThoughtsResponse{ContractVersion: model.ReaderThoughtContractVersion, Items: make([]dto.ReaderThoughtResponse, 0, len(items)), NextCursor: next}
	for _, item := range items {
		out.Items = append(out.Items, thoughtResponse(item))
	}
	return out, nil
}

func (s *ReaderVNextService) ListThoughtConflicts(ctx context.Context, after string, limit int) (dto.ReaderThoughtConflictsResponse, error) {
	items, next, err := s.thoughts.ListThoughtConflicts(ctx, after, limit)
	if err != nil {
		return dto.ReaderThoughtConflictsResponse{}, mapReaderError(err)
	}
	out := dto.ReaderThoughtConflictsResponse{ContractVersion: model.ReaderThoughtContractVersion, Items: make([]dto.ReaderThoughtConflictResponse, 0, len(items)), NextCursor: next}
	for _, item := range items {
		out.Items = append(out.Items, dto.ReaderThoughtConflictResponse{
			Sequence:          item.Sequence,
			AnnotationID:      item.AnnotationID,
			WinnerAtDetection: thoughtConflictOperationResponse(item.Winner),
			Loser:             thoughtConflictOperationResponse(item.Loser),
		})
	}
	return out, nil
}

func thoughtConflictOperationResponse(item model.ReaderThoughtConflictOperation) dto.ReaderThoughtConflictOperationResponse {
	out := dto.ReaderThoughtConflictOperationResponse{
		ContractVersion: model.ReaderThoughtContractVersion,
		Sequence:        item.Sequence,
		OpID:            item.OpID,
		DeviceID:        item.DeviceID,
		LogicalClock:    item.LogicalClock,
		OperationKind:   item.OperationKind,
		AnnotationID:    item.AnnotationID,
		HostKind:        item.HostKind,
		HostID:          item.HostID,
		Target:          item.Target,
		Payload:         item.Payload,
		CreatedAt:       item.CreatedAt,
	}
	if item.RecoveryOf != nil {
		out.RecoveryOf = &dto.ReaderThoughtVersionKeyResponse{LogicalClock: item.RecoveryOf.LogicalClock, DeviceID: item.RecoveryOf.DeviceID, OpID: item.RecoveryOf.OpID}
	}
	if item.ExpectedWinnerKey != nil {
		out.ExpectedCurrentWinnerKey = &dto.ReaderThoughtVersionKeyResponse{LogicalClock: item.ExpectedWinnerKey.LogicalClock, DeviceID: item.ExpectedWinnerKey.DeviceID, OpID: item.ExpectedWinnerKey.OpID}
	}
	return out
}

func (s *ReaderVNextService) GetThought(ctx context.Context, rawID string) (dto.ReaderThoughtResponse, error) {
	item, err := s.thoughts.GetThought(ctx, rawID)
	if err != nil {
		return dto.ReaderThoughtResponse{}, mapReaderError(err)
	}
	return thoughtResponse(*item), nil
}

func thoughtResponse(item model.ReaderThought) dto.ReaderThoughtResponse {
	lifecycleStatus := item.LifecycleStatus
	if lifecycleStatus == "" {
		lifecycleStatus = "active"
	}
	out := dto.ReaderThoughtResponse{ContractVersion: model.ReaderThoughtContractVersion, ID: item.ID, HostKind: item.HostKind, HostID: item.HostID, Target: item.Target, Quote: item.Quote, Body: item.Body, Source: item.Source, Deleted: item.Deleted, LastSequence: item.LastSequence, WinnerKey: thoughtVersionKeyResponse(item.WinnerKey), CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, LifecycleStatus: lifecycleStatus, LifecycleReason: item.LifecycleReason, TombstonedAt: item.TombstonedAt, OriginalHostSnapshot: item.OriginalHostSnapshot}
	if item.LinkID != nil {
		id := item.LinkID.String()
		out.LinkID = &id
	}
	return out
}

func thoughtVersionKeyResponse(key model.ReaderThoughtVersionKey) dto.ReaderThoughtVersionKeyResponse {
	return dto.ReaderThoughtVersionKeyResponse{
		LogicalClock: key.LogicalClock,
		DeviceID:     key.DeviceID,
		OpID:         key.OpID,
	}
}
