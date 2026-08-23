package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/notetitle"
	"webtag/internal/problem"
)

type ReaderNoteCreateCommand struct {
	Title   string
	Content string
}

type ReaderNotePublishCommand struct {
	NoteID                    uuid.UUID
	ExpectedDraftRevision     int64
	ExpectedPublishedRevision int64
	ReanchorOps               []ReaderNoteReanchorOperationCommand
}

type ReaderNoteRestoreCommand struct {
	NoteID                    uuid.UUID
	Revision                  int64
	ExpectedDraftRevision     int64
	ExpectedPublishedRevision int64
	ReanchorOps               []ReaderNoteReanchorOperationCommand
}

type ReaderNoteReanchorOperationCommand struct {
	ThoughtID string                           `json:"thought_id"`
	Status    string                           `json:"status"`
	Reason    string                           `json:"reason,omitempty"`
	Target    *ReaderNoteReanchorTargetCommand `json:"target,omitempty"`
	Quote     *ReaderThoughtQuoteCommand       `json:"quote,omitempty"`
	Range     *ReaderNoteReanchorRangeCommand  `json:"range,omitempty"`
}

type ReaderNoteReanchorTargetCommand struct {
	Kind    string                                 `json:"kind"`
	HostID  string                                 `json:"host_id"`
	Version ReaderNoteReanchorTargetVersionCommand `json:"version"`
}

type ReaderNoteReanchorTargetVersionCommand struct {
	NoteRevision int64 `json:"note_revision"`
}

type ReaderNoteReanchorRangeCommand struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type ReaderNotePage struct {
	Items      []model.ReaderNote
	Count      int
	NextCursor string
}

func deriveNoteTitle(content string) string {
	return notetitle.Derive(content)
}

func (s *ReaderNoteApplication) CreateNote(ctx context.Context, command ReaderNoteCreateCommand) (model.ReaderNote, error) {
	title := strings.TrimSpace(command.Title)
	if title == "" {
		title = deriveNoteTitle(command.Content)
	}
	note := model.ReaderNote{Title: title, PublishedContent: ""}
	if command.Content != "" {
		note.DraftContent = &command.Content
	}
	created, err := s.notes.CreateNote(ctx, note)
	if err != nil {
		return model.ReaderNote{}, mapReaderError(err)
	}
	return *created, nil
}

func (s *ReaderNoteApplication) ListNotes(ctx context.Context, after string, limit int) (ReaderNotePage, error) {
	items, count, next, err := s.notes.ListNotes(ctx, after, limit)
	if err != nil {
		return ReaderNotePage{}, mapReaderError(err)
	}
	return ReaderNotePage{Items: items, Count: count, NextCursor: next}, nil
}

func (s *ReaderNoteApplication) GetNote(ctx context.Context, id uuid.UUID) (model.ReaderNote, error) {
	note, err := s.notes.GetNote(ctx, id)
	if err != nil {
		return model.ReaderNote{}, mapReaderError(err)
	}
	return *note, nil
}

func (s *ReaderNoteApplication) SaveNoteDraft(ctx context.Context, command model.ReaderNoteDraftCommand) (model.ReaderNote, error) {
	note, err := s.notes.SaveNoteDraft(ctx, command)
	if err != nil {
		return model.ReaderNote{}, mapReaderError(err)
	}
	return *note, nil
}

func (s *ReaderNoteApplication) DiscardNoteDraft(ctx context.Context, command model.ReaderNoteDiscardDraftCommand) error {
	if command.ExpectedDraftRevision < 1 {
		return problem.NewWithCode(problem.Invalid, "invalid_draft_revision", "draft revision must be positive")
	}
	return mapReaderError(s.notes.DiscardNoteDraft(ctx, command))
}

func (s *ReaderNoteApplication) PublishNote(ctx context.Context, command ReaderNotePublishCommand) (model.ReaderNote, error) {
	if err := validateReaderReanchorOps(command.ReanchorOps); err != nil {
		return model.ReaderNote{}, err
	}
	rawOps, err := readerNoteReanchorOpsRaw(command.ReanchorOps)
	if err != nil {
		return model.ReaderNote{}, err
	}
	note, err := s.notes.PublishNote(ctx, model.ReaderNotePublishCommand{
		NoteID: command.NoteID, ExpectedDraftRevision: command.ExpectedDraftRevision,
		ExpectedPublishedRevision: command.ExpectedPublishedRevision, ReanchorOps: rawOps,
	})
	if err != nil {
		return model.ReaderNote{}, mapReaderError(err)
	}
	return *note, nil
}

