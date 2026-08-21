package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/text/cases"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/notetitle"
	"webtag/internal/problem"
	"webtag/internal/repository"
)

// ReaderAIBackend is deliberately tiny. The Reader service owns scope and
// context construction; the backend owns the provider transport and can be
// disabled without making the rest of Reader unavailable.
type ReaderAIBackend interface {
	Complete(context.Context, string, string) (answer string, modelName string, err error)
}

type ReaderVNextService struct {
	thoughts          ReaderThoughtStore
	notes             ReaderNoteStore
	inbox             ReaderInboxStore
	todos             ReaderTodoStore
	library           ReaderLibraryStore
	hosts             ReaderHostStore
	ai                ReaderAIBackend
	inboxCommands     InboxProposalCommands
	now               func() time.Time
	activityCursorKey []byte
}

type ReaderVNextServiceOptions struct {
	CursorSigningKey      string
	InboxProposalCommands InboxProposalCommands
}

func NewReaderVNextService(stores ReaderStores, ai ReaderAIBackend, options ...ReaderVNextServiceOptions) *ReaderVNextService {
	cursorKey := processReaderCursorKey
	var configured ReaderVNextServiceOptions
	if len(options) > 0 {
		configured = options[0]
	}
	if configured.CursorSigningKey != "" {
		cursorKey = []byte(configured.CursorSigningKey)
	}
	return &ReaderVNextService{
		thoughts:          stores.Thoughts,
		notes:             stores.Notes,
		inbox:             stores.Inbox,
		todos:             stores.Todos,
		library:           stores.Library,
		hosts:             stores.Hosts,
		ai:                ai,
		inboxCommands:     configured.InboxProposalCommands,
		now:               time.Now,
		activityCursorKey: append([]byte(nil), cursorKey...),
	}
}

func readerUUID(raw, field string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, problem.NewWithCode(problem.Invalid, "invalid_"+field, field+" must be a UUID")
	}
	return id, nil
}

func parseReaderInboxPartition(raw string, defaultActive bool) (model.ReaderInboxPartition, error) {
	partition := model.ReaderInboxPartition(strings.TrimSpace(raw))
	if partition == "" && defaultActive {
		return model.ReaderInboxPartitionActive, nil
	}
	if !partition.Valid() {
		return "", problem.NewWithCode(problem.Invalid, "invalid_inbox_partition", "partition must be active or expired")
	}
	return partition, nil
}

// readerErrorMapping binds repository sentinels to one public problem.
// Several sentinels may share an entry when they describe the same client-facing
// fault.
type readerErrorMapping struct {
	targets []error
	kind    problem.Kind
	code    string
	message string
}

// readerErrorMappings is scanned top to bottom, so the slice order is the
// contract: an error wrapping more than one sentinel keeps the classification of the
// earliest entry it matches. Reordering entries is a behaviour change, not a
// cosmetic one.
var readerErrorMappings = []readerErrorMapping{
	{targets: []error{repository.ErrNotFound}, kind: problem.NotFound, code: "reader_not_found", message: "reader resource not found"},
	{targets: []error{repository.ErrReaderThoughtReattachInvalidState}, kind: problem.Conflict, code: "thought_reattach_invalid_state", message: "thought is not a historical tombstone that can be reattached"},
	{targets: []error{repository.ErrReaderHostNotTrashed}, kind: problem.Conflict, code: "host_not_trashed", message: "reader host must be trashed before permanent purge"},
	{targets: []error{repository.ErrReaderTodoHostRevisionNotApplicable}, kind: problem.Invalid, code: "todo_host_revision_not_applicable", message: "expected_host_revision is not applicable to standalone TODOs"},
	{targets: []error{repository.ErrRevisionConflict}, kind: problem.Conflict, code: problem.CodeRevisionConflict, message: "resource revision is stale"},
	{targets: []error{repository.ErrInvalidReaderHostKind}, kind: problem.Invalid, code: "invalid_host_kind", message: "host_kind must be link, inbox, or note"},
	{targets: []error{repository.ErrInvalidReaderCursor}, kind: problem.Invalid, code: problem.CodeInvalidCursor, message: "invalid cursor"},
	{targets: []error{repository.ErrInvalidReaderReanchor}, kind: problem.Invalid, code: "invalid_reanchor_ops", message: "invalid reanchor operations"},
	{targets: []error{repository.ErrReaderNoteContentEmpty}, kind: problem.Invalid, code: "note_content_empty", message: "note content must not be empty"},
	{targets: []error{repository.ErrReaderNoteDraftDirty}, kind: problem.Conflict, code: "note_draft_dirty", message: "note draft has unpublished changes"},
	{targets: []error{repository.ErrReaderNoteReanchorIncomplete}, kind: problem.Invalid, code: "note_reanchor_incomplete", message: "reanchor operations must exactly match active note thoughts"},
	{targets: []error{repository.ErrInvalidReaderFeedItem}, kind: problem.Invalid, code: "invalid_feed_item", message: "invalid feed item"},
	{targets: []error{repository.ErrReaderTodoProjectionImmutable}, kind: problem.Invalid, code: "todo_projection_immutable", message: "projected TODO text and due date are source-owned"},
	{targets: []error{repository.ErrReaderTodoHostMissing}, kind: problem.Conflict, code: "todo_host_missing", message: "the TODO source is no longer available"},
	{targets: []error{repository.ErrReaderTodoAnchorNotFound}, kind: problem.Conflict, code: "todo_anchor_not_found", message: "the TODO source block is no longer available"},
	{targets: []error{repository.ErrReaderTodoAnchorAmbiguous}, kind: problem.Conflict, code: "todo_anchor_ambiguous", message: "the TODO source block is ambiguous"},
	{targets: []error{repository.ErrReaderThoughtOpConflict}, kind: problem.Conflict, code: "thought_op_conflict", message: "the operation id was already used with different content"},
	{targets: []error{repository.ErrReaderThoughtRecoveryConflict}, kind: problem.Conflict, code: "thought_recovery_conflict", message: "the current thought winner changed before recovery"},
	{targets: []error{repository.ErrReaderThoughtClockExhausted}, kind: problem.Conflict, code: "thought_clock_exhausted", message: "the thought logical clock is exhausted"},
	{targets: []error{repository.ErrReaderThoughtClockInvalid}, kind: problem.Invalid, code: "invalid_thought_clock", message: "the thought logical clock is invalid"},
	{targets: []error{repository.ErrReaderThoughtLinkMismatch, repository.ErrInvalidReaderThought}, kind: problem.Invalid, code: "invalid_thought", message: "thought host and payload are inconsistent"},
	{targets: []error{repository.ErrReaderInboxTitleRequired}, kind: problem.Invalid, code: "inbox_title_required", message: "inbox title must not be blank when confirming"},
	{targets: []error{repository.ErrReaderInboxStateConflict}, kind: problem.Conflict, code: "inbox_state_conflict", message: "the inbox item is in a state that cannot accept this transition"},
}

