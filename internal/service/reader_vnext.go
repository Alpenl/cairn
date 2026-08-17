package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/text/cases"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/notetitle"
	"webtag/internal/repository"
)

// ReaderAIBackend is deliberately tiny. The Reader service owns scope and
// context construction; the backend owns the provider transport and can be
// disabled without making the rest of Reader unavailable.
type ReaderAIBackend interface {
	Complete(context.Context, string, string) (answer string, modelName string, err error)
}

type ReaderVNextService struct {
	store             repository.ReaderVNextStore
	ai                ReaderAIBackend
	inboxAI           ReaderInboxAIBackend
	inboxScheduler    ReaderInboxJobScheduler
	inboxCommands     InboxProposalCommands
	metadataCache     CacheInvalidator
	now               func() time.Time
	activityMu        sync.Mutex
	activityLast      time.Time
	activityCursorKey []byte
}

// readerFeedSourceStore is an additive capability. Keeping the original
// ReaderVNextStore.ListFeed method intact lets older service test doubles keep
// compiling while the mixed Feed gains a source-filtered snapshot path.
type readerFeedSourceStore interface {
	ListFeedWithSources(context.Context, string, string, string, []string, int) (*model.ReaderFeedPage, error)
}

type ReaderVNextServiceOptions struct {
	CursorSigningKey string
}

func NewReaderVNextService(store repository.ReaderVNextStore, ai ReaderAIBackend, options ...ReaderVNextServiceOptions) *ReaderVNextService {
	cursorKey := processReaderCursorKey
	if len(options) > 0 && options[0].CursorSigningKey != "" {
		cursorKey = []byte(options[0].CursorSigningKey)
	}
	return &ReaderVNextService{
		store:             store,
		ai:                ai,
		now:               time.Now,
		activityCursorKey: append([]byte(nil), cursorKey...),
	}
}

// ConfigureMetadataCacheInvalidator installs the installation-level aggregate
// invalidator shared by the Link metadata command and normal Link writers.
// It is optional for lightweight tests and deployments without in-memory
// aggregate caches.
func (s *ReaderVNextService) ConfigureMetadataCacheInvalidator(invalidator CacheInvalidator) {
	s.metadataCache = invalidator
}

func readerUUID(raw, field string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_"+field, field+" must be a UUID")
	}
	return id, nil
}

func parseReaderInboxPartition(raw string, defaultActive bool) (model.ReaderInboxPartition, error) {
	partition := model.ReaderInboxPartition(strings.TrimSpace(raw))
	if partition == "" && defaultActive {
		return model.ReaderInboxPartitionActive, nil
	}
	if !partition.Valid() {
		return "", httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_inbox_partition", "partition must be active or expired")
	}
	return partition, nil
}

// readerErrorMapping binds a set of repository sentinels to one HTTP shape.
// Several sentinels may share an entry when they describe the same client-facing
// fault.
type readerErrorMapping struct {
	targets []error
	status  int
	code    string
	message string
}

// readerErrorMappings is scanned top to bottom, so the slice order is the
// contract: an error wrapping more than one sentinel keeps the HTTP shape of the
// earliest entry it matches. Reordering entries is a behaviour change, not a
// cosmetic one.
var readerErrorMappings = []readerErrorMapping{
	{targets: []error{repository.ErrNotFound}, status: http.StatusNotFound, code: "reader_not_found", message: "reader resource not found"},
	{targets: []error{repository.ErrReaderThoughtReattachInvalidState}, status: http.StatusConflict, code: "thought_reattach_invalid_state", message: "thought is not a historical tombstone that can be reattached"},
	{targets: []error{repository.ErrReaderHostNotTrashed}, status: http.StatusConflict, code: "host_not_trashed", message: "reader host must be trashed before permanent purge"},
	{targets: []error{repository.ErrReaderTodoHostRevisionNotApplicable}, status: http.StatusUnprocessableEntity, code: "todo_host_revision_not_applicable", message: "expected_host_revision is not applicable to standalone TODOs"},
	{targets: []error{repository.ErrRevisionConflict}, status: http.StatusConflict, code: httperr.CodeRevisionConflict, message: "resource revision is stale"},
	{targets: []error{repository.ErrInvalidReaderHostKind}, status: http.StatusUnprocessableEntity, code: "invalid_host_kind", message: "host_kind must be link, inbox, or note"},
	{targets: []error{repository.ErrInvalidReaderCursor}, status: http.StatusUnprocessableEntity, code: httperr.CodeInvalidCursor, message: "invalid cursor"},
	{targets: []error{repository.ErrInvalidReaderFeedReason}, status: http.StatusUnprocessableEntity, code: httperr.CodeInvalidFeedReason, message: "feed reason evidence is incomplete"},
	{targets: []error{repository.ErrInvalidReaderReanchor}, status: http.StatusUnprocessableEntity, code: "invalid_reanchor_ops", message: "invalid reanchor operations"},
	{targets: []error{repository.ErrReaderNoteContentEmpty}, status: http.StatusUnprocessableEntity, code: "note_content_empty", message: "note content must not be empty"},
	{targets: []error{repository.ErrReaderNoteDraftDirty}, status: http.StatusConflict, code: "note_draft_dirty", message: "note draft has unpublished changes"},
	{targets: []error{repository.ErrReaderNoteReanchorIncomplete}, status: http.StatusUnprocessableEntity, code: "note_reanchor_incomplete", message: "reanchor operations must exactly match active note thoughts"},
	{targets: []error{repository.ErrInvalidReaderFeedItem}, status: http.StatusUnprocessableEntity, code: "invalid_feed_item", message: "invalid feed item"},
	{targets: []error{repository.ErrReaderTodoProjectionImmutable}, status: http.StatusUnprocessableEntity, code: "todo_projection_immutable", message: "projected TODO text and due date are source-owned"},
	{targets: []error{repository.ErrReaderTodoHostMissing}, status: http.StatusConflict, code: "todo_host_missing", message: "the TODO source is no longer available"},
	{targets: []error{repository.ErrReaderTodoAnchorNotFound}, status: http.StatusConflict, code: "todo_anchor_not_found", message: "the TODO source block is no longer available"},
	{targets: []error{repository.ErrReaderTodoAnchorAmbiguous}, status: http.StatusConflict, code: "todo_anchor_ambiguous", message: "the TODO source block is ambiguous"},
	{targets: []error{repository.ErrReaderThoughtOpConflict}, status: http.StatusConflict, code: "thought_op_conflict", message: "the operation id was already used with different content"},
	{targets: []error{repository.ErrReaderThoughtRecoveryConflict}, status: http.StatusConflict, code: "thought_recovery_conflict", message: "the current thought winner changed before recovery"},
	{targets: []error{repository.ErrReaderThoughtClockExhausted}, status: http.StatusConflict, code: "thought_clock_exhausted", message: "the thought logical clock is exhausted"},
	{targets: []error{repository.ErrReaderThoughtClockInvalid}, status: http.StatusUnprocessableEntity, code: "invalid_thought_clock", message: "the thought logical clock is invalid"},
	{targets: []error{repository.ErrReaderThoughtLinkMismatch, repository.ErrInvalidReaderThought}, status: http.StatusUnprocessableEntity, code: "invalid_thought", message: "thought host and payload are inconsistent"},
	{targets: []error{repository.ErrReaderInboxTitleRequired}, status: http.StatusUnprocessableEntity, code: "inbox_title_required", message: "inbox title must not be blank when confirming"},
	{targets: []error{repository.ErrReaderInboxStateConflict}, status: http.StatusConflict, code: "inbox_state_conflict", message: "the inbox item is in a state that cannot accept this transition"},
	{targets: []error{repository.ErrInvalidReaderCategoryMembership}, status: http.StatusUnprocessableEntity, code: "invalid_category_membership", message: "category membership identity is invalid"},
}