func validateReaderReanchorOps(ops []ReaderNoteReanchorOperationCommand) error {
	if len(ops) > 500 {
		return problem.NewWithCode(problem.Invalid, "invalid_reanchor_ops", "too many reanchor operations")
	}
	for _, op := range ops {
		if err := validateReaderReanchorOp(op); err != nil {
			return err
		}
	}
	return nil
}

func validateReaderReanchorOp(op ReaderNoteReanchorOperationCommand) error {
	if strings.TrimSpace(op.ThoughtID) == "" {
		return problem.NewWithCode(problem.Invalid, "invalid_reanchor_ops", "reanchor operation must name a thought")
	}
	switch op.Status {
	case "historical":
		return nil
	case "reanchored":
		return validateReaderReanchoredOp(op)
	default:
		return problem.NewWithCode(problem.Invalid, "invalid_reanchor_ops", "reanchor operation status is invalid")
	}
}

func validateReaderReanchoredOp(op ReaderNoteReanchorOperationCommand) error {
	if op.Reason != "diff-context" && op.Reason != "unique-quote" {
		return problem.NewWithCode(problem.Invalid, "invalid_reanchor_ops", "reanchor operation reason is invalid")
	}
	if op.Target == nil || op.Target.Kind != "note" || strings.TrimSpace(op.Target.HostID) == "" || op.Target.Version.NoteRevision <= 0 {
		return problem.NewWithCode(problem.Invalid, "invalid_reanchor_ops", "reanchor operation target is invalid")
	}
	if op.Quote == nil || op.Quote.Exact == nil || *op.Quote.Exact == "" {
		return problem.NewWithCode(problem.Invalid, "invalid_reanchor_ops", "reanchor operation quote is invalid")
	}
	if op.Range == nil || op.Range.Start < 0 || op.Range.End <= op.Range.Start || op.Range.End > 1<<24 {
		return problem.NewWithCode(problem.Invalid, "invalid_reanchor_ops", "reanchor operation range is invalid")
	}
	return nil
}

func readerNoteReanchorOpsRaw(ops []ReaderNoteReanchorOperationCommand) ([]json.RawMessage, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	raw := make([]json.RawMessage, 0, len(ops))
	for _, op := range ops {
		encoded, err := json.Marshal(op)
		if err != nil || len(encoded) > 128*1024 {
			return nil, problem.NewWithCode(problem.Invalid, "invalid_reanchor_ops", "reanchor operation must be JSON encodable")
		}
		raw = append(raw, encoded)
	}
	return raw, nil
}

func (s *ReaderNoteApplication) DeleteNote(ctx context.Context, id uuid.UUID) (model.ReaderHostLifecycleResult, error) {
	result, err := s.hosts.SoftDeleteHost(ctx, model.ReaderHostNote, id)
	if err != nil {
		return model.ReaderHostLifecycleResult{}, mapReaderError(err)
	}
	return result, nil
}

func (s *ReaderNoteApplication) RestoreNote(ctx context.Context, id uuid.UUID) (model.ReaderHostLifecycleResult, error) {
	if s.hostRestores == nil {
		return model.ReaderHostLifecycleResult{}, errors.New("reader host restore commands are not configured")
	}
	result, err := s.hostRestores.RestoreHost(ctx, model.ReaderHostNote, id)
	if err != nil {
		return model.ReaderHostLifecycleResult{}, mapReaderError(err)
	}
	return result, nil
}

func (s *ReaderNoteApplication) ListNoteHistory(ctx context.Context, id uuid.UUID, limit int) ([]model.ReaderNoteHistory, error) {
	items, err := s.notes.ListNoteHistory(ctx, id, limit)
	if err != nil {
		return nil, mapReaderError(err)
	}
	return items, nil
}

func (s *ReaderNoteApplication) RestoreNoteRevision(ctx context.Context, command ReaderNoteRestoreCommand) (model.ReaderNote, error) {
	if err := validateReaderReanchorOps(command.ReanchorOps); err != nil {
		return model.ReaderNote{}, err
	}
	rawOps, err := readerNoteReanchorOpsRaw(command.ReanchorOps)
	if err != nil {
		return model.ReaderNote{}, err
	}
	note, err := s.notes.RestoreNoteRevision(ctx, model.ReaderNoteRestoreCommand{
		NoteID: command.NoteID, Revision: command.Revision, ExpectedDraftRevision: command.ExpectedDraftRevision,
		ExpectedPublishedRevision: command.ExpectedPublishedRevision, ReanchorOps: rawOps,
	})
	if err != nil {
		return model.ReaderNote{}, mapReaderError(err)
	}
	return *note, nil
}