// mapReaderError translates repository sentinels into public problems. Anything the
// table does not recognise is returned untouched so callers never lose an
// unexpected cause.
func mapReaderError(err error) error {
	if err == nil {
		return nil
	}
	for _, mapping := range readerErrorMappings {
		for _, target := range mapping.targets {
			if errors.Is(err, target) {
				return problem.NewWithCode(mapping.kind, mapping.code, mapping.message)
			}
		}
	}
	return err
}

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

func deriveNoteTitle(content string) string {
	return notetitle.Derive(content)
}

func (s *ReaderVNextService) CreateNote(ctx context.Context, request dto.ReaderNoteCreateRequest) (dto.ReaderNoteResponse, error) {
	title := strings.TrimSpace(request.Title)
	if title == "" {
		title = deriveNoteTitle(request.Content)
	}
	note := model.ReaderNote{Title: title, PublishedContent: ""}
	if request.Content != "" {
		note.DraftContent = &request.Content
	}
	created, err := s.notes.CreateNote(ctx, note)
	if err != nil {
		return dto.ReaderNoteResponse{}, mapReaderError(err)
	}
	return noteResponse(*created), nil
}

func (s *ReaderVNextService) ListNotes(ctx context.Context, after string, limit int) (dto.ReaderNotesResponse, error) {
	items, count, next, err := s.notes.ListNotes(ctx, after, limit)
	if err != nil {
		return dto.ReaderNotesResponse{}, mapReaderError(err)
	}
	out := dto.ReaderNotesResponse{Items: make([]dto.ReaderNoteResponse, 0, len(items)), Count: count, NextCursor: next}
	for _, item := range items {
		out.Items = append(out.Items, noteResponse(item))
	}
	return out, nil
}

func (s *ReaderVNextService) GetNote(ctx context.Context, rawID string) (dto.ReaderNoteResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	note, err := s.notes.GetNote(ctx, id)
	if err != nil {
		return dto.ReaderNoteResponse{}, mapReaderError(err)
	}
	return noteResponse(*note), nil
}

func noteResponse(note model.ReaderNote) dto.ReaderNoteResponse {
	dirty := note.DraftContent != nil && *note.DraftContent != note.PublishedContent
	return dto.ReaderNoteResponse{ID: note.ID.String(), Title: note.Title, PublishedContent: note.PublishedContent, PublishedRevision: note.PublishedRevision, DraftContent: note.DraftContent, DraftRevision: note.DraftRevision, DraftUpdatedAt: note.DraftUpdatedAt, DeletedAt: note.DeletedAt, CreatedAt: note.CreatedAt, UpdatedAt: note.UpdatedAt, Dirty: dirty}
}

func (s *ReaderVNextService) SaveNoteDraft(ctx context.Context, rawID string, request dto.ReaderNoteDraftRequest) (dto.ReaderNoteResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	note, err := s.notes.SaveNoteDraft(ctx, model.ReaderNoteDraftCommand{NoteID: id, Content: request.Content, ExpectedDraftRevision: request.ExpectedDraftRevision})
	if err != nil {
		return dto.ReaderNoteResponse{}, mapReaderError(err)
	}
	return noteResponse(*note), nil
}

func (s *ReaderVNextService) DiscardNoteDraft(ctx context.Context, rawID string, expectedRevision int64) error {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return err
	}
	if expectedRevision < 1 {
		return problem.NewWithCode(problem.Invalid, "invalid_draft_revision", "draft revision must be positive")
	}
	return mapReaderError(s.notes.DiscardNoteDraft(ctx, model.ReaderNoteDiscardDraftCommand{NoteID: id, ExpectedDraftRevision: expectedRevision}))
}

func (s *ReaderVNextService) PublishNote(ctx context.Context, rawID string, request dto.ReaderNotePublishRequest) (dto.ReaderNoteResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	if request.ExpectedDraftRevision == nil || request.ExpectedPublishedRevision == nil {
		return dto.ReaderNoteResponse{}, problem.NewWithCode(problem.Invalid, "note_revision_required", "draft and published revisions are required")
	}
	if err := validateReaderReanchorOps(request.ReanchorOps); err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	note, err := s.notes.PublishNote(ctx, model.ReaderNotePublishCommand{NoteID: id, ExpectedDraftRevision: *request.ExpectedDraftRevision, ExpectedPublishedRevision: *request.ExpectedPublishedRevision, ReanchorOps: request.ReanchorOps})
	if err != nil {
		return dto.ReaderNoteResponse{}, mapReaderError(err)
	}
	return noteResponse(*note), nil
}

func validateReaderReanchorOps(ops []json.RawMessage) error {
	if len(ops) > 500 {
		return problem.NewWithCode(problem.Invalid, "invalid_reanchor_ops", "too many reanchor operations")
	}
	for _, raw := range ops {
		if len(raw) == 0 || len(raw) > 128*1024 || !json.Valid(raw) {
			return problem.NewWithCode(problem.Invalid, "invalid_reanchor_ops", "reanchor operation must be valid bounded JSON")
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return problem.NewWithCode(problem.Invalid, "invalid_reanchor_ops", "reanchor operation must be a JSON object")
		}
	}
	return nil
}

