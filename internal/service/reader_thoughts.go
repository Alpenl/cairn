package service

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"webtag/internal/model"
	"webtag/internal/problem"
)

type ReaderThoughtOpCommand struct {
	ContractVersion          int
	OpID                     string
	DeviceID                 string
	LogicalClock             int64
	OperationKind            string
	AnnotationID             string
	HostKind                 string
	HostID                   string
	Target                   ReaderThoughtTargetCommand
	Payload                  ReaderThoughtPayloadCommand
	RecoveryOf               *model.ReaderThoughtVersionKey
	ExpectedCurrentWinnerKey *model.ReaderThoughtVersionKey
}

type ReaderThoughtTargetCommand struct {
	Kind    string                            `json:"kind"`
	HostID  string                            `json:"host_id"`
	Version ReaderThoughtTargetVersionCommand `json:"version"`
}

type ReaderThoughtTargetVersionCommand struct {
	ContentRevision  int64  `json:"content_revision,omitempty"`
	SourceHash       string `json:"source_hash,omitempty"`
	NoteRevision     int64  `json:"note_revision,omitempty"`
	MetadataRevision int64  `json:"metadata_revision,omitempty"`
}

type ReaderThoughtPayloadCommand struct {
	Body     string                                `json:"body,omitempty"`
	Quote    *ReaderThoughtQuoteCommand            `json:"quote,omitempty"`
	Source   string                                `json:"source,omitempty"`
	LinkID   string                                `json:"link_id,omitempty"`
	Reattach *model.ReaderThoughtReattachOperation `json:"-"`
}

type ReaderThoughtQuoteCommand struct {
	Exact  *string `json:"exact,omitempty"`
	Prefix *string `json:"prefix,omitempty"`
	Suffix *string `json:"suffix,omitempty"`
	Start  *int    `json:"start,omitempty"`
	End    *int    `json:"end,omitempty"`
}

func (payload ReaderThoughtPayloadCommand) reattachOnly() bool {
	return payload.Reattach != nil && payload.Body == "" && payload.Quote == nil && payload.Source == "" && payload.LinkID == ""
}

func (payload *ReaderThoughtPayloadCommand) UnmarshalJSON(data []byte) error {
	var decoded readerThoughtPayloadJSON
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	payload.Body = decoded.Body
	payload.Quote = decoded.Quote
	payload.Source = decoded.Source
	payload.LinkID = decoded.LinkID
	payload.Reattach = nil
	if decoded.Reattach != nil {
		payload.Reattach = &model.ReaderThoughtReattachOperation{
			ExpectedLastSequence: decoded.Reattach.ExpectedLastSequence,
			ExpectedHostRevision: decoded.Reattach.ExpectedHostRevision,
		}
	}
	return nil
}

type readerThoughtPayloadJSON struct {
	Body     string                     `json:"body,omitempty"`
	Quote    *ReaderThoughtQuoteCommand `json:"quote,omitempty"`
	Source   string                     `json:"source,omitempty"`
	LinkID   string                     `json:"link_id,omitempty"`
	Reattach *readerThoughtReattachJSON `json:"reattach,omitempty"`
}

type readerThoughtReattachJSON struct {
	ExpectedLastSequence int64 `json:"expected_last_sequence"`
	ExpectedHostRevision int64 `json:"expected_host_revision"`
}

func readerThoughtModelJSON(input ReaderThoughtOpCommand) (json.RawMessage, json.RawMessage, error) {
	target, err := json.Marshal(input.Target)
	if err != nil {
		return nil, nil, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "thought target must be JSON encodable")
	}
	payload := readerThoughtPayloadJSON{
		Body: input.Payload.Body, Quote: input.Payload.Quote, Source: input.Payload.Source, LinkID: input.Payload.LinkID,
	}
	if input.Payload.Reattach != nil {
		payload.Reattach = &readerThoughtReattachJSON{
			ExpectedLastSequence: input.Payload.Reattach.ExpectedLastSequence,
			ExpectedHostRevision: input.Payload.Reattach.ExpectedHostRevision,
		}
	}
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, problem.NewWithCode(problem.Invalid, "invalid_thought_payload", "thought payload must be JSON encodable")
	}
	return target, encodedPayload, nil
}