// mapReaderError translates repository sentinels into HTTP errors. Anything the
// table does not recognise is returned untouched so callers never lose an
// unexpected cause.
func mapReaderError(err error) error {
	if err == nil {
		return nil
	}
	for _, mapping := range readerErrorMappings {
		for _, target := range mapping.targets {
			if errors.Is(err, target) {
				return httperr.NewWithCode(mapping.status, mapping.code, mapping.message)
			}
		}
	}
	return err
}

func (s *ReaderVNextService) PushThoughtOps(ctx context.Context, request dto.ReaderThoughtOpsRequest) ([]dto.ReaderThoughtAckResponse, error) {
	if len(request.Ops) == 0 || len(request.Ops) > 200 {
		return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_ops", "thought operation batch must contain between 1 and 200 operations")
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
	acks, err := s.store.AppendThoughtOps(ctx, ops)
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
		return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_op", message)
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
	if err := validateThoughtLogicalClock(input.ContractVersion, input.LogicalClock); err != nil {
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
		return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_payload", "thought target and payload must be JSON")
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
	if contractVersion != 0 && contractVersion != model.ReaderThoughtContractVersion {
		return httperr.NewWithCode(http.StatusUnprocessableEntity, "unsupported_thought_contract", "unsupported thought contract_version")
	}
	return nil
}

func validateThoughtLogicalClock(contractVersion int, logicalClock int64) error {
	if contractVersion == 0 {
		if logicalClock != 0 {
			return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_clock", "legacy thought operations must use logical_clock 0")
		}
		return nil
	}
	if logicalClock < 1 || logicalClock > model.ReaderThoughtMaxLogicalClock {
		return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_clock", "logical_clock must be a safe positive integer")
	}
	return nil
}

func validateThoughtOperationKind(operationKind string) error {
	if operationKind != "add" && operationKind != "update" && operationKind != "delete" {
		return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_op", "unsupported thought operation")
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
		SourceKey        string `json:"source_key"`
	} `json:"version"`
}

func readerThoughtTarget(input dto.ReaderThoughtOpRequest) (readerThoughtTargetWire, error) {
	var target readerThoughtTargetWire
	if err := json.Unmarshal(input.Target, &target); err != nil {
		return readerThoughtTargetWire{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_target", "thought target must be an object")
	}
	if strings.TrimSpace(target.HostID) == "" || target.HostID != input.HostID {
		return readerThoughtTargetWire{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_target", "thought target host_id must match host_id")
	}
	switch target.Kind {
	case "saved-content":
		if target.Version.ContentRevision <= 0 {
			return readerThoughtTargetWire{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_target", "saved-content target requires content_revision")
		}
	case "summary":
		if strings.TrimSpace(target.Version.SourceHash) == "" {
			return readerThoughtTargetWire{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_target", "summary target requires source_hash")
		}
	case "note":
		if target.Version.NoteRevision <= 0 {
			return readerThoughtTargetWire{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_target", "note target requires note_revision")
		}
	case "inbox":
		if target.Version.MetadataRevision <= 0 {
			return readerThoughtTargetWire{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_target", "inbox target requires metadata_revision")
		}
	case "legacy-stale":
		if strings.TrimSpace(target.Version.SourceKey) == "" {
			return readerThoughtTargetWire{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_target", "legacy-stale target requires source_key")
		}
	default:
		return readerThoughtTargetWire{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_target", "unsupported thought target kind")
	}
	return target, nil
}

func validateThoughtTarget(input dto.ReaderThoughtOpRequest) error {
	_, err := readerThoughtTarget(input)
	return err
}

func invalidThoughtReattachPayload() error {
	return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_payload", "reattach operation payload is invalid")
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
		return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_target", "reattach target must match the destination host revision")
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
		return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_payload", "thought payload must be an object")
	}
	if len(payload.Quote) == 0 {
		if input.OperationKind == "delete" {
			return nil
		}
		return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_payload", "thought payload requires a JSON quote")
	}
	if !json.Valid(payload.Quote) {
		return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_payload", "thought payload quote must be valid JSON")
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
		return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_recovery", "recovery_of and expected_current_winner_key must appear together")
	}
	for _, key := range []*dto.ReaderThoughtVersionKeyResponse{input.RecoveryOf, input.ExpectedCurrentWinnerKey} {
		if key.LogicalClock < 0 || key.LogicalClock > model.ReaderThoughtMaxLogicalClock ||
			validateThoughtBoundedField(key.DeviceID, 128, "thought recovery device_id is invalid") != nil ||
			validateThoughtBoundedField(key.OpID, 128, "thought recovery op_id is invalid") != nil {
			return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_recovery", "thought recovery keys are invalid")
		}
	}
	return nil
}

func (s *ReaderVNextService) ListThoughts(ctx context.Context, query, after string, limit int) (dto.ReaderThoughtsResponse, error) {
	// 与 read_search / library_search 一致地夹住 ?q=：这条路径直接落到
	// content_text 上的 ILIKE 全表模式匹配，不设上限时一个几 KB 的
	// %_%_%_… 就能打满一个核。
	if len([]rune(query)) > maxListQueryLen {
		return dto.ReaderThoughtsResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeQueryTooLong, "search query too long")
	}
	items, next, err := s.store.ListThoughts(ctx, query, after, limit)
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
	items, next, err := s.store.ListThoughtHistory(ctx, after, limit)
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
	items, next, err := s.store.ListThoughtsSince(ctx, after, limit)
	if err != nil {
		return dto.ReaderThoughtsResponse{}, mapReaderError(err)
	}
	out := dto.ReaderThoughtsResponse{ContractVersion: model.ReaderThoughtContractVersion, Items: make([]dto.ReaderThoughtResponse, 0, len(items)), NextCursor: next}
	for _, item := range items {
		out.Items = append(out.Items, thoughtResponse(item))
	}
	return out, nil
}

type readerThoughtConflictStore interface {
	ListThoughtConflicts(context.Context, string, int) ([]model.ReaderThoughtConflict, string, error)
}

func (s *ReaderVNextService) ListThoughtConflicts(ctx context.Context, after string, limit int) (dto.ReaderThoughtConflictsResponse, error) {
	store, ok := s.store.(readerThoughtConflictStore)
	if !ok {
		return dto.ReaderThoughtConflictsResponse{ContractVersion: model.ReaderThoughtContractVersion, Items: []dto.ReaderThoughtConflictResponse{}}, nil
	}
	items, next, err := store.ListThoughtConflicts(ctx, after, limit)
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
	item, err := s.store.GetThought(ctx, rawID)
	if err != nil {
		return dto.ReaderThoughtResponse{}, mapReaderError(err)
	}
	return thoughtResponse(*item), nil
}

func (s *ReaderVNextService) ReattachThought(ctx context.Context, rawID string, request dto.ReaderThoughtReattachRequest) (dto.ReaderThoughtResponse, error) {
	if request.TargetHostKind != "link" && request.TargetHostKind != "note" && request.TargetHostKind != "inbox" {
		return dto.ReaderThoughtResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_thought_host", "target_host_kind must be link, note, or inbox")
	}
	targetHostID, err := readerUUID(request.TargetHostID, "target_host_id")
	if err != nil {
		return dto.ReaderThoughtResponse{}, err
	}
	if request.ExpectedLastSequence < 0 {
		return dto.ReaderThoughtResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "reader_last_sequence_invalid", "expected_last_sequence must be non-negative")
	}
	if request.ExpectedHostRevision <= 0 {
		return dto.ReaderThoughtResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "reader_host_revision_required", "expected_host_revision must be positive")
	}
	item, err := s.store.ReattachThought(ctx, model.ReaderThoughtReattachCommand{ThoughtID: strings.TrimSpace(rawID), TargetHostKind: request.TargetHostKind, TargetHostID: targetHostID.String(), ExpectedLastSequence: request.ExpectedLastSequence, ExpectedHostRevision: request.ExpectedHostRevision})
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
	created, err := s.store.CreateNote(ctx, note)
	if err != nil {
		return dto.ReaderNoteResponse{}, mapReaderError(err)
	}
	return noteResponse(*created), nil
}

func (s *ReaderVNextService) ListNotes(ctx context.Context, after string, limit int) (dto.ReaderNotesResponse, error) {
	items, count, next, err := s.store.ListNotes(ctx, after, limit)
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
	note, err := s.store.GetNote(ctx, id)
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
	note, err := s.store.SaveNoteDraft(ctx, model.ReaderNoteDraftCommand{NoteID: id, Content: request.Content, ExpectedDraftRevision: request.ExpectedDraftRevision})
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
		return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_draft_revision", "draft revision must be positive")
	}
	return mapReaderError(s.store.DiscardNoteDraft(ctx, model.ReaderNoteDiscardDraftCommand{NoteID: id, ExpectedDraftRevision: expectedRevision}))
}

func (s *ReaderVNextService) PublishNote(ctx context.Context, rawID string, request dto.ReaderNotePublishRequest) (dto.ReaderNoteResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	if request.ExpectedDraftRevision == nil || request.ExpectedPublishedRevision == nil {
		return dto.ReaderNoteResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "note_revision_required", "draft and published revisions are required")
	}
	if err := validateReaderReanchorOps(request.ReanchorOps); err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	note, err := s.store.PublishNote(ctx, model.ReaderNotePublishCommand{NoteID: id, ExpectedDraftRevision: *request.ExpectedDraftRevision, ExpectedPublishedRevision: *request.ExpectedPublishedRevision, ReanchorOps: request.ReanchorOps})
	if err != nil {
		return dto.ReaderNoteResponse{}, mapReaderError(err)
	}
	return noteResponse(*note), nil
}

func validateReaderReanchorOps(ops []json.RawMessage) error {
	if len(ops) > 500 {
		return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_reanchor_ops", "too many reanchor operations")
	}
	for _, raw := range ops {
		if len(raw) == 0 || len(raw) > 128*1024 || !json.Valid(raw) {
			return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_reanchor_ops", "reanchor operation must be valid bounded JSON")
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_reanchor_ops", "reanchor operation must be a JSON object")
		}
	}
	return nil
}

func (s *ReaderVNextService) DeleteNote(ctx context.Context, rawID string) (dto.ReaderHostLifecycleResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	if lifecycle, ok := s.store.(repository.ReaderHostLifecycleStore); ok {
		result, lifecycleErr := lifecycle.SoftDeleteHost(ctx, model.ReaderHostNote, id)
		if lifecycleErr != nil {
			return dto.ReaderHostLifecycleResponse{}, mapReaderError(lifecycleErr)
		}
		return readerHostLifecycleResponse(result), nil
	}
	if err := mapReaderError(s.store.DeleteNote(ctx, id)); err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	return dto.ReaderHostLifecycleResponse{HostKind: string(model.ReaderHostNote), HostID: id.String(), State: string(model.ReaderHostTrashed), Changed: true}, nil
}

func (s *ReaderVNextService) RestoreNote(ctx context.Context, rawID string) (dto.ReaderHostLifecycleResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	if lifecycle, ok := s.store.(repository.ReaderHostLifecycleStore); ok {
		result, lifecycleErr := lifecycle.RestoreHost(ctx, model.ReaderHostNote, id)
		if lifecycleErr != nil {
			return dto.ReaderHostLifecycleResponse{}, mapReaderError(lifecycleErr)
		}
		return readerHostLifecycleResponse(result), nil
	}
	if err := mapReaderError(s.store.RestoreNote(ctx, id)); err != nil {
		return dto.ReaderHostLifecycleResponse{}, err
	}
	return dto.ReaderHostLifecycleResponse{HostKind: string(model.ReaderHostNote), HostID: id.String(), State: string(model.ReaderHostLive), Changed: true}, nil
}

func (s *ReaderVNextService) ListNoteHistory(ctx context.Context, rawID string, limit int) ([]dto.ReaderNoteHistoryResponse, error) {
	id, err := readerUUID(rawID, "note_id")
	if err != nil {
		return nil, err
	}
	items, err := s.store.ListNoteHistory(ctx, id, limit)
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
		return dto.ReaderNoteResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "note_revision_required", "draft and published revisions are required")
	}
	if err := validateReaderReanchorOps(request.ReanchorOps); err != nil {
		return dto.ReaderNoteResponse{}, err
	}
	note, err := s.store.RestoreNoteRevision(ctx, model.ReaderNoteRestoreCommand{NoteID: id, Revision: revision, ExpectedDraftRevision: *request.ExpectedDraftRevision, ExpectedPublishedRevision: *request.ExpectedPublishedRevision, ReanchorOps: request.ReanchorOps})
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
	input := model.ReaderInbox{URL: strings.TrimSpace(request.URL), IdentityKey: normalizedURL, SourceKind: request.SourceKind, Title: request.Title, Body: request.Body, Note: request.Note, Summary: request.Summary, Tags: append([]string(nil), request.Tags...), ProposalSignals: []byte(`{}`), ProposalStatus: "pending"}
	var item *model.ReaderInbox
	if s.inboxCommands != nil {
		result, commandErr := s.inboxCommands.CreateInboxProposal(ctx, CreateInboxProposalCommand{Inbox: input})
		if commandErr != nil {
			return dto.ReaderInboxResponse{}, mapReaderError(commandErr)
		}
		item = result.Inbox
	} else {
		item, err = s.store.CreateInbox(ctx, input)
		if err != nil {
			return dto.ReaderInboxResponse{}, mapReaderError(err)
		}
	}
	if item == nil {
		return dto.ReaderInboxResponse{}, errors.New("create Reader inbox: durable command returned nil item")
	}
	// Production installs a scheduler during dependency construction. Creating
	// the durable row and queue message together keeps ordinary capture writes
	// on the proposal path without making offline/test callers depend on River.
	if s.inboxCommands == nil && s.inboxScheduler != nil {
		job, created, beginErr := s.store.BeginInboxResummarizeJob(ctx, item.ID, item.MetadataRevision)
		if beginErr != nil {
			return dto.ReaderInboxResponse{}, mapReaderError(beginErr)
		}
		if created {
			args := ReaderInboxSummaryJobArgs{JobID: job.ID, InboxID: item.ID, ExpectedMetadataRevision: job.ExpectedMetadataRevision}
			if enqueueErr := s.inboxScheduler.EnqueueReaderInboxSummary(ctx, args); enqueueErr != nil {
				_ = s.store.FailInboxJob(ctx, job.ID, "reader_inbox_job_enqueue_failed")
				return dto.ReaderInboxResponse{}, fmt.Errorf("enqueue Reader inbox job: %w", enqueueErr)
			}
		}
	}
	return inboxResponse(*item), nil
}

func (s *ReaderVNextService) ListInbox(ctx context.Context, rawPartition, after string, limit int) (dto.ReaderInboxResponsePage, error) {
	partition, err := parseReaderInboxPartition(rawPartition, true)
	if err != nil {
		return dto.ReaderInboxResponsePage{}, err
	}
	items, activeCount, expiredCount, next, err := s.store.ListInbox(ctx, partition, after, limit)
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
	item, err := s.store.GetInbox(ctx, id)
	if err != nil {
		return dto.ReaderInboxResponse{}, mapReaderError(err)
	}
	return inboxResponse(*item), nil
}

func inboxResponse(item model.ReaderInbox) dto.ReaderInboxResponse {
	categoryIDs := make([]string, 0, len(item.CategoryIDs))
	for _, categoryID := range item.CategoryIDs {
		categoryIDs = append(categoryIDs, categoryID.String())
	}
	out := dto.ReaderInboxResponse{ID: item.ID.String(), URL: item.URL, SourceKind: item.SourceKind, Title: item.Title, Body: item.Body, Note: item.Note, Summary: item.Summary, SuggestedTags: item.SuggestedTags, ProposalSignals: item.ProposalSignals, ProposalStatus: item.ProposalStatus, Tags: item.Tags, CategoryIDs: categoryIDs, Status: item.Status, MetadataRevision: item.MetadataRevision, ExpiresAt: item.ExpiresAt, ExpiredAt: item.ExpiredAt, Expired: item.Expired, DeletedAt: item.DeletedAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
	if item.JobID != nil {
		id := item.JobID.String()
		out.JobID = &id
	}
	return out
}

func (s *ReaderVNextService) PatchInbox(ctx context.Context, rawID string, request dto.ReaderInboxPatchRequest, expected int64) (dto.ReaderInboxResponse, error) {
	id, err := readerUUID(rawID, "inbox_id")
	if err != nil {
		return dto.ReaderInboxResponse{}, err
	}
	item, err := s.store.PatchInbox(ctx, model.ReaderInboxPatch{ID: id, Title: request.Title, Body: request.Body, Note: request.Note, Summary: request.Summary, Tags: request.Tags, ExpectedRevision: expected})
	if err != nil {
		return dto.ReaderInboxResponse{}, mapReaderError(err)
	}
	return inboxResponse(*item), nil
}

type readerInboxConfirmationCASStore interface {
	ConfirmInboxCAS(context.Context, uuid.UUID, int64) (uuid.UUID, error)
}

func (s *ReaderVNextService) ConfirmInbox(ctx context.Context, rawID string, expectedRevision int64) (map[string]string, error) {
	id, err := readerUUID(rawID, "inbox_id")
	if err != nil {
		return nil, err
	}
	item, err := s.store.GetInbox(ctx, id)
	if err != nil {
		return nil, mapReaderError(err)
	}
	if item.Title == nil || strings.TrimSpace(*item.Title) == "" {
		return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, "inbox_title_required", "inbox title must not be blank when confirming")
	}
	if expectedRevision >= 0 && item.MetadataRevision != expectedRevision {
		return nil, mapReaderError(repository.ErrRevisionConflict)
	}
	var linkID uuid.UUID
	if store, ok := s.store.(readerInboxConfirmationCASStore); ok && expectedRevision >= 0 {
		linkID, err = store.ConfirmInboxCAS(ctx, id, expectedRevision)
	} else {
		linkID, err = s.store.ConfirmInbox(ctx, id)
	}
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
	if lifecycle, ok := s.store.(repository.ReaderHostLifecycleStore); ok {
		_, err = lifecycle.SoftDeleteHost(ctx, model.ReaderHostInbox, id)
		return mapReaderError(err)
	}
	_, err = s.store.UpdateInboxStatus(ctx, id, "discarded")
	return mapReaderError(err)
}

// RestoreInbox is an Inbox-specific lifecycle command because an expired live
// row is not trashed. The repository atomically renews only expired pending
// rows and leaves all user/AI-owned content untouched.
func (s *ReaderVNextService) RestoreInbox(ctx context.Context, rawID string) error {
	id, err := readerUUID(rawID, "inbox_id")
	if err != nil {
		return err
	}
	return mapReaderError(s.store.RestoreInbox(ctx, id))
}

// ConfirmAIProposals confirms the next stable server-selected set of completed
// AI proposals. The client supplies only the partition; eligibility and the
// atomic transition stay at the repository boundary.
func (s *ReaderVNextService) ConfirmAIProposals(ctx context.Context, rawPartition string) (dto.ReaderInboxConfirmAIProposalsResponse, error) {
	partition, err := parseReaderInboxPartition(rawPartition, false)
	if err != nil {
		return dto.ReaderInboxConfirmAIProposalsResponse{}, err
	}
	confirmation, err := s.store.ConfirmAIProposals(ctx, partition)
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
	lifecycle, ok := s.store.(repository.ReaderHostLifecycleStore)
	if !ok {
		return dto.ReaderHostLifecycleResponse{}, errors.New("reader host lifecycle is not configured")
	}
	result, err := lifecycle.RestoreHost(ctx, kind, id)
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
	lifecycle, ok := s.store.(repository.ReaderHostLifecycleStore)
	if !ok {
		return errors.New("reader host lifecycle is not configured")
	}
	return mapReaderError(lifecycle.PurgeHost(ctx, kind, id, operationID))
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
	lifecycle, ok := s.store.(repository.ReaderHostLifecycleStore)
	if !ok {
		return dto.ReaderTrashResponse{}, errors.New("reader host lifecycle is not configured")
	}
	items, count, next, err := lifecycle.ListTrash(ctx, kind, after, limit)
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
		return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_inbox_batch", "inbox batch must contain between 1 and 100 ids")
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
		return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_inbox_batch", "inbox batch must contain at least one unique id")
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
			return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_inbox_batch_revision", "expected revisions must use requested inbox ids and non-negative revisions")
		}
		expectedRevisions[id] = revision
	}
	if len(expectedRevisions) > 0 && len(expectedRevisions) != len(ids) {
		return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_inbox_batch_revision", "expected revisions must cover every requested inbox id")
	}
	confirmations := make([]model.ReaderInboxBulkConfirmation, 0, len(ids))
	for _, id := range ids {
		var expectedRevision *int64
		if len(expectedRevisions) > 0 {
			revision, ok := expectedRevisions[id]
			if !ok {
				return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_inbox_batch_revision", "expected revisions must cover every requested inbox id")
			}
			revisionCopy := revision
			expectedRevision = &revisionCopy
		}
		confirmations = append(confirmations, model.ReaderInboxBulkConfirmation{ID: id, ExpectedRevision: expectedRevision})
	}
	items, err := s.store.BulkConfirmInbox(ctx, confirmations)
	if err != nil {
		return nil, mapReaderError(err)
	}
	return items, nil
}