func (s *ReaderVNextService) DeleteNote(ctx context.Context, rawID string) (dto.ReaderHostLifecycleResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	result, err := s.hosts.SoftDeleteHost(ctx, model.ReaderHostNote, id)
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, mapReaderError(err)
	}
	return readerHostLifecycleResponse(result), nil
}

func (s *ReaderVNextService) RestoreNote(ctx context.Context, rawID string) (dto.ReaderHostLifecycleResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	result, err := s.hosts.RestoreHost(ctx, model.ReaderHostNote, id)
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, mapReaderError(err)
	}
	return readerHostLifecycleResponse(result), nil
}

func (s *ReaderVNextService) ListNoteHistory(ctx context.Context, rawID string, limit int) ([]dto.ReaderNoteHistoryResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return nil, err
	}
	items, err := s.notes.ListNoteHistory(ctx, id, limit)
	if err != nil {
		return nil, mapReaderError(err)
	}
	out := make([]dto.ReaderNoteHistoryResponse, 0, len(items))
	for _, item := range items {
		out = append(out, dto.ReaderNoteHistoryResponse{ID: item.ID, Revision: item.Revision, Title: item.Title, Content: item.Content, ReanchorOps: item.ReanchorOps, CreatedAt: item.CreatedAt})
	}
	return out, nil
}

func (s *ReaderVNextService) RestoreNoteRevision(ctx context.Context, rawID string, revision int64, request dto.ReaderNoteRestoreRequest) (dto.ReaderNoteResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	if request.ExpectedDraftRevision == nil || request.ExpectedPublishedRevision == nil {
		return dto.ReaderNoteResponse{}, problem.NewWithCode(problem.Invalid, "note_revision_required", "draft and published revisions are required")
	}
	if err := validateReaderReanchorOps(request.ReanchorOps); err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	note, err := s.notes.RestoreNoteRevision(ctx, model.ReaderNoteRestoreCommand{NoteID: id, Revision: revision, ExpectedDraftRevision: *request.ExpectedDraftRevision, ExpectedPublishedRevision: *request.ExpectedPublishedRevision, ReanchorOps: request.ReanchorOps})
	if err != nil {
		return dto.ReaderNoteResponse{}, mapReaderError(err)
	}
	return noteResponse(*note), nil
}

// CreateInbox is a collection entry point, so it goes through the same
// validateURL contract as /api/links and /api/ingest.
//
// It previously trusted the binding tag's `url` rule alone, which accepts any
// scheme that carries a host and performs no SSRF check. Confirming such an
// item copies its URL verbatim into links.url and links.source_key, so a
// `file://` or private-network address reached the collection write path with
// nothing in between — and a variant spelling created a second reading record
// that /api/links would have deduplicated.
func (s *ReaderVNextService) CreateInbox(ctx context.Context, request dto.ReaderInboxCreateRequest) (dto.ReaderInboxResponse, error) {
	normalizedURL, err := validateURL(request.URL)
	if err != nil {
		return dto.ReaderInboxResponse{}, err
	}
	input := model.ReaderInbox{URL: strings.TrimSpace(request.URL), IdentityKey: normalizedURL, SourceKind: request.SourceKind, Title: request.Title, Body: request.Body, Note: request.Note, Summary: request.Summary, Tags: append([]string(nil), request.Tags...), ProposalStatus: "idle"}
	if s.inboxCommands == nil {
		return dto.ReaderInboxResponse{}, errors.New("create Reader inbox: durable commands are not configured")
	}
	result, commandErr := s.inboxCommands.CreateInboxProposal(ctx, CreateInboxProposalCommand{Inbox: input})
	if commandErr != nil {
		return dto.ReaderInboxResponse{}, mapReaderError(commandErr)
	}
	item := result.Inbox
	if item == nil {
		return dto.ReaderInboxResponse{}, errors.New("create Reader inbox: durable command returned nil item")
	}
	return inboxResponse(*item), nil
}

func (s *ReaderVNextService) ListInbox(ctx context.Context, rawPartition, after string, limit int) (dto.ReaderInboxResponsePage, error) {
	partition, err := parseReaderInboxPartition(rawPartition, true)
	if err != nil {
		return dto.ReaderInboxResponsePage{}, err
	}
	items, activeCount, expiredCount, next, err := s.inbox.ListInbox(ctx, partition, after, limit)
	if err != nil {
		return dto.ReaderInboxResponsePage{}, mapReaderError(err)
	}
	out := dto.ReaderInboxResponsePage{
		Items:        make([]dto.ReaderInboxListItemResponse, 0, len(items)),
		NextCursor:   next,
		ActiveCount:  activeCount,
		ExpiredCount: expiredCount,
	}
	for _, item := range items {
		out.Items = append(out.Items, inboxListItemResponse(item))
	}
	return out, nil
}

// inboxListItemResponse maps the queue projection. It exists so the list path
// cannot accidentally reuse inboxResponse and reintroduce the body/note the
// projection was created to leave behind.
func inboxListItemResponse(item model.ReaderInboxListItem) dto.ReaderInboxListItemResponse {
	tags := item.Tags
	if tags == nil {
		tags = []string{}
	}
	return dto.ReaderInboxListItemResponse{
		ID:               item.ID.String(),
		URL:              item.URL,
		SourceKind:       item.SourceKind,
		Title:            item.Title,
		Preview:          item.Preview,
		Tags:             tags,
		Status:           item.Status,
		MetadataRevision: item.MetadataRevision,
		Expired:          item.Expired,
		UpdatedAt:        item.UpdatedAt,
	}
}

func (s *ReaderVNextService) GetInbox(ctx context.Context, rawID string) (dto.ReaderInboxResponse, error) {
	id, err := readerUUID(rawID, "inbox_id")
	if err != nil {
		return dto.ReaderInboxResponse{}, err
	}
	item, err := s.inbox.GetInbox(ctx, id)
	if err != nil {
		return dto.ReaderInboxResponse{}, mapReaderError(err)
	}
	return inboxResponse(*item), nil
}

