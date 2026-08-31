package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"webtag/internal/problem"
	"webtag/internal/repository"
)

// ReaderAIBackend is deliberately tiny. The Reader service owns scope and
// context construction; the backend owns the provider transport and can be
// disabled without making the rest of Reader unavailable.
type ReaderAIBackend interface {
	Complete(context.Context, string, string) (answer string, modelName string, err error)
}

type ReaderThoughtApplication struct {
	thoughts ReaderThoughtStore
	now      func() time.Time
}

type ReaderNoteApplication struct {
	notes        ReaderNoteStore
	hosts        ReaderHostStore
	hostRestores ReaderHostRestoreCommands
}

type ReaderInboxApplication struct {
	inbox            ReaderInboxStore
	inboxCommands    InboxProposalCommands
	inboxConfirm     ReaderInboxConfirmCommands
	inboxBulkConfirm ReaderInboxBulkConfirmCommands
	inboxAIConfirm   ReaderInboxAIConfirmCommands
}

type ReaderTodoApplication struct {
	todos ReaderTodoStore
}

type ReaderLibraryApplication struct {
	library           ReaderLibraryStore
	ai                ReaderAIBackend
	feedFeedback      ReaderFeedFeedbackCommands
	now               func() time.Time
	activityCursorKey []byte
}

type ReaderHostApplication struct {
	hosts        ReaderHostStore
	hostRestores ReaderHostRestoreCommands
}

// ReaderApplications is a composition result, not a facade. HTTP routing
// injects each named feature directly and no caller can invoke Reader methods
// through this aggregate.
type ReaderApplications struct {
	Thoughts *ReaderThoughtApplication
	Notes    *ReaderNoteApplication
	Inbox    *ReaderInboxApplication
	Todos    *ReaderTodoApplication
	Library  *ReaderLibraryApplication
	Hosts    *ReaderHostApplication
}

type ReaderApplicationOptions struct {
	CursorSigningKey         string
	InboxProposalCommands    InboxProposalCommands
	InboxConfirmCommands     ReaderInboxConfirmCommands
	InboxBulkConfirmCommands ReaderInboxBulkConfirmCommands
	InboxAIConfirmCommands   ReaderInboxAIConfirmCommands
	FeedFeedbackCommands     ReaderFeedFeedbackCommands
	HostRestoreCommands      ReaderHostRestoreCommands
}

func NewReaderApplications(stores ReaderStores, ai ReaderAIBackend, configured ReaderApplicationOptions) *ReaderApplications {
	cursorKey := processReaderCursorKey
	if configured.CursorSigningKey != "" {
		cursorKey = []byte(configured.CursorSigningKey)
	}
	requireReaderApplicationCommand("InboxProposalCommands", configured.InboxProposalCommands)
	requireReaderApplicationCommand("InboxConfirmCommands", configured.InboxConfirmCommands)
	requireReaderApplicationCommand("InboxBulkConfirmCommands", configured.InboxBulkConfirmCommands)
	requireReaderApplicationCommand("InboxAIConfirmCommands", configured.InboxAIConfirmCommands)
	requireReaderApplicationCommand("FeedFeedbackCommands", configured.FeedFeedbackCommands)
	requireReaderApplicationCommand("HostRestoreCommands", configured.HostRestoreCommands)
	return &ReaderApplications{
		Thoughts: &ReaderThoughtApplication{thoughts: stores.Thoughts, now: time.Now},
		Notes: &ReaderNoteApplication{
			notes: stores.Notes, hosts: stores.Hosts, hostRestores: configured.HostRestoreCommands,
		},
		Inbox: &ReaderInboxApplication{
			inbox: stores.Inbox, inboxCommands: configured.InboxProposalCommands,
			inboxConfirm: configured.InboxConfirmCommands, inboxBulkConfirm: configured.InboxBulkConfirmCommands,
			inboxAIConfirm: configured.InboxAIConfirmCommands,
		},
		Todos: &ReaderTodoApplication{todos: stores.Todos},
		Library: &ReaderLibraryApplication{
			library: stores.Library, ai: ai, feedFeedback: configured.FeedFeedbackCommands,
			now: time.Now, activityCursorKey: append([]byte(nil), cursorKey...),
		},
		Hosts: &ReaderHostApplication{hosts: stores.Hosts, hostRestores: configured.HostRestoreCommands},
	}
}

func requireReaderApplicationCommand(name string, command any) {
	if command == nil {
		panic("service.NewReaderApplications: ReaderApplicationOptions." + name + " is required")
	}
}

func readerUUID(raw, field string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, problem.NewWithCode(problem.Invalid, "invalid_"+field, field+" must be a UUID")
	}
	return id, nil
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