// DiscardInboxBulk is the matching internal seam for batch discard. A
// discarded item is safe to retry; a confirmed item is rejected so a bulk
// action cannot remove a saved link's source capture accidentally.
func (s *ReaderVNextService) DiscardInboxBulk(ctx context.Context, rawIDs []string) ([]model.ReaderInboxBulkResult, error) {
	ids, err := parseReaderInboxBulkIDs(rawIDs)
	if err != nil {
		return nil, err
	}
	items, err := s.store.BulkUpdateInboxStatus(ctx, ids, "discarded")
	if err != nil {
		return nil, mapReaderError(err)
	}
	return items, nil
}

// RepairProjectedTodos rebuilds every host's TODO projection from the current
// Thoughts and published Notes and reports how many blocks the sources emit.
//
// Thought and Note commands maintain the projection inside their own
// transaction, so this is a drift repair, not part of any read. It stays an
// explicit operator command and a test entry point on purpose: putting a
// whole-installation rebuild back behind GET is exactly the cost this change
// removed. Dismissed projections stay dismissed here too — the repair shares
// the same tombstone rule as every other path.
func (s *ReaderVNextService) RepairProjectedTodos(ctx context.Context) (int, error) {
	projected, err := s.store.RepairTodoProjections(ctx)
	if err != nil {
		return 0, mapReaderError(err)
	}
	return projected, nil
}