func inboxResponse(item model.ReaderInbox) dto.ReaderInboxResponse {
	return dto.ReaderInboxResponse{ID: item.ID.String(), URL: item.URL, SourceKind: item.SourceKind, Title: item.Title, Body: item.Body, Note: item.Note, Summary: item.Summary, SuggestedTags: item.SuggestedTags, ProposalStatus: item.ProposalStatus, Tags: item.Tags, Status: item.Status, MetadataRevision: item.MetadataRevision, ExpiresAt: item.ExpiresAt, Expired: item.Expired, DeletedAt: item.DeletedAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
}

func (s *ReaderVNextService) PatchInbox(ctx context.Context, rawID string, request dto.ReaderInboxPatchRequest, expected int64) (dto.ReaderInboxResponse, error) {
	id, err := readerUUID(rawID, "inbox_id")
	if err != nil {
		return dto.ReaderInboxResponse{}, err
	}
	item, err := s.inbox.PatchInbox(ctx, model.ReaderInboxPatch{ID: id, Title: request.Title, Body: request.Body, Note: request.Note, Summary: request.Summary, Tags: request.Tags, ExpectedRevision: expected})
	if err != nil {
		return dto.ReaderInboxResponse{}, mapReaderError(err)
	}
	return inboxResponse(*item), nil
}

func (s *ReaderVNextService) ConfirmInbox(ctx context.Context, rawID string, expectedRevision int64) (map[string]string, error) {
	id, err := readerUUID(rawID, "inbox_id")
	if err != nil {
		return nil, err
	}
	item, err := s.inbox.GetInbox(ctx, id)
	if err != nil {
		return nil, mapReaderError(err)
	}
	if item.Title == nil || strings.TrimSpace(*item.Title) == "" {
		return nil, problem.NewWithCode(problem.Invalid, "inbox_title_required", "inbox title must not be blank when confirming")
	}
	if expectedRevision >= 0 && item.MetadataRevision != expectedRevision {
		return nil, mapReaderError(repository.ErrRevisionConflict)
	}
	var expected *int64
	if expectedRevision >= 0 {
		expected = &expectedRevision
	}
	linkID, err := s.inbox.ConfirmInbox(ctx, id, expected)
	if err != nil {
		return nil, mapReaderError(err)
	}
	return map[string]string{"target_kind": "link", "link_id": linkID.String(), "status": "confirmed"}, nil
}

func (s *ReaderVNextService) DiscardInbox(ctx context.Context, rawID string) error {
	id, err := readerUUID(rawID, "inbox_id")
	if err != nil {
		return err
	}
	return mapReaderError(s.inbox.DiscardInbox(ctx, id))
}

// RestoreInbox is an Inbox-specific lifecycle command because an expired live
// row is not trashed. The repository atomically renews only expired pending
// rows and leaves all user/AI-owned content untouched.
func (s *ReaderVNextService) RestoreInbox(ctx context.Context, rawID string) error {
	id, err := readerUUID(rawID, "inbox_id")
	if err != nil {
		return err
	}
	return mapReaderError(s.inbox.RestoreInbox(ctx, id))
}

// ConfirmAIProposals confirms the next stable server-selected set of completed
// AI proposals. The client supplies only the partition; eligibility and the
// atomic transition stay at the repository boundary.
func (s *ReaderVNextService) ConfirmAIProposals(ctx context.Context, rawPartition string) (dto.ReaderInboxConfirmAIProposalsResponse, error) {
	partition, err := parseReaderInboxPartition(rawPartition, false)
	if err != nil {
		return dto.ReaderInboxConfirmAIProposalsResponse{}, err
	}
	confirmation, err := s.inbox.ConfirmAIProposals(ctx, partition)
	if err != nil {
		return dto.ReaderInboxConfirmAIProposalsResponse{}, mapReaderError(err)
	}
	response := dto.ReaderInboxConfirmAIProposalsResponse{
		Atomic:         true,
		Items:          make([]dto.ReaderInboxBulkItemResponse, 0, len(confirmation.Items)),
		RemainingCount: confirmation.RemainingCount,
	}
	for _, item := range confirmation.Items {
		out := dto.ReaderInboxBulkItemResponse{InboxID: item.ID.String(), Status: item.Status}
		if item.LinkID != nil {
			linkID := item.LinkID.String()
			out.LinkID = &linkID
		}
		response.Items = append(response.Items, out)
	}
	return response, nil
}

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

func parseReaderInboxBulkIDs(rawIDs []string) ([]uuid.UUID, error) {
	if len(rawIDs) == 0 || len(rawIDs) > 100 {
		return nil, problem.NewWithCode(problem.Invalid, "invalid_inbox_batch", "inbox batch must contain between 1 and 100 ids")
	}
	ids := make([]uuid.UUID, 0, len(rawIDs))
	seen := make(map[uuid.UUID]struct{}, len(rawIDs))
	for _, rawID := range rawIDs {
		id, err := readerUUID(rawID, "inbox_id")
		if err != nil {
			return nil, err
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, problem.NewWithCode(problem.Invalid, "invalid_inbox_batch", "inbox batch must contain at least one unique id")
	}
	return ids, nil
}

// ConfirmInboxBulk is an internal service seam for the future batch endpoint.
// The repository commits the whole batch atomically and returns the same link
// id for retries of already-confirmed captures.
func (s *ReaderVNextService) ConfirmInboxBulk(ctx context.Context, rawIDs []string, rawExpectedRevisions map[string]int64) ([]model.ReaderInboxBulkResult, error) {
	ids, err := parseReaderInboxBulkIDs(rawIDs)
	if err != nil {
		return nil, err
	}
	expectedRevisions := make(map[uuid.UUID]int64, len(rawExpectedRevisions))
	for rawID, revision := range rawExpectedRevisions {
		id, parseErr := readerUUID(rawID, "expected_revision inbox_id")
		if parseErr != nil || revision < 0 {
			return nil, problem.NewWithCode(problem.Invalid, "invalid_inbox_batch_revision", "expected revisions must use requested inbox ids and non-negative revisions")
		}
		expectedRevisions[id] = revision
	}
	if len(expectedRevisions) > 0 && len(expectedRevisions) != len(ids) {
		return nil, problem.NewWithCode(problem.Invalid, "invalid_inbox_batch_revision", "expected revisions must cover every requested inbox id")
	}
	confirmations := make([]model.ReaderInboxBulkConfirmation, 0, len(ids))
	for _, id := range ids {
		var expectedRevision *int64
		if len(expectedRevisions) > 0 {
			revision, ok := expectedRevisions[id]
			if !ok {
				return nil, problem.NewWithCode(problem.Invalid, "invalid_inbox_batch_revision", "expected revisions must cover every requested inbox id")
			}
			revisionCopy := revision
			expectedRevision = &revisionCopy
		}
		confirmations = append(confirmations, model.ReaderInboxBulkConfirmation{ID: id, ExpectedRevision: expectedRevision})
	}
	items, err := s.inbox.BulkConfirmInbox(ctx, confirmations)
	if err != nil {
		return nil, mapReaderError(err)
	}
	return items, nil
}

// DiscardInboxBulk is the matching internal seam for batch discard. A trashed
// item is safe to retry; a confirmed item is rejected so a bulk
// action cannot remove a saved link's source capture accidentally.
func (s *ReaderVNextService) DiscardInboxBulk(ctx context.Context, rawIDs []string) ([]model.ReaderInboxBulkResult, error) {
	ids, err := parseReaderInboxBulkIDs(rawIDs)
	if err != nil {
		return nil, err
	}
	items, err := s.inbox.BulkDiscardInbox(ctx, ids)
	if err != nil {
		return nil, mapReaderError(err)
	}
	return items, nil
}

func (s *ReaderVNextService) CreateTodo(ctx context.Context, request dto.ReaderTodoCreateRequest) (dto.ReaderTodoResponse, error) {
	text := strings.TrimSpace(request.Text)
	if text == "" {
		return dto.ReaderTodoResponse{}, problem.NewWithCode(problem.Invalid, "todo_text_required", "todo text is required")
	}
	todo, err := s.todos.CreateTodo(ctx, model.ReaderTodo{Text: text, DueAt: request.DueAt, OriginKind: "standalone"})
	if err != nil {
		return dto.ReaderTodoResponse{}, mapReaderError(err)
	}
	return todoResponse(*todo), nil
}

// ListTodos pages the stored projection and nothing else. It used to parse
// every Thought and published Note first and reconcile the whole projection,
// which made a read scale with the installation and write on every GET.
// Thought and Note commands now maintain the projection as they commit, so the
// stored rows are already the answer.
func (s *ReaderVNextService) ListTodos(ctx context.Context, after string, limit int) (dto.ReaderTodosResponse, error) {
	page, err := s.todos.ListTodos(ctx, after, limit)
	if err != nil {
		return dto.ReaderTodosResponse{}, mapReaderError(err)
	}
	out := dto.ReaderTodosResponse{Items: make([]dto.ReaderTodoResponse, 0, len(page.Items)), NextAfter: page.Next}
	for _, item := range page.Items {
		out.Items = append(out.Items, todoResponse(item))
	}
	return out, nil
}

func todoResponse(item model.ReaderTodo) dto.ReaderTodoResponse {
	return dto.ReaderTodoResponse{ID: item.ID.String(), Text: item.Text, DueAt: item.DueAt, Done: item.Done, OriginKind: item.OriginKind, OriginHostKind: item.OriginHostKind, OriginHostID: item.OriginHostID, OriginRef: item.OriginRef, HostRevision: item.HostRevision, CompletedAt: item.CompletedAt, Expired: item.Expired, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, SourceHref: readerTodoSourceHref(item)}
}

func readerTodoSourceHref(item model.ReaderTodo) *string {
	if item.OriginKind == "standalone" {
		return nil
	}
	var ref map[string]any
	_ = json.Unmarshal(item.OriginRef, &ref)
	stringRef := func(key string) string {
		value, _ := ref[key].(string)
		return strings.TrimSpace(value)
	}
	kind, id := stringRef("source_kind"), stringRef("source_id")
	if kind == "" && item.OriginHostKind != nil {
		kind = strings.TrimSpace(*item.OriginHostKind)
	}
	if id == "" && item.OriginHostID != nil {
		id = strings.TrimSpace(*item.OriginHostID)
	}
	var href string
	switch kind {
	case "link":
		href = "/?view=reading&link_id=" + url.QueryEscape(id)
	case "note":
		href = "/?view=notes&note_id=" + url.QueryEscape(id)
	case "inbox":
		href = "/?view=pending&inbox_id=" + url.QueryEscape(id)
	case "thought":
		href = "/?tool=history&thought_view=live"
	}
	if href == "" || (kind != "thought" && id == "") {
		return nil
	}
	return &href
}

func (s *ReaderVNextService) PatchTodo(ctx context.Context, rawID string, request dto.ReaderTodoPatchRequest) (dto.ReaderTodoResponse, error) {
	id, err := readerUUID(rawID, "todo_id")
	if err != nil {
		return dto.ReaderTodoResponse{}, err
	}
	item, err := s.todos.PatchTodo(ctx, model.ReaderTodoPatch{
		ID:                      id,
		Text:                    request.Text,
		DueAt:                   request.DueAt,
		DueAtSet:                request.DueAtSet || request.DueAt != nil,
		Done:                    request.Done,
		ExpectedHostRevision:    request.ExpectedHostRevision,
		ExpectedHostRevisionSet: request.ExpectedHostRevisionSet || request.ExpectedHostRevision != nil,
	})
	if err != nil {
		return dto.ReaderTodoResponse{}, mapReaderError(err)
	}
	return todoResponse(*item), nil
}

func (s *ReaderVNextService) DeleteTodo(ctx context.Context, rawID string) error {
	id, err := readerUUID(rawID, "todo_id")
	if err != nil {
		return err
	}
	return mapReaderError(s.todos.DeleteTodo(ctx, id))
}

func (s *ReaderVNextService) GetEngagement(ctx context.Context, rawID string) (dto.ReaderEngagementResponse, error) {
	id, err := readerUUID(rawID, "link_id")
	if err != nil {
		return dto.ReaderEngagementResponse{}, err
	}
	item, err := s.library.GetEngagement(ctx, id)
	if err != nil {
		return dto.ReaderEngagementResponse{}, mapReaderError(err)
	}
	return engagementResponse(*item), nil
}

func (s *ReaderVNextService) PatchEngagement(ctx context.Context, rawID string, request dto.ReaderEngagementRequest) (dto.ReaderEngagementResponse, error) {
	id, err := readerUUID(rawID, "link_id")
	if err != nil {
		return dto.ReaderEngagementResponse{}, err
	}
	if request.Read == nil && request.Progress == nil && request.ReadLater == nil {
		return dto.ReaderEngagementResponse{}, problem.NewWithCode(problem.Invalid, "engagement_patch_empty", "at least one engagement field is required")
	}
	if request.Progress != nil && (math.IsNaN(float64(*request.Progress)) || math.IsInf(float64(*request.Progress), 0) || *request.Progress < 0 || *request.Progress > 1) {
		return dto.ReaderEngagementResponse{}, problem.NewWithCode(problem.Invalid, "invalid_progress", "progress must be between 0 and 1")
	}
	item, err := s.library.PatchEngagement(ctx, model.ReaderEngagementPatch{LinkID: id, Read: request.Read, Progress: request.Progress, ReadLater: request.ReadLater})
	if err != nil {
		return dto.ReaderEngagementResponse{}, mapReaderError(err)
	}
	return engagementResponse(*item), nil
}

func engagementResponse(item model.ReaderEngagement) dto.ReaderEngagementResponse {
	return dto.ReaderEngagementResponse{LinkID: item.LinkID.String(), Read: item.Read, Progress: item.Progress, ReadLater: item.ReadLater, LastOpened: item.LastOpened, UpdatedAt: item.UpdatedAt}
}

type readerFeedActionIdentity struct {
	key    string
	kind   string
	source string
	id     uuid.UUID
}

func readerFeedActionIdentityForKey(itemKey string) (readerFeedActionIdentity, error) {
	itemKey = strings.TrimSpace(itemKey)
	kind, rawID, ok := strings.Cut(itemKey, ":")
	if !ok || strings.TrimSpace(rawID) == "" {
		return readerFeedActionIdentity{}, problem.NewWithCode(problem.Invalid, "invalid_feed_item", "feed item key must use a canonical source prefix and UUID")
	}
	id, err := uuid.Parse(rawID)
	if err != nil || itemKey != kind+":"+id.String() {
		return readerFeedActionIdentity{}, problem.NewWithCode(problem.Invalid, "invalid_feed_item", "feed item key must use a canonical source prefix and UUID")
	}
	var source string
	switch kind {
	case "link":
		source = "reading"
	case "inbox":
		source = "inbox"
	case "subscription":
		source = "subscription"
	default:
		return readerFeedActionIdentity{}, problem.NewWithCode(problem.Invalid, "invalid_feed_item", "feed item key must use a canonical source prefix and UUID")
	}
	return readerFeedActionIdentity{key: itemKey, kind: kind, source: source, id: id}, nil
}

func feedItemResponse(item model.ReaderFeedItem) dto.ReaderFeedItemResponse {
	out := dto.ReaderFeedItemResponse{
		Key:         item.Key,
		Source:      item.Source,
		ResourceKey: item.ResourceIdentity(),
		Title:       item.Title,
		Summary:     item.Summary,
		URL:         item.URL,
		Read:        item.Read,
		ReadLater:   item.ReadLater,
		Saved:       item.Saved,
		EventAt:     item.VisibleEventAt(),
	}
	if item.LinkID != nil {
		value := item.LinkID.String()
		out.LinkID = &value
	}
	if item.InboxID != nil {
		value := item.InboxID.String()
		out.InboxID = &value
	}
	if item.FeedItemID != nil {
		value := item.FeedItemID.String()
		out.FeedItemID = &value
	}
	return out
}

// FeedWithSources returns one live mixed-feed page. The cursor carries its mode
// and source filter, so changing either parameter while paging is rejected.
func (s *ReaderVNextService) FeedWithSources(ctx context.Context, mode, after string, sources []string, limit int) (dto.ReaderFeedResponse, error) {
	mode = strings.TrimSpace(mode)
	if err := validateReaderFeedRequestMode(mode); err != nil {
		return dto.ReaderFeedResponse{}, err
	}
	normalizedSources, err := normalizeReaderFeedSources(sources)
	if err != nil {
		return dto.ReaderFeedResponse{}, err
	}
	page, err := s.library.ListFeedWithSources(ctx, mode, after, normalizedSources, limit)
	if err != nil {
		return dto.ReaderFeedResponse{}, mapReaderError(err)
	}
	if page == nil {
		return dto.ReaderFeedResponse{}, problem.NewWithCode(problem.Internal, "reader_feed_unavailable", "reader feed returned no page")
	}
	responseMode := strings.TrimSpace(page.Mode)
	if responseMode == "" {
		responseMode = mode
		if responseMode == "" {
			responseMode = "recommended"
		}
	}
	if responseMode != "recommended" && responseMode != "chronological" {
		return dto.ReaderFeedResponse{}, problem.NewWithCode(problem.Invalid, "invalid_feed_mode", "unsupported feed mode")
	}
	out := dto.ReaderFeedResponse{
		Items:      make([]dto.ReaderFeedItemResponse, 0, len(page.Items)),
		NextCursor: page.NextCursor,
		Mode:       responseMode,
	}
	for _, item := range page.Items {
		out.Items = append(out.Items, feedItemResponse(item))
	}
	return out, nil
}

func validateReaderFeedRequestMode(mode string) error {
	if mode != "" && mode != "recommended" && mode != "chronological" {
		return problem.NewWithCode(problem.Invalid, "invalid_feed_mode", "unsupported feed mode")
	}
	return nil
}

func normalizeReaderFeedSources(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(raw))
	for _, value := range raw {
		for _, part := range strings.Split(value, ",") {
			source := strings.ToLower(strings.TrimSpace(part))
			if source == "" {
				continue
			}
			switch source {
			case "saved":
				source = "reading"
			case "pending":
				source = "inbox"
			case "reading", "inbox", "subscription":
			default:
				return nil, problem.NewWithCode(problem.Invalid, "invalid_feed_source", "unsupported feed source")
			}
			seen[source] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil, nil
	}
	result := make([]string, 0, len(seen))
	for source := range seen {
		result = append(result, source)
	}
	sort.Strings(result)
	return result, nil
}

func (s *ReaderVNextService) FeedbackFeed(ctx context.Context, itemKey, action string) (dto.ReaderFeedFeedbackResponse, error) {
	identity, err := readerFeedActionIdentityForKey(itemKey)
	if err != nil {
		return dto.ReaderFeedFeedbackResponse{}, err
	}
	if action != "hide" && action != "save" && action != "unsave" {
		return dto.ReaderFeedFeedbackResponse{}, problem.NewWithCode(problem.Invalid, "invalid_feed_action", "unsupported feed action")
	}
	if identity.source != "subscription" && (action == "save" || action == "unsave") {
		return dto.ReaderFeedFeedbackResponse{}, problem.NewWithCode(problem.Invalid, "invalid_feed_item", "only subscription items can be saved")
	}
	feedback, err := s.library.FeedbackFeed(ctx, identity.key, action)
	if err = mapReaderError(err); err != nil {
		return dto.ReaderFeedFeedbackResponse{}, err
	}
	response := dto.ReaderFeedFeedbackResponse{ItemKey: feedback.ItemKey, Action: feedback.Action}
	if feedback.LinkID != nil {
		linkID := feedback.LinkID.String()
		response.LinkID = &linkID
	}
	return response, nil
}

func (s *ReaderVNextService) Home(ctx context.Context) (dto.ReaderHomeResponse, error) {
	return s.HomeAggregate(ctx)
}

func (s *ReaderVNextService) RelatedTags(ctx context.Context, rawLinkID string, limit int) (dto.ReaderRelatedTagsResponse, error) {
	var linkID *uuid.UUID
	if strings.TrimSpace(rawLinkID) != "" {
		id, err := readerUUID(rawLinkID, "link_id")
		if err != nil {
			return dto.ReaderRelatedTagsResponse{}, err
		}
		linkID = &id
	}
	items, err := s.library.RelatedTags(ctx, linkID, limit)
	if err != nil {
		return dto.ReaderRelatedTagsResponse{}, mapReaderError(err)
	}
	return dto.ReaderRelatedTagsResponse{Items: items}, nil
}

func (s *ReaderVNextService) Activity(ctx context.Context, rawKind, rawAfter string, limit int) (dto.ReaderActivityResponse, error) {
	kind, err := normalizeReaderActivityKind(rawKind)
	if err != nil {
		return dto.ReaderActivityResponse{}, mapReaderError(err)
	}
	after, err := s.decodeReaderActivityCursor(ctx, kind, rawAfter)
	if err != nil {
		return dto.ReaderActivityResponse{}, mapReaderError(err)
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	page, err := s.library.ListActivity(ctx, model.ReaderActivityQuery{Kind: kind, After: after, Limit: limit})
	if err != nil {
		return dto.ReaderActivityResponse{}, mapReaderError(err)
	}
	out := dto.ReaderActivityResponse{
		Kind:    kind,
		Tags:    make([]dto.ReaderTagActivityResponse, 0, len(page.Items)),
		Domains: make([]dto.ReaderDomainActivityResponse, 0, len(page.Items)),
	}
	for _, item := range page.Items {
		switch item.Kind {
		case model.ReaderActivityKindTag:
			out.Tags = append(out.Tags, dto.ReaderTagActivityResponse{Tag: item.Key, LastAt: item.LastAt})
		case model.ReaderActivityKindDomain:
			out.Domains = append(out.Domains, dto.ReaderDomainActivityResponse{Domain: item.Key, LastAt: item.LastAt})
		default:
			return dto.ReaderActivityResponse{}, mapReaderError(fmt.Errorf("%w: invalid activity row kind", repository.ErrInvalidReaderCursor))
		}
	}
	if page.HasMore {
		if len(page.Items) == 0 {
			return dto.ReaderActivityResponse{}, mapReaderError(fmt.Errorf("%w: empty activity continuation page", repository.ErrInvalidReaderCursor))
		}
		out.NextCursor = s.encodeReaderActivityCursor(ctx, kind, page.Items[len(page.Items)-1])
	}
	return out, nil
}
func (s *ReaderVNextService) PatchLinkMetadata(ctx context.Context, rawID string, request dto.ReaderLinkMetadataRequest, expected int64) (dto.ReaderLinkMetadataResponse, error) {
	id, err := readerUUID(rawID, "link_id")
	if err != nil {
		return dto.ReaderLinkMetadataResponse{}, err
	}
	if !request.Complete() {
		return dto.ReaderLinkMetadataResponse{}, problem.NewWithCode(problem.Invalid, problem.CodeMetadataFieldsRequired, "title, summary, and tags are required")
	}
	if err := validateLinkMetadataRequest(&request); err != nil {
		return dto.ReaderLinkMetadataResponse{}, err
	}
	update, err := s.library.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{LinkID: id, Title: request.Title, Summary: request.Summary, Tags: request.Tags, ExpectedRevision: expected})
	if err != nil {
		if errors.Is(err, repository.ErrRevisionConflict) {
			return dto.ReaderLinkMetadataResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeMetadataRevisionConflict, "link metadata revision is stale")
		}
		return dto.ReaderLinkMetadataResponse{}, mapReaderError(err)
	}
	if update.MetadataRevision < 1 || update.MetadataRevision > model.LinkMetadataMaxRevision {
		return dto.ReaderLinkMetadataResponse{}, problem.NewWithCode(problem.Conflict, problem.CodeMetadataRevisionConflict, "link metadata revision is outside the JavaScript-safe range")
	}
	return dto.ReaderLinkMetadataResponse{LinkID: id.String(), MetadataRevision: update.MetadataRevision}, nil
}

const (
	maxLinkMetadataTitleRunes   = 512
	maxLinkMetadataSummaryRunes = 4096
	maxLinkMetadataTags         = 50
	maxLinkMetadataTagRunes     = 64
)

func validateLinkMetadataRequest(request *dto.ReaderLinkMetadataRequest) error {
	if request.Title != nil && utf8.RuneCountInString(*request.Title) > maxLinkMetadataTitleRunes {
		return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "title exceeds 512 characters")
	}
	if request.Summary != nil && utf8.RuneCountInString(*request.Summary) > maxLinkMetadataSummaryRunes {
		return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "summary exceeds 4096 characters")
	}
	if request.Tags == nil {
		return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "tags must be an array")
	}
	if len(request.Tags) > maxLinkMetadataTags {
		return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "tags may contain at most 50 items")
	}

	folder := cases.Fold()
	seen := make(map[string]struct{}, len(request.Tags))
	tags := make([]string, 0, len(request.Tags))
	for _, raw := range request.Tags {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "tags must not contain empty values")
		}
		if utf8.RuneCountInString(tag) > maxLinkMetadataTagRunes {
			return problem.NewWithCode(problem.Invalid, problem.CodeInvalidLinkMetadata, "tags may not exceed 64 characters")
		}
		key := folder.String(tag)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		tags = append(tags, tag)
	}
	request.Tags = tags
	return nil
}