type ReaderThoughtOpsCommand struct {
	Ops []ReaderThoughtOpCommand
}

type ReaderThoughtPage struct {
	Items      []model.ReaderThought
	NextCursor string
}

type ReaderThoughtConflictPage struct {
	Items      []model.ReaderThoughtConflict
	NextCursor string
}

func (s *ReaderThoughtApplication) PushThoughtOps(ctx context.Context, command ReaderThoughtOpsCommand) ([]model.ReaderThoughtAck, error) {
	if len(command.Ops) == 0 || len(command.Ops) > 200 {
		return nil, problem.NewWithCode(problem.Invalid, "invalid_thought_ops", "thought operation batch must contain between 1 and 200 operations")
	}
	ops := make([]model.ReaderThoughtOp, 0, len(command.Ops))
	for _, input := range command.Ops {
		if err := validateThoughtCommand(input); err != nil {
			return nil, err
		}
		reattach, err := readerThoughtReattachOperation(input)
		if err != nil {
			return nil, err
		}
		target, payload, err := readerThoughtModelJSON(input)
		if err != nil {
			return nil, err
		}
		ops = append(ops, model.ReaderThoughtOp{
			OpID: input.OpID, DeviceID: input.DeviceID, LogicalClock: input.LogicalClock,
			OperationKind: input.OperationKind, AnnotationID: input.AnnotationID,
			HostKind: input.HostKind, HostID: input.HostID, Target: target,
			Payload: payload, RecoveryOf: cloneThoughtVersionKey(input.RecoveryOf),
			ExpectedWinnerKey: cloneThoughtVersionKey(input.ExpectedCurrentWinnerKey),
			Reattach:          reattach,
			CreatedAt:         s.now(),
		})
	}
	acks, err := s.thoughts.AppendThoughtOps(ctx, ops)
	if err != nil {
		return nil, mapReaderError(err)
	}
	return acks, nil
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
func validateThoughtEnvelope(input ReaderThoughtOpCommand) error {
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
func readerThoughtTarget(input ReaderThoughtOpCommand) (ReaderThoughtTargetCommand, error) {
	target := input.Target
	if strings.TrimSpace(target.HostID) == "" || target.HostID != input.HostID {
		return ReaderThoughtTargetCommand{}, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "thought target host_id must match host_id")
	}
	switch target.Kind {
	case "saved-content":
		if target.Version.ContentRevision <= 0 {
			return ReaderThoughtTargetCommand{}, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "saved-content target requires content_revision")
		}
	case "summary":
		if strings.TrimSpace(target.Version.SourceHash) == "" {
			return ReaderThoughtTargetCommand{}, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "summary target requires source_hash")
		}
	case "note":
		if target.Version.NoteRevision <= 0 {
			return ReaderThoughtTargetCommand{}, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "note target requires note_revision")
		}
	case "inbox":
		if target.Version.MetadataRevision <= 0 {
			return ReaderThoughtTargetCommand{}, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "inbox target requires metadata_revision")
		}
	default:
		return ReaderThoughtTargetCommand{}, problem.NewWithCode(problem.Invalid, "invalid_thought_target", "unsupported thought target kind")
	}
	return target, nil
}

func validateThoughtTarget(input ReaderThoughtOpCommand) error {
	_, err := readerThoughtTarget(input)
	return err
}

func invalidThoughtReattachPayload() error {
	return problem.NewWithCode(problem.Invalid, "invalid_thought_payload", "reattach operation payload is invalid")
}