func (s *ReaderVNextService) CreateTodo(ctx context.Context, request dto.ReaderTodoCreateRequest) (dto.ReaderTodoResponse, error) {
	text := strings.TrimSpace(request.Text)
	if text == "" {
		return dto.ReaderTodoResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "todo_text_required", "todo text is required")
	}
	todo, err := s.store.CreateTodo(ctx, model.ReaderTodo{Text: text, DueAt: request.DueAt, OriginKind: "standalone"})
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
	page, err := s.store.ListTodos(ctx, after, limit)
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
	item, err := s.store.PatchTodo(ctx, model.ReaderTodoPatch{
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
	return mapReaderError(s.store.DeleteTodo(ctx, id))
}

func (s *ReaderVNextService) GetEngagement(ctx context.Context, rawID string) (dto.ReaderEngagementResponse, error) {
	id, err := readerUUID(rawID, "link_id")
	if err != nil {
		return dto.ReaderEngagementResponse{}, err
	}
	item, err := s.store.GetEngagement(ctx, id)
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
		return dto.ReaderEngagementResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "engagement_patch_empty", "at least one engagement field is required")
	}
	if request.Progress != nil && (math.IsNaN(float64(*request.Progress)) || math.IsInf(float64(*request.Progress), 0) || *request.Progress < 0 || *request.Progress > 1) {
		return dto.ReaderEngagementResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_progress", "progress must be between 0 and 1")
	}
	item, err := s.store.PatchEngagement(ctx, model.ReaderEngagementPatch{LinkID: id, Read: request.Read, Progress: request.Progress, ReadLater: request.ReadLater})
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
		return readerFeedActionIdentity{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_item", "feed item key must use a canonical source prefix and UUID")
	}
	id, err := uuid.Parse(rawID)
	if err != nil || itemKey != kind+":"+id.String() {
		return readerFeedActionIdentity{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_item", "feed item key must use a canonical source prefix and UUID")
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
		return readerFeedActionIdentity{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_item", "feed item key must use a canonical source prefix and UUID")
	}
	return readerFeedActionIdentity{key: itemKey, kind: kind, source: source, id: id}, nil
}

func readerFeedSourceName(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "saved":
		return "reading"
	case "pending":
		return "inbox"
	case "feed":
		return "subscription"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

// normalizeReaderFeedItem repairs the representation of legacy snapshots and
// lightweight service doubles before it reaches the DTO. The action key is
// still the source-prefixed identity used by FeedbackFeed; subscription items
// may additionally carry the linked saved-resource identity.
// readerFeedSourceFromUnion infers the source of an item that carries no
// source and no key, from whichever union member is populated.
func readerFeedSourceFromUnion(item model.ReaderFeedItem) (string, error) {
	switch {
	case item.FeedItemID != nil:
		return "subscription", nil
	case item.InboxID != nil && item.LinkID == nil:
		return "inbox", nil
	case item.LinkID != nil && item.InboxID == nil:
		return "reading", nil
	default:
		return "", httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_item", "feed item must have one canonical union identity")
	}
}

// resolveReaderFeedItemSource settles the source, preferring the declared one,
// then the one implied by the action key, then the populated union member.
func resolveReaderFeedItemSource(item model.ReaderFeedItem) (string, error) {
	source := readerFeedSourceName(item.Source)
	if source == "" {
		if strings.TrimSpace(item.Key) != "" {
			identity, identityErr := readerFeedActionIdentityForKey(item.Key)
			if identityErr != nil {
				return "", identityErr
			}
			source = identity.source
		} else {
			unionSource, unionErr := readerFeedSourceFromUnion(item)
			if unionErr != nil {
				return "", unionErr
			}
			source = unionSource
		}
	}
	if source != "reading" && source != "inbox" && source != "subscription" {
		return "", httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_item", "feed item has an unsupported source")
	}
	return source, nil
}

// seedReaderFeedItemKey derives the action key from the union member matching
// the resolved source, for items that arrived without one.
func seedReaderFeedItemKey(item model.ReaderFeedItem, source string) (model.ReaderFeedItem, error) {
	if strings.TrimSpace(item.Key) != "" {
		return item, nil
	}
	var id *uuid.UUID
	switch source {
	case "reading":
		id = item.LinkID
	case "inbox":
		id = item.InboxID
	case "subscription":
		id = item.FeedItemID
	}
	if id == nil {
		return model.ReaderFeedItem{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_item", "feed item must have an action identity")
	}
	item.Key = map[string]string{
		"reading":      "link:",
		"inbox":        "inbox:",
		"subscription": "subscription:",
	}[source] + id.String()
	return item, nil
}

// bindReaderFeedItemResource pins a union member to the action identity,
// rejecting an item that already points somewhere else.
func bindReaderFeedItemResource(current **uuid.UUID, id uuid.UUID) error {
	if *current != nil && **current != id {
		return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_item", "feed item resource and action identity disagree")
	}
	if *current == nil {
		bound := id
		*current = &bound
	}
	return nil
}

// bindReaderFeedItemUnion enforces that exactly the union member belonging to
// the source is populated, and that it agrees with the action identity.
func bindReaderFeedItemUnion(item model.ReaderFeedItem, source string, id uuid.UUID) (model.ReaderFeedItem, error) {
	switch source {
	case "reading":
		if item.InboxID != nil || item.FeedItemID != nil {
			return model.ReaderFeedItem{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_item", "reading item has an incompatible union identity")
		}
		if err := bindReaderFeedItemResource(&item.LinkID, id); err != nil {
			return model.ReaderFeedItem{}, err
		}
	case "inbox":
		if item.LinkID != nil || item.FeedItemID != nil {
			return model.ReaderFeedItem{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_item", "inbox item has an incompatible union identity")
		}
		if err := bindReaderFeedItemResource(&item.InboxID, id); err != nil {
			return model.ReaderFeedItem{}, err
		}
	case "subscription":
		if item.InboxID != nil {
			return model.ReaderFeedItem{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_item", "subscription item has an incompatible union identity")
		}
		if err := bindReaderFeedItemResource(&item.FeedItemID, id); err != nil {
			return model.ReaderFeedItem{}, err
		}
	}
	return item, nil
}

// fillReaderFeedItemDefaults derives the representation fields legacy
// snapshots and service doubles never stored.
func fillReaderFeedItemDefaults(item model.ReaderFeedItem, source string) model.ReaderFeedItem {
	item.ActionKey = item.Key
	if strings.TrimSpace(item.ResourceKey) == "" {
		item.ResourceKey = item.ResourceIdentity()
	}
	if strings.TrimSpace(item.DedupeKey) == "" {
		item.DedupeKey = item.DedupeIdentity()
	}
	if strings.TrimSpace(item.SectionID) == "" {
		item.SectionID = source
	}
	if item.Actions == nil {
		item.Actions = item.ActionCapabilities()
	}
	return item
}

func normalizeReaderFeedItem(item model.ReaderFeedItem) (model.ReaderFeedItem, error) {
	if strings.TrimSpace(item.Key) == "" && strings.TrimSpace(item.ActionKey) != "" {
		item.Key = strings.TrimSpace(item.ActionKey)
	}
	source, err := resolveReaderFeedItemSource(item)
	if err != nil {
		return model.ReaderFeedItem{}, err
	}
	item, err = seedReaderFeedItemKey(item, source)
	if err != nil {
		return model.ReaderFeedItem{}, err
	}
	identity, err := readerFeedActionIdentityForKey(item.Key)
	if err != nil {
		return model.ReaderFeedItem{}, err
	}
	if identity.source != source {
		return model.ReaderFeedItem{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_item", "feed item source and action identity disagree")
	}
	item.Key = identity.key
	item.Source = source
	if strings.TrimSpace(item.ActionKey) != "" && strings.TrimSpace(item.ActionKey) != item.Key {
		return model.ReaderFeedItem{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_item", "feed item action identity disagrees with key")
	}
	item, err = bindReaderFeedItemUnion(item, source, identity.id)
	if err != nil {
		return model.ReaderFeedItem{}, err
	}
	return fillReaderFeedItemDefaults(item, source), nil
}

func feedItemResponse(item model.ReaderFeedItem) dto.ReaderFeedItemResponse {
	actions := item.ActionCapabilities()
	if actions == nil {
		actions = []string{}
	}
	enabledScoreSignals := make([]string, 0, len(item.EnabledScoreSignals))
	for _, signal := range item.EnabledScoreSignals {
		enabledScoreSignals = append(enabledScoreSignals, string(signal))
	}
	out := dto.ReaderFeedItemResponse{
		Key:         item.Key,
		Source:      item.Source,
		ItemType:    item.Source,
		ResourceKey: item.ResourceIdentity(),
		ActionKey:   item.ActionIdentity(),
		DedupeKey:   item.DedupeIdentity(),
		SectionID:   item.SectionIdentity(),
		Actions:     actions,
		Title:       item.Title,
		Summary:     item.Summary,
		URL:         item.URL,
		Read:        item.Read,
		ReadLater:   item.ReadLater,
		Saved:       item.Saved,
		Score:       item.Score,
		ScoreContributions: dto.ReaderFeedScoreContributions{
			PendingConfirmation:   item.ScoreContributions.PendingConfirmation,
			SavedLibrary:          item.ScoreContributions.SavedLibrary,
			SubscriptionRecent:    item.ScoreContributions.SubscriptionRecent,
			Unread:                item.ScoreContributions.Unread,
			ReadLater:             item.ScoreContributions.ReadLater,
			ChronologicalFallback: item.ScoreContributions.ChronologicalFallback,
		},
		EnabledScoreSignals: enabledScoreSignals,
		ReasonCode:          string(item.ReasonCode),
		ReasonParams: dto.ReaderFeedReasonParams{
			Source:    item.ReasonParams.Source,
			Read:      item.ReasonParams.Read,
			ReadLater: item.ReasonParams.ReadLater,
			CreatedAt: item.ReasonParams.CreatedAt,
		},
		ReasonContribution: item.ReasonContribution,
		ReasonText:         item.ReasonText,
		PublishedAt:        item.PublishedAt,
		EventAt:            item.VisibleEventAt(),
		CreatedAt:          item.CreatedAt,
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

var readerFeedSourceOrder = []string{"inbox", "reading", "subscription"}

func readerFeedSourceLabel(source string) string {
	switch source {
	case "inbox":
		return "收件箱"
	case "reading":
		return "收藏"
	case "subscription":
		return "订阅"
	default:
		return source
	}
}

func readerFeedSourceEnabled(sources []string, source string) bool {
	if len(sources) == 0 {
		return true
	}
	for _, candidate := range sources {
		if candidate == source {
			return true
		}
	}
	return false
}

func readerFeedAppendCapability(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

func cloneReaderFeedStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append(make([]string, 0, len(values)), values...)
}

func readerFeedDerivedMetadata(items []model.ReaderFeedItem, sources []string) ([]string, []dto.ReaderFeedSectionResponse, []dto.ReaderFeedSourceResponse) {
	counts := make(map[string]int, len(readerFeedSourceOrder))
	actions := make(map[string][]string, len(readerFeedSourceOrder))
	for _, source := range readerFeedSourceOrder {
		counts[source] = 0
		actions[source] = []string{}
	}
	for _, item := range items {
		if _, ok := counts[item.Source]; !ok {
			continue
		}
		counts[item.Source]++
		for _, action := range item.ActionCapabilities() {
			actions[item.Source] = readerFeedAppendCapability(actions[item.Source], action)
		}
	}
	capabilities := []string{"snapshot", "cursor", "dedupe", "reason", "source_filter"}
	sections := make([]dto.ReaderFeedSectionResponse, 0, len(readerFeedSourceOrder))
	sourceMeta := make([]dto.ReaderFeedSourceResponse, 0, len(readerFeedSourceOrder))
	for _, source := range readerFeedSourceOrder {
		if !readerFeedSourceEnabled(sources, source) {
			continue
		}
		if source == "inbox" {
			capabilities = readerFeedAppendCapability(capabilities, "inbox_batch")
		}
		if counts[source] == 0 {
			actions[source] = model.ReaderFeedItem{Source: source}.ActionCapabilities()
		}
		if len(actions[source]) > 0 {
			capabilities = readerFeedAppendCapability(capabilities, "actions")
		}
		sectionActions := cloneReaderFeedStrings(actions[source])
		sections = append(sections, dto.ReaderFeedSectionResponse{ID: source, Source: source, Label: readerFeedSourceLabel(source), Count: counts[source], Capabilities: sectionActions})
		sourceMeta = append(sourceMeta, dto.ReaderFeedSourceResponse{ID: source, Label: readerFeedSourceLabel(source), Enabled: true, Count: counts[source], Capabilities: cloneReaderFeedStrings(sectionActions)})
	}
	return capabilities, sections, sourceMeta
}

func (s *ReaderVNextService) Feed(ctx context.Context, mode, snapshotID, after string, limit int) (dto.ReaderFeedResponse, error) {
	return s.FeedWithSources(ctx, mode, snapshotID, after, nil, limit)
}

// FeedWithSources lists one immutable mixed-feed snapshot. The source filter
// is part of snapshot identity: callers must send the same filter while
// refreshing or advancing a cursor, otherwise the repository rejects the
// request instead of silently mixing two orderings.
func (s *ReaderVNextService) FeedWithSources(ctx context.Context, mode, snapshotID, after string, sources []string, limit int) (dto.ReaderFeedResponse, error) {
	mode = strings.TrimSpace(mode)
	if err := validateReaderFeedRequestMode(mode); err != nil {
		return dto.ReaderFeedResponse{}, err
	}
	normalizedSources, err := normalizeReaderFeedSources(sources)
	if err != nil {
		return dto.ReaderFeedResponse{}, err
	}
	page, err := s.listReaderFeedPage(ctx, mode, snapshotID, after, normalizedSources, limit)
	if err != nil {
		return dto.ReaderFeedResponse{}, err
	}
	responseMode, err := resolveReaderFeedResponseMode(page.Mode, mode)
	if err != nil {
		return dto.ReaderFeedResponse{}, err
	}
	out := dto.ReaderFeedResponse{Items: make([]dto.ReaderFeedItemResponse, 0, len(page.Items)), NextCursor: page.NextCursor, SnapshotID: page.SnapshotID, Mode: responseMode}
	normalizedItems := make([]model.ReaderFeedItem, 0, len(page.Items))
	for _, item := range page.Items {
		normalized, normalizeErr := normalizeReaderFeedItem(item)
		if normalizeErr != nil {
			return dto.ReaderFeedResponse{}, normalizeErr
		}
		normalizedItems = append(normalizedItems, normalized)
		out.Items = append(out.Items, feedItemResponse(normalized))
	}
	derivedCapabilities, derivedSections, derivedSources := readerFeedDerivedMetadata(normalizedItems, normalizedSources)
	out.Capabilities = readerFeedCapabilitiesResponse(page.Capabilities, derivedCapabilities)
	out.Sections = readerFeedSectionsResponse(page.Sections, derivedSections)
	out.Sources = readerFeedSourcesResponse(page.Sources, derivedSources)
	return out, nil
}

// validateReaderFeedRequestMode guards the caller-supplied mode. An empty mode
// stays legal here because the store, not the caller, picks the default.
func validateReaderFeedRequestMode(mode string) error {
	if mode != "" && mode != "recommended" && mode != "chronological" {
		return httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_mode", "unsupported feed mode")
	}
	return nil
}

// listReaderFeedPage picks the store call matching the source filter and
// guarantees a non-nil page to the caller: a store that answers with neither a
// page nor an error is a server fault, not an empty feed.
func (s *ReaderVNextService) listReaderFeedPage(ctx context.Context, mode, snapshotID, after string, normalizedSources []string, limit int) (*model.ReaderFeedPage, error) {
	var page *model.ReaderFeedPage
	var err error
	if len(normalizedSources) == 0 {
		page, err = s.store.ListFeed(ctx, mode, snapshotID, after, limit)
	} else {
		filteredStore, ok := s.store.(readerFeedSourceStore)
		if !ok {
			return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, "feed_source_filter_unavailable", "feed source filtering is unavailable")
		}
		page, err = filteredStore.ListFeedWithSources(ctx, mode, snapshotID, after, normalizedSources, limit)
	}
	if err != nil {
		return nil, mapReaderError(err)
	}
	if page == nil {
		return nil, httperr.NewWithCode(http.StatusInternalServerError, "reader_feed_unavailable", "reader feed returned no page")
	}
	return page, nil
}

// resolveReaderFeedResponseMode reports the mode the snapshot was actually built
// with, falling back to the request mode and then to "recommended". The result
// is always a concrete mode, so clients can echo it back on the next page.
func resolveReaderFeedResponseMode(pageMode, requestMode string) (string, error) {
	responseMode := strings.TrimSpace(pageMode)
	if responseMode == "" {
		responseMode = requestMode
		if responseMode == "" {
			responseMode = "recommended"
		}
	}
	if responseMode != "recommended" && responseMode != "chronological" {
		return "", httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_mode", "unsupported feed mode")
	}
	return responseMode, nil
}

// readerFeedCapabilitiesResponse prefers what the store declared and falls back
// to the derived set only when the store stayed silent. An explicitly empty
// (non-nil) store value is an answer, not a gap.
func readerFeedCapabilitiesResponse(pageCapabilities, derived []string) []string {
	if pageCapabilities == nil {
		return derived
	}
	return cloneReaderFeedStrings(pageCapabilities)
}

// readerFeedSectionsResponse mirrors readerFeedCapabilitiesResponse for sections
// and copies every capability slice so the response never aliases store memory.
func readerFeedSectionsResponse(pageSections []model.ReaderFeedSection, derived []dto.ReaderFeedSectionResponse) []dto.ReaderFeedSectionResponse {
	if pageSections == nil {
		return derived
	}
	out := make([]dto.ReaderFeedSectionResponse, 0, len(pageSections))
	for _, section := range pageSections {
		out = append(out, dto.ReaderFeedSectionResponse{ID: section.ID, Source: section.Source, Label: section.Label, Count: section.Count, Capabilities: cloneReaderFeedStrings(section.Capabilities)})
	}
	return out
}

// readerFeedSourcesResponse mirrors readerFeedCapabilitiesResponse for the
// source list and copies every capability slice so the response never aliases
// store memory.
func readerFeedSourcesResponse(pageSources []model.ReaderFeedSource, derived []dto.ReaderFeedSourceResponse) []dto.ReaderFeedSourceResponse {
	if pageSources == nil {
		return derived
	}
	out := make([]dto.ReaderFeedSourceResponse, 0, len(pageSources))
	for _, source := range pageSources {
		out = append(out, dto.ReaderFeedSourceResponse{ID: source.ID, Label: source.Label, Enabled: source.Enabled, Count: source.Count, Capabilities: cloneReaderFeedStrings(source.Capabilities)})
	}
	return out
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
				return nil, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_source", "unsupported feed source")
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
	if action != "not_interested" && action != "hide" && action != "save" && action != "unsave" {
		return dto.ReaderFeedFeedbackResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_action", "unsupported feed action")
	}
	if identity.source == "inbox" && (action == "save" || action == "unsave") {
		return dto.ReaderFeedFeedbackResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_feed_item", "the requested action is unavailable for inbox items")
	}
	feedback, err := s.store.FeedbackFeed(ctx, identity.key, action)
	if err = mapReaderError(err); err != nil {
		return dto.ReaderFeedFeedbackResponse{}, err
	}
	response := dto.ReaderFeedFeedbackResponse{ItemKey: feedback.ItemKey, Action: feedback.Action, Saved: feedback.Saved}
	if feedback.Association != nil {
		response.Association = &dto.ReaderFeedSaveAssociationResponse{FeedItemID: feedback.Association.FeedItemID.String(), LinkID: feedback.Association.LinkID.String(), CreatedLink: feedback.Association.CreatedLink}
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
	items, modelName, degraded, err := s.store.RelatedTags(ctx, linkID, limit)
	if err != nil {
		return dto.ReaderRelatedTagsResponse{}, mapReaderError(err)
	}
	return dto.ReaderRelatedTagsResponse{Items: items, Model: modelName, Degraded: degraded}, nil
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
	if err := s.refreshActivityIfDue(ctx); err != nil {
		return dto.ReaderActivityResponse{}, mapReaderError(err)
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	page, err := s.store.ListActivity(ctx, model.ReaderActivityQuery{Kind: kind, After: after, Limit: limit})
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

const readerActivityRefreshInterval = 30 * time.Second

func (s *ReaderVNextService) refreshActivityIfDue(ctx context.Context) error {
	now := s.now()
	s.activityMu.Lock()
	if !s.activityLast.IsZero() && now.Sub(s.activityLast) < readerActivityRefreshInterval {
		s.activityMu.Unlock()
		return nil
	}
	s.activityLast = now
	s.activityMu.Unlock()

	if err := s.store.RefreshActivity(ctx); err != nil {
		s.activityMu.Lock()
		if s.activityLast.Equal(now) {
			s.activityLast = time.Time{}
		}
		s.activityMu.Unlock()
		return err
	}
	return nil
}

func (s *ReaderVNextService) PatchLinkMetadata(ctx context.Context, rawID string, request dto.ReaderLinkMetadataRequest, expected int64) (dto.ReaderLinkMetadataResponse, error) {
	id, err := readerUUID(rawID, "link_id")
	if err != nil {
		return dto.ReaderLinkMetadataResponse{}, err
	}
	if !request.Complete() {
		return dto.ReaderLinkMetadataResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeMetadataFieldsRequired, "title, summary, and tags are required")
	}
	if err := validateLinkMetadataRequest(&request); err != nil {
		return dto.ReaderLinkMetadataResponse{}, err
	}
	update, err := s.store.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{LinkID: id, Title: request.Title, Summary: request.Summary, Tags: request.Tags, ExpectedRevision: expected})
	if err != nil {
		if errors.Is(err, repository.ErrRevisionConflict) {
			return dto.ReaderLinkMetadataResponse{}, httperr.NewWithCode(http.StatusConflict, httperr.CodeMetadataRevisionConflict, "link metadata revision is stale")
		}
		return dto.ReaderLinkMetadataResponse{}, mapReaderError(err)
	}
	if update.MetadataRevision < 1 || update.MetadataRevision > model.LinkMetadataMaxRevision {
		return dto.ReaderLinkMetadataResponse{}, httperr.NewWithCode(http.StatusConflict, httperr.CodeMetadataRevisionConflict, "link metadata revision is outside the JavaScript-safe range")
	}
	if update.TagsChanged && s.metadataCache != nil {
		s.metadataCache.Invalidate(ctx)
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
		return httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidLinkMetadata, "title exceeds 512 characters")
	}
	if request.Summary != nil && utf8.RuneCountInString(*request.Summary) > maxLinkMetadataSummaryRunes {
		return httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidLinkMetadata, "summary exceeds 4096 characters")
	}
	if request.Tags == nil {
		return httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidLinkMetadata, "tags must be an array")
	}
	if len(request.Tags) > maxLinkMetadataTags {
		return httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidLinkMetadata, "tags may contain at most 50 items")
	}

	folder := cases.Fold()
	seen := make(map[string]struct{}, len(request.Tags))
	tags := make([]string, 0, len(request.Tags))
	for _, raw := range request.Tags {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			return httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidLinkMetadata, "tags must not contain empty values")
		}
		if utf8.RuneCountInString(tag) > maxLinkMetadataTagRunes {
			return httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidLinkMetadata, "tags may not exceed 64 characters")
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

func (s *ReaderVNextService) ListContentHistory(ctx context.Context, rawID string, limit int) ([]dto.ReaderContentHistoryResponse, error) {
	id, err := readerUUID(rawID, "link_id")
	if err != nil {
		return nil, err
	}
	items, err := s.store.ListContentHistory(ctx, id, limit)
	if err != nil {
		return nil, mapReaderError(err)
	}
	out := make([]dto.ReaderContentHistoryResponse, 0, len(items))
	for _, item := range items {
		out = append(out, dto.ReaderContentHistoryResponse{ID: item.ID, Revision: item.Revision, Content: item.Content, ContentDocument: item.ContentDocument, ContentFormat: item.ContentFormat, ContentSource: item.ContentSource, CreatedAt: item.CreatedAt})
	}
	return out, nil
}

func (s *ReaderVNextService) RestoreContentHistory(ctx context.Context, rawID string, historyID, expectedRevision int64) (dto.ReaderContentHistoryRestoreResponse, error) {
	id, err := readerUUID(rawID, "link_id")
	if err != nil {
		return dto.ReaderContentHistoryRestoreResponse{}, err
	}
	revision, err := s.store.RestoreContentHistory(ctx, id, historyID, expectedRevision)
	if err != nil {
		return dto.ReaderContentHistoryRestoreResponse{}, mapReaderError(err)
	}
	return dto.ReaderContentHistoryRestoreResponse{LinkID: id.String(), ContentRevision: revision}, nil
}

// validateReaderAIRequest normalises and bounds the request before any saved
// content is resolved, keeping the wire contract deterministic when AI is off.
func validateReaderAIRequest(ctx context.Context, request dto.ReaderAIRequest) (prompt, scope, selected string, err error) {
	prompt = strings.TrimSpace(request.Prompt)
	if prompt == "" {
		return "", "", "", httperr.NewWithCode(http.StatusUnprocessableEntity, "ai_prompt_required", "prompt is required")
	}
	scope = strings.TrimSpace(request.Scope)
	if scope == "" {
		scope = "general"
	}
	if scope != "general" && scope != "selection" && scope != "thought" {
		return "", "", "", httperr.NewWithCode(http.StatusUnprocessableEntity, "ai_scope_invalid", "unsupported AI scope")
	}
	selected = strings.TrimSpace(request.SelectedText)
	if scope == "selection" && selected == "" {
		return "", "", "", httperr.NewWithCode(http.StatusUnprocessableEntity, "ai_selection_required", "selected text is required for selection scope")
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
		linkContext, err = s.store.GetAIContext(ctx, linkID)
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
		return "", httperr.NewWithCode(http.StatusRequestEntityTooLarge, "ai_context_too_large", "AI context is too large")
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
	if capability, ok := s.ai.(readerAICapability); ok && !capability.Available() {
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

func categoryResponse(item model.ReaderCategory) dto.ReaderCategoryResponse {
	return dto.ReaderCategoryResponse{ID: item.ID.String(), Name: item.Name, Count: item.Count, CreatedAt: item.CreatedAt}
}

func (s *ReaderVNextService) ListCategories(ctx context.Context) (dto.ReaderCategoriesResponse, error) {
	items, err := s.store.ListCategories(ctx)
	if err != nil {
		return dto.ReaderCategoriesResponse{}, mapReaderError(err)
	}
	out := dto.ReaderCategoriesResponse{Items: make([]dto.ReaderCategoryResponse, 0, len(items))}
	for _, item := range items {
		out.Items = append(out.Items, categoryResponse(item))
	}
	return out, nil
}

func (s *ReaderVNextService) CreateCategory(ctx context.Context, request dto.ReaderCategoryRequest) (dto.ReaderCategoryResponse, error) {
	name := strings.TrimSpace(request.Name)
	if name == "" {
		return dto.ReaderCategoryResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, "category_name_required", "category name is required")
	}
	item, err := s.store.CreateCategory(ctx, name)
	if err != nil {
		return dto.ReaderCategoryResponse{}, mapReaderError(err)
	}
	return categoryResponse(*item), nil
}

func (s *ReaderVNextService) DeleteCategory(ctx context.Context, rawID string) error {
	id, err := readerUUID(rawID, "category_id")
	if err != nil {
		return err
	}
	return mapReaderError(s.store.DeleteCategory(ctx, id))
}

func (s *ReaderVNextService) SetCategoryMembership(ctx context.Context, rawID string, request dto.ReaderCategoryMembershipRequest) error {
	id, err := readerUUID(rawID, "category_id")
	if err != nil {
		return err
	}
	return mapReaderError(s.store.SetCategoryMembership(ctx, id, strings.TrimSpace(request.HostKind), strings.TrimSpace(request.HostID), request.Present))
}