// validateReaderAIRequest normalises and bounds the request before any saved
// content is resolved, keeping the wire contract deterministic when AI is off.
func validateReaderAIRequest(ctx context.Context, request dto.ReaderAIRequest) (prompt, scope, selected string, err error) {
	prompt = strings.TrimSpace(request.Prompt)
	if prompt == "" {
		return "", "", "", problem.NewWithCode(problem.Invalid, "ai_prompt_required", "prompt is required")
	}
	scope = strings.TrimSpace(request.Scope)
	if scope == "" {
		scope = "general"
	}
	if scope != "general" && scope != "selection" && scope != "thought" {
		return "", "", "", problem.NewWithCode(problem.Invalid, "ai_scope_invalid", "unsupported AI scope")
	}
	selected = strings.TrimSpace(request.SelectedText)
	if scope == "selection" && selected == "" {
		return "", "", "", problem.NewWithCode(problem.Invalid, "ai_selection_required", "selected text is required for selection scope")
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", "", mapReaderAIError(ctxErr)
	}
	return prompt, scope, selected, nil
}

// composeReaderAIPrompt appends the untrusted selection and link context and
// enforces the context bound.
func (s *ReaderVNextService) composeReaderAIPrompt(ctx context.Context, request dto.ReaderAIRequest, prompt, selected string) (string, error) {
	var linkContext *model.ReaderAIContext
	if rawLinkID := strings.TrimSpace(request.LinkID); rawLinkID != "" {
		linkID, err := readerUUID(rawLinkID, "link_id")
		if err != nil {
			return "", err
		}
		linkContext, err = s.library.GetAIContext(ctx, linkID)
		if err != nil {
			return "", mapReaderAIError(mapReaderError(err))
		}
	}
	if selected != "" {
		prompt += "\n\nSelected text (untrusted context):\n" + selected
	}
	if linkContext != nil {
		prompt += "\n\nReader link context (untrusted context):\n" + readerAIContextText(*linkContext)
	}
	if len([]rune(prompt)) > 16000 {
		return "", problem.NewWithCode(problem.TooLarge, "ai_context_too_large", "AI context is too large")
	}
	return prompt, nil
}