func thoughtReattachTargetMatches(input ReaderThoughtOpCommand, target ReaderThoughtTargetCommand, revision int64) bool {
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

// readerThoughtReattachOperation recognizes the narrow command written by the
// durable history outbox. Frozen body, quote, and source remain server-owned.
func readerThoughtReattachOperation(input ReaderThoughtOpCommand) (*model.ReaderThoughtReattachOperation, error) {
	if input.Payload.Reattach == nil {
		return nil, nil
	}
	if input.OperationKind != "update" || !input.Payload.reattachOnly() || input.RecoveryOf != nil || input.ExpectedCurrentWinnerKey != nil {
		return nil, invalidThoughtReattachPayload()
	}
	command := *input.Payload.Reattach
	if command.ExpectedLastSequence < 0 || command.ExpectedHostRevision <= 0 {
		return nil, invalidThoughtReattachPayload()
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
func validateThoughtPayload(input ReaderThoughtOpCommand) error {
	reattach, err := readerThoughtReattachOperation(input)
	if err != nil {
		return err
	}
	if reattach != nil {
		return nil
	}
	if input.Payload.Quote == nil {
		if input.OperationKind == "delete" {
			return nil
		}
		return problem.NewWithCode(problem.Invalid, "invalid_thought_payload", "thought payload requires a JSON quote")
	}
	return nil
}

func validateThoughtCommand(input ReaderThoughtOpCommand) error {
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

func cloneThoughtVersionKey(value *model.ReaderThoughtVersionKey) *model.ReaderThoughtVersionKey {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func validateThoughtRecovery(input ReaderThoughtOpCommand) error {
	if input.RecoveryOf == nil && input.ExpectedCurrentWinnerKey == nil {
		return nil
	}
	if input.RecoveryOf == nil || input.ExpectedCurrentWinnerKey == nil {
		return problem.NewWithCode(problem.Invalid, "invalid_thought_recovery", "recovery_of and expected_current_winner_key must appear together")
	}
	for _, key := range []*model.ReaderThoughtVersionKey{input.RecoveryOf, input.ExpectedCurrentWinnerKey} {
		if key.LogicalClock < 0 || key.LogicalClock > model.ReaderThoughtMaxLogicalClock ||
			validateThoughtBoundedField(key.DeviceID, 128, "thought recovery device_id is invalid") != nil ||
			validateThoughtBoundedField(key.OpID, 128, "thought recovery op_id is invalid") != nil {
			return problem.NewWithCode(problem.Invalid, "invalid_thought_recovery", "thought recovery keys are invalid")
		}
	}
	return nil
}

func (s *ReaderThoughtApplication) ListThoughts(ctx context.Context, query, after string, limit int) (ReaderThoughtPage, error) {
	// 与 read_search / library_search 一致地夹住 ?q=：这条路径直接落到
	// content_text 上的 ILIKE 全表模式匹配，不设上限时一个几 KB 的
	// %_%_%_… 就能打满一个核。
	if len([]rune(query)) > maxListQueryLen {
		return ReaderThoughtPage{}, problem.NewWithCode(problem.Invalid, problem.CodeQueryTooLong, "search query too long")
	}
	items, next, err := s.thoughts.ListThoughts(ctx, query, after, limit)
	if err != nil {
		return ReaderThoughtPage{}, mapReaderError(err)
	}
	return ReaderThoughtPage{Items: items, NextCursor: next}, nil
}

func (s *ReaderThoughtApplication) ListThoughtHistory(ctx context.Context, after string, limit int) (ReaderThoughtPage, error) {
	items, next, err := s.thoughts.ListThoughtHistory(ctx, after, limit)
	if err != nil {
		return ReaderThoughtPage{}, mapReaderError(err)
	}
	return ReaderThoughtPage{Items: items, NextCursor: next}, nil
}

func (s *ReaderThoughtApplication) SyncThoughts(ctx context.Context, after string, limit int) (ReaderThoughtPage, error) {
	items, next, err := s.thoughts.ListThoughtsSince(ctx, after, limit)
	if err != nil {
		return ReaderThoughtPage{}, mapReaderError(err)
	}
	return ReaderThoughtPage{Items: items, NextCursor: next}, nil
}

func (s *ReaderThoughtApplication) ListThoughtConflicts(ctx context.Context, after string, limit int) (ReaderThoughtConflictPage, error) {
	items, next, err := s.thoughts.ListThoughtConflicts(ctx, after, limit)
	if err != nil {
		return ReaderThoughtConflictPage{}, mapReaderError(err)
	}
	return ReaderThoughtConflictPage{Items: items, NextCursor: next}, nil
}

func (s *ReaderThoughtApplication) GetThought(ctx context.Context, rawID string) (model.ReaderThought, error) {
	item, err := s.thoughts.GetThought(ctx, rawID)
	if err != nil {
		return model.ReaderThought{}, mapReaderError(err)
	}
	return *item, nil
}