func (s *ReaderVNextService) completeReaderAI(ctx context.Context, prompt, scope string) (answer, modelName string, err error) {
	answer, modelName, err = s.ai.Complete(ctx, prompt, scope)
	if err != nil {
		return "", "", mapReaderAIError(err)
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "", "", mapReaderAIError(errReaderAIEmptyResponse)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", mapReaderAIError(ctxErr)
	}
	return answer, modelName, nil
}

func (s *ReaderVNextService) CompleteAI(ctx context.Context, request dto.ReaderAIRequest) (dto.ReaderAIResponse, error) {
	prompt, scope, selected, err := validateReaderAIRequest(ctx, request)
	if err != nil {
		return dto.ReaderAIResponse{}, err
	}
	// Capability-off is an explicit privacy boundary: do not resolve a link
	// identity into content, tags, or thoughts when no provider is available.
	// Basic request validation above remains useful to keep the wire contract
	// deterministic without touching saved content.
	if s.ai == nil {
		return dto.ReaderAIResponse{Enabled: false}, nil
	}
	prompt, err = s.composeReaderAIPrompt(ctx, request, prompt, selected)
	if err != nil {
		return dto.ReaderAIResponse{}, err
	}
	answer, modelName, err := s.completeReaderAI(ctx, prompt, scope)
	if err != nil {
		return dto.ReaderAIResponse{}, err
	}
	return dto.ReaderAIResponse{Enabled: true, Answer: answer, Model: strings.TrimSpace(modelName)}, nil
}

func readerAIContextText(context model.ReaderAIContext) string {
	var builder strings.Builder
	if context.Content != "" {
		builder.WriteString("Content:\n")
		builder.WriteString(context.Content)
		builder.WriteString("\n")
	}
	if context.Summary != "" {
		builder.WriteString("Summary:\n")
		builder.WriteString(context.Summary)
		builder.WriteString("\n")
	}
	if len(context.Tags) > 0 {
		builder.WriteString("Tags: ")
		for index, tag := range context.Tags {
			if index >= 32 {
				break
			}
			if index > 0 {
				builder.WriteString(", ")
			}
			builder.WriteString(string([]rune(tag)[:minReaderRunes(tag, 128)]))
		}
		builder.WriteString("\n")
	}
	for index, thought := range context.Thoughts {
		if index >= 8 {
			break
		}
		builder.WriteString("Thought ")
		builder.WriteString(strconv.Itoa(index + 1))
		builder.WriteString(": ")
		builder.WriteString(thought.Body)
		builder.WriteString("\n")
	}
	return boundReaderAIContext(builder.String())
}

func minReaderRunes(value string, maximum int) int {
	count := len([]rune(value))
	if count > maximum {
		return maximum
	}
	return count
}
