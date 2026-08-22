package repository

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"webtag/internal/database"
	"webtag/internal/model"
)

func scanReaderThought(row readerScanner) (*model.ReaderThought, error) {
	return scanReaderThoughtWithLifecycle(row, false)
}

func scanReaderThoughtWithLifecycle(row readerScanner, lifecycle bool) (*model.ReaderThought, error) {
	var out model.ReaderThought
	var target, quote []byte
	if lifecycle {
		var reason pgtype.Text
		var tombstonedAt pgtype.Timestamptz
		if err := row.Scan(&out.ID, &out.HostKind, &out.HostID, &out.LinkID, &target, &quote, &out.Body, &out.Source, &out.Deleted, &out.LastSequence, &out.WinnerKey.LogicalClock, &out.WinnerKey.DeviceID, &out.WinnerKey.OpID, &out.CreatedAt, &out.UpdatedAt, &reason, &tombstonedAt); err != nil {
			return nil, err
		}
		out.LifecycleStatus = "active"
		if reason.Valid || tombstonedAt.Valid {
			out.LifecycleStatus = "tombstone"
			if reason.Valid {
				value := reason.String
				out.LifecycleReason = &value
			}
			if tombstonedAt.Valid {
				value := tombstonedAt.Time
				out.TombstonedAt = &value
			}
		}
	} else if err := row.Scan(&out.ID, &out.HostKind, &out.HostID, &out.LinkID, &target, &quote, &out.Body, &out.Source, &out.Deleted, &out.LastSequence, &out.WinnerKey.LogicalClock, &out.WinnerKey.DeviceID, &out.WinnerKey.OpID, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return nil, err
	} else {
		out.LifecycleStatus = "active"
	}
	out.Target = append(json.RawMessage(nil), target...)
	if len(quote) > 0 {
		out.Quote = append(json.RawMessage(nil), quote...)
	}
	return &out, nil
}

// applyReaderThoughtSnapshot replaces only user-visible content. Sequence and
// winner metadata intentionally remain from the materialized row so clients
// can continue to advance their sync cursor and perform CAS without ever
// treating a later replay as authority over archived content.
const readerThoughtSnapshotVersion = 1

func applyReaderThoughtSnapshot(item *model.ReaderThought, raw []byte) error {
	var snapshot struct {
		SnapshotVersion      int             `json:"snapshot_version"`
		ID                   string          `json:"id"`
		HostKind             string          `json:"host_kind"`
		HostID               string          `json:"host_id"`
		LinkID               *string         `json:"link_id"`
		Target               json.RawMessage `json:"target"`
		Quote                json.RawMessage `json:"quote"`
		Body                 string          `json:"body"`
		Source               string          `json:"source"`
		OriginalHostSnapshot json.RawMessage `json:"original_host_snapshot"`
	}
	if !json.Valid(raw) || json.Unmarshal(raw, &snapshot) != nil {
		return ErrInvalidReaderThought
	}
	if snapshot.SnapshotVersion != readerThoughtSnapshotVersion {
		return ErrInvalidReaderThought
	}
	if snapshot.ID != "" {
		item.ID = snapshot.ID
	}
	if snapshot.HostKind != "" {
		item.HostKind = snapshot.HostKind
	}
	if snapshot.HostID != "" {
		item.HostID = snapshot.HostID
	}
	if snapshot.LinkID != nil {
		linkID, err := uuid.Parse(*snapshot.LinkID)
		if err != nil {
			return ErrInvalidReaderThought
		}
		item.LinkID = &linkID
	} else {
		item.LinkID = nil
	}
	if len(snapshot.Target) > 0 {
		item.Target = append(item.Target[:0], snapshot.Target...)
	}
	if len(snapshot.Quote) > 0 {
		item.Quote = append(item.Quote[:0], snapshot.Quote...)
	} else {
		item.Quote = nil
	}
	item.Body = snapshot.Body
	item.Source = snapshot.Source
	if len(snapshot.OriginalHostSnapshot) > 0 {
		item.OriginalHostSnapshot = append(item.OriginalHostSnapshot[:0], snapshot.OriginalHostSnapshot...)
	}
	return nil
}

// applyReaderThoughtReplaySnapshot is deliberately stricter than the helper
// used by reattach flows. Sync is a recovery boundary: returning the mutable
// projection for a malformed tombstone would silently replace the historical
// record a client is supposed to preserve.
func applyReaderThoughtReplaySnapshot(item *model.ReaderThought, raw []byte) error {
	if err := validateReaderThoughtReplaySnapshot(item.ID, raw); err != nil {
		return err
	}
	return applyReaderThoughtSnapshot(item, raw)
}

// applyReaderThoughtUserDeletionSnapshot intentionally never copies snapshot
// fields into the replay item. A user deletion stores only an
// anti-resurrection marker, and sync must not disclose stale projection
// content if that marker is replayed after another operation advanced the log.
func applyReaderThoughtUserDeletionSnapshot(item *model.ReaderThought, raw []byte) error {
	var snapshot struct {
		ID       string `json:"id"`
		HostKind string `json:"host_kind"`
		HostID   string `json:"host_id"`
	}
	if !item.Deleted || !json.Valid(raw) || json.Unmarshal(raw, &snapshot) != nil ||
		snapshot.ID == "" || snapshot.ID != item.ID || snapshot.HostKind != item.HostKind || snapshot.HostID != item.HostID {
		return ErrInvalidReaderThought
	}
	item.LinkID = nil
	item.Target = json.RawMessage(`{}`)
	item.Quote = nil
	item.Body = ""
	item.Source = ""
	item.OriginalHostSnapshot = nil
	return nil
}

var readerThoughtReplaySnapshotFields = []string{
	"snapshot_version", "id", "host_kind", "host_id", "link_id", "type", "body", "target", "quote", "source",
	"created_at", "updated_at", "original_host_snapshot", "original_host_identity", "frozen_at",
}

func decodeReaderThoughtReplaySnapshot(raw []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if !json.Valid(raw) || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, ErrInvalidReaderThought
	}
	for _, name := range readerThoughtReplaySnapshotFields {
		if len(fields[name]) == 0 {
			return nil, ErrInvalidReaderThought
		}
	}
	var snapshotVersion int
	if json.Unmarshal(fields["snapshot_version"], &snapshotVersion) != nil || snapshotVersion != readerThoughtSnapshotVersion {
		return nil, ErrInvalidReaderThought
	}
	return fields, nil
}

func readerThoughtSnapshotString(fields map[string]json.RawMessage, name string) (string, bool) {
	rawValue := strings.TrimSpace(string(fields[name]))
	if len(rawValue) < 2 || rawValue[0] != '"' {
		return "", false
	}
	var value string
	if json.Unmarshal(fields[name], &value) != nil {
		return "", false
	}
	return value, true
}

func readerThoughtSnapshotObject(fields map[string]json.RawMessage, name string) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(fields[name], &value) == nil && value != nil
}

func validReaderThoughtSnapshotIdentity(fields map[string]json.RawMessage, thoughtID string) bool {
	id, ok := readerThoughtSnapshotString(fields, "id")
	if !ok || id == "" || id != thoughtID {
		return false
	}
	hostKind, hostKindOK := readerThoughtSnapshotString(fields, "host_kind")
	hostID, hostIDOK := readerThoughtSnapshotString(fields, "host_id")
	typeName, typeOK := readerThoughtSnapshotString(fields, "type")
	return hostKindOK && strings.TrimSpace(hostKind) != "" && hostIDOK && strings.TrimSpace(hostID) != "" && typeOK && typeName == "thought"
}

func validReaderThoughtSnapshotContent(fields map[string]json.RawMessage) bool {
	if _, ok := readerThoughtSnapshotString(fields, "body"); !ok {
		return false
	}
	if _, ok := readerThoughtSnapshotString(fields, "source"); !ok {
		return false
	}
	if !readerThoughtSnapshotObject(fields, "target") || !readerThoughtSnapshotObject(fields, "original_host_identity") {
		return false
	}
	quote := strings.TrimSpace(string(fields["quote"]))
	return (quote == "null" || readerThoughtSnapshotObject(fields, "quote")) && json.Valid(fields["original_host_snapshot"])
}

func validReaderThoughtSnapshotLink(fields map[string]json.RawMessage) bool {
	if strings.TrimSpace(string(fields["link_id"])) == "null" {
		return true
	}
	var value string
	if json.Unmarshal(fields["link_id"], &value) != nil {
		return false
	}
	_, err := uuid.Parse(value)
	return err == nil
}

func validReaderThoughtSnapshotTimes(fields map[string]json.RawMessage) bool {
	for _, name := range []string{"created_at", "updated_at", "frozen_at"} {
		var value time.Time
		if json.Unmarshal(fields[name], &value) != nil || value.IsZero() {
			return false
		}
	}
	return true
}

func validateReaderThoughtReplaySnapshot(thoughtID string, raw []byte) error {
	fields, err := decodeReaderThoughtReplaySnapshot(raw)
	if err != nil {
		return err
	}
	if !validReaderThoughtSnapshotIdentity(fields, thoughtID) || !validReaderThoughtSnapshotContent(fields) ||
		!validReaderThoughtSnapshotLink(fields) || !validReaderThoughtSnapshotTimes(fields) {
		return ErrInvalidReaderThought
	}
	return nil
}

func scanReaderThoughtSyncSnapshot(row readerScanner) (*model.ReaderThought, error) {
	var (
		item          model.ReaderThought
		target, quote []byte
		snapshot      []byte
		reason        pgtype.Text
		tombstonedAt  pgtype.Timestamptz
	)
	if err := row.Scan(
		&item.ID, &item.HostKind, &item.HostID, &item.LinkID, &target, &quote,
		&item.Body, &item.Source, &item.Deleted, &item.LastSequence,
		&item.WinnerKey.LogicalClock, &item.WinnerKey.DeviceID, &item.WinnerKey.OpID,
		&item.CreatedAt, &item.UpdatedAt, &snapshot, &reason, &tombstonedAt,
	); err != nil {
		return nil, err
	}
	item.Target = append(json.RawMessage(nil), target...)
	if len(quote) > 0 {
		item.Quote = append(json.RawMessage(nil), quote...)
	}
	if !reason.Valid && !tombstonedAt.Valid && len(snapshot) == 0 {
		item.LifecycleStatus = "active"
		return &item, nil
	}
	if !reason.Valid || strings.TrimSpace(reason.String) == "" || !tombstonedAt.Valid || len(snapshot) == 0 {
		return nil, ErrInvalidReaderThought
	}
	if reason.String == "user_deleted" {
		if err := applyReaderThoughtUserDeletionSnapshot(&item, snapshot); err != nil {
			return nil, err
		}
	} else if err := applyReaderThoughtReplaySnapshot(&item, snapshot); err != nil {
		return nil, err
	}
	item.LifecycleStatus = "tombstone"
	reasonValue := reason.String
	item.LifecycleReason = &reasonValue
	item.TombstonedAt = &tombstonedAt.Time
	return &item, nil
}

func scanReaderThoughtHistorySnapshot(row readerScanner) (*model.ReaderThought, error) {
	var (
		item     model.ReaderThought
		target   []byte
		quote    []byte
		snapshot []byte
		reason   string
		at       time.Time
	)
	if err := row.Scan(
		&item.ID, &item.HostKind, &item.HostID, &item.LinkID, &target, &quote,
		&item.Body, &item.Source, &item.Deleted, &item.LastSequence,
		&item.WinnerKey.LogicalClock, &item.WinnerKey.DeviceID, &item.WinnerKey.OpID,
		&item.CreatedAt, &item.UpdatedAt, &snapshot, &reason, &at,
	); err != nil {
		return nil, err
	}
	item.Target = append(item.Target, target...)
	item.Quote = append(item.Quote, quote...)
	if err := applyReaderThoughtReplaySnapshot(&item, snapshot); err != nil {
		return nil, fmt.Errorf("decode thought lifecycle snapshot: %w", err)
	}
	item.LifecycleStatus = "tombstone"
	item.LifecycleReason = &reason
	item.TombstonedAt = &at
	return &item, nil
}

// Qualified columns keep the sync/history joins unambiguous while remaining
// valid for the other queries that read directly from reader_thoughts.
const readerThoughtColumns = `reader_thoughts.id, reader_thoughts.host_kind, reader_thoughts.host_id, reader_thoughts.link_id, reader_thoughts.target, reader_thoughts.quote, reader_thoughts.body, reader_thoughts.source, reader_thoughts.deleted, reader_thoughts.last_sequence, reader_thoughts.winner_logical_clock, reader_thoughts.winner_device_id, reader_thoughts.winner_op_id, reader_thoughts.created_at, reader_thoughts.updated_at`

type duplicateThoughtOp struct {
	sequence  int64
	createdAt time.Time
}

func (r *PGXReaderVNextRepository) AppendThoughtOps(ctx context.Context, ops []model.ReaderThoughtOp) ([]model.ReaderThoughtAck, error) {
	if len(ops) == 0 {
		return []model.ReaderThoughtAck{}, nil
	}
	var acks []model.ReaderThoughtAck
	err := r.withTx(ctx, func(db database.Querier) error {
		duplicates, newOps, err := classifyThoughtOps(ctx, db, ops)
		if err != nil {
			return err
		}
		acks, err = r.appendThoughtOpsLocked(ctx, db, ops, newOps, duplicates)
		return err
	})
	if err != nil {
		return nil, err
	}
	return acks, nil
}

func classifyThoughtOps(ctx context.Context, db database.Querier, ops []model.ReaderThoughtOp) ([]*duplicateThoughtOp, []model.ReaderThoughtOp, error) {
	duplicates := make([]*duplicateThoughtOp, len(ops))
	newOps := make([]model.ReaderThoughtOp, 0, len(ops))
	for index, op := range ops {
		if err := validateReaderThoughtOpEnvelope(op); err != nil {
			return nil, nil, err
		}
		if op.Reattach != nil {
			sequence, exists, err := existingClientReattach(ctx, db, op)
			if err != nil {
				return nil, nil, err
			}
			if exists {
				duplicates[index] = &duplicateThoughtOp{sequence: sequence}
				continue
			}
			newOps = append(newOps, op)
			continue
		}
		sequence, createdAt, exists, err := readExistingThoughtOp(ctx, db, op)
		if err != nil {
			return nil, nil, err
		}
		if exists {
			duplicates[index] = &duplicateThoughtOp{sequence: sequence, createdAt: createdAt}
			continue
		}
		newOps = append(newOps, op)
	}
	return duplicates, newOps, nil
}

func (r *PGXReaderVNextRepository) appendThoughtOpsLocked(
	ctx context.Context,
	db database.Querier,
	ops, newOps []model.ReaderThoughtOp,
	duplicates []*duplicateThoughtOp,
) ([]model.ReaderThoughtAck, error) {
	// An accepted operation has an immutable envelope, so duplicates can be
	// recognized before checking whether their former host is still live.
	if err := lockReaderThoughtOpHosts(ctx, db, newOps); err != nil {
		return nil, err
	}
	if err := lockReaderThoughtOps(ctx, db, ops); err != nil {
		return nil, err
	}
	acks := make([]model.ReaderThoughtAck, 0, len(ops))
	for index, op := range ops {
		ack, err := r.appendOneThoughtOp(ctx, db, op, duplicates[index])
		if err != nil {
			return nil, err
		}
		acks = append(acks, ack)
	}
	if err := r.fillThoughtWinnerKeys(ctx, db, ops, acks); err != nil {
		return nil, err
	}
	return acks, nil
}

func (r *PGXReaderVNextRepository) appendOneThoughtOp(ctx context.Context, db database.Querier, op model.ReaderThoughtOp, existing *duplicateThoughtOp) (model.ReaderThoughtAck, error) {
	if existing != nil {
		op.CreatedAt = existing.createdAt
		return model.ReaderThoughtAck{OpID: op.OpID, Sequence: existing.sequence, Disposition: "duplicate", SubmittedKey: thoughtVersionKey(op)}, nil
	}
	if op.Reattach != nil {
		sequence, duplicate, err := r.appendClientReattachThoughtOp(ctx, db, op)
		if err != nil {
			return model.ReaderThoughtAck{}, err
		}
		disposition := "superseded"
		if duplicate {
			disposition = "duplicate"
		}
		return model.ReaderThoughtAck{OpID: op.OpID, Sequence: sequence, Disposition: disposition, SubmittedKey: thoughtVersionKey(op)}, nil
	}
	sequence, createdAt, duplicate, err := r.appendThoughtOp(ctx, db, op)
	if err != nil {
		return model.ReaderThoughtAck{}, err
	}
	op.CreatedAt = createdAt
	ack := model.ReaderThoughtAck{OpID: op.OpID, Sequence: sequence, Disposition: "superseded", SubmittedKey: thoughtVersionKey(op)}
	if duplicate {
		ack.Disposition = "duplicate"
		return ack, nil
	}
	if err := r.validateThoughtRecovery(ctx, db, op); err != nil {
		return model.ReaderThoughtAck{}, err
	}
	if err := r.materializeThought(ctx, db, op, sequence); err != nil {
		return model.ReaderThoughtAck{}, err
	}
	return ack, nil
}

func (r *PGXReaderVNextRepository) fillThoughtWinnerKeys(ctx context.Context, db database.Querier, ops []model.ReaderThoughtOp, acks []model.ReaderThoughtAck) error {
	for index := range acks {
		winner, err := r.readThoughtWinnerKey(ctx, db, ops[index].AnnotationID)
		if err != nil {
			return err
		}
		acks[index].WinnerKey = winner
		if acks[index].Disposition != "duplicate" && acks[index].SubmittedKey.Compare(winner) == 0 {
			acks[index].Disposition = "applied"
		}
	}
	return nil
}

// lockReaderThoughtOps serializes the previous-winner read with the ensuing
// materialization for every Thought in a batch. Existing Thought rows are
// locked before their advisory lock: server-derived writers already hold the
// row while they allocate their next logical clock, so the shared order must
// be row then advisory. A first write has no row to lock; its advisory lock
// still serializes concurrent creators. Acquiring Thought ids in stable order
// also keeps multi-Thought batches from deadlocking each other.
func lockReaderThoughtOps(ctx context.Context, db database.Querier, ops []model.ReaderThoughtOp) error {
	annotationIDs := make(map[string]struct{}, len(ops))
	for _, op := range ops {
		annotationIDs[op.AnnotationID] = struct{}{}
	}
	ordered := make([]string, 0, len(annotationIDs))
	for annotationID := range annotationIDs {
		ordered = append(ordered, annotationID)
	}
	sort.Strings(ordered)
	for _, annotationID := range ordered {
		if err := lockReaderThoughtOp(ctx, db, annotationID); err != nil {
			return err
		}
	}
	return nil
}

// lockReaderThoughtOp preserves the row-then-advisory order for every
// materializing Thought writer. The row lock serializes replacement writes;
// the advisory lock covers the no-row first-write case and makes the winner
// snapshot/event transition atomic across client and server-derived writers.
func lockReaderThoughtOp(ctx context.Context, db database.Querier, annotationID string) error {
	var lockedID string
	err := db.QueryRow(ctx, `
		SELECT id
		FROM reader_thoughts
		WHERE id=$1
		FOR UPDATE`, annotationID).Scan(&lockedID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lock thought materialization row: %w", err)
	}
	if _, err := db.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, annotationID); err != nil {
		return fmt.Errorf("lock thought supersession: %w", err)
	}
	return nil
}

type readerThoughtHostLock struct {
	kind model.ReaderHostKind
	id   uuid.UUID
	mode readerThoughtHostLockMode
}

type readerThoughtHostLockMode string

const (
	readerThoughtHostShare  readerThoughtHostLockMode = "FOR SHARE"
	readerThoughtHostUpdate readerThoughtHostLockMode = "FOR UPDATE"
)

var errReaderThoughtHostUnavailable = errors.New("reader thought host is unavailable")

// lockReaderThoughtHost is the first step of every operation that changes a
// live Thought. The global order is host, Thought row, then Thought advisory
// lock. Host lifecycle writers already use host -> Thought, so keeping client
// and server-derived writers on this order prevents a host/Thought wait cycle.
func lockReaderThoughtHost(
	ctx context.Context,
	db database.Querier,
	kind model.ReaderHostKind,
	id uuid.UUID,
	mode readerThoughtHostLockMode,
) error {
	if mode != readerThoughtHostShare && mode != readerThoughtHostUpdate {
		return fmt.Errorf("%w: invalid thought host lock mode", ErrInvalidReaderThought)
	}
	var query string
	switch kind {
	case model.ReaderHostLink:
		query = `SELECT id FROM links WHERE id=$1 AND deleted_at IS NULL ` + string(mode)
	case model.ReaderHostInbox:
		query = `SELECT id FROM reader_inbox WHERE id=$1 AND deleted_at IS NULL ` + string(mode)
	case model.ReaderHostNote:
		query = `SELECT id FROM reader_notes WHERE id=$1 AND deleted_at IS NULL ` + string(mode)
	default:
		return fmt.Errorf("%w: invalid thought host kind", ErrInvalidReaderThought)
	}
	var lockedID uuid.UUID
	if err := db.QueryRow(ctx, query, id).Scan(&lockedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", errReaderThoughtHostUnavailable, kind)
		}
		return fmt.Errorf("lock %s thought host: %w", kind, err)
	}
	return nil
}

// lockReaderThoughtOpHosts serializes live thought writes with host lifecycle
// transitions. FOR SHARE permits concurrent thought batches on one host while
// conflicting with the FOR UPDATE lock used by delete, restore, and purge.
// Explicit thought deletion deliberately skips the host lock: deleting a
// thought must remain possible after its host entered trash or was purged.
func lockReaderThoughtOpHosts(ctx context.Context, db database.Querier, ops []model.ReaderThoughtOp) error {
	unique := make(map[string]readerThoughtHostLock, len(ops))
	for _, op := range ops {
		if op.OperationKind == "delete" {
			continue
		}
		kind := model.ReaderHostKind(strings.TrimSpace(op.HostKind))
		if !kind.Valid() {
			return fmt.Errorf("%w: invalid thought host kind", ErrInvalidReaderThought)
		}
		id, err := uuid.Parse(strings.TrimSpace(op.HostID))
		if err != nil {
			return fmt.Errorf("%w: invalid %s host id", ErrInvalidReaderThought, kind)
		}
		key := string(kind) + "\x00" + id.String()
		mode := readerThoughtHostShare
		if op.Reattach != nil {
			mode = readerThoughtHostUpdate
		}
		if current, ok := unique[key]; !ok || current.mode == readerThoughtHostShare && mode == readerThoughtHostUpdate {
			unique[key] = readerThoughtHostLock{kind: kind, id: id, mode: mode}
		}
	}
	locks := make([]readerThoughtHostLock, 0, len(unique))
	for _, lock := range unique {
		locks = append(locks, lock)
	}
	sort.Slice(locks, func(i, j int) bool {
		if locks[i].kind != locks[j].kind {
			return locks[i].kind < locks[j].kind
		}
		return locks[i].id.String() < locks[j].id.String()
	})
	for _, lock := range locks {
		if err := lockReaderThoughtHost(ctx, db, lock.kind, lock.id, lock.mode); err != nil {
			if errors.Is(err, errReaderThoughtHostUnavailable) {
				if lock.kind == model.ReaderHostLink {
					return ErrReaderThoughtLinkMismatch
				}
				return fmt.Errorf("%w: %s host is unavailable", ErrInvalidReaderThought, lock.kind)
			}
			return err
		}
	}
	return nil
}

func thoughtVersionKey(op model.ReaderThoughtOp) model.ReaderThoughtVersionKey {
	return model.ReaderThoughtVersionKey{
		LogicalClock: op.LogicalClock,
		DeviceID:     op.DeviceID,
		OpID:         op.OpID,
	}
}

func (r *PGXReaderVNextRepository) readThoughtWinnerKey(ctx context.Context, db database.Querier, annotationID string) (model.ReaderThoughtVersionKey, error) {
	var key model.ReaderThoughtVersionKey
	err := db.QueryRow(ctx, `
		SELECT winner_logical_clock,winner_device_id,winner_op_id
		FROM reader_thoughts
		WHERE id=$1`, annotationID).Scan(
		&key.LogicalClock, &key.DeviceID, &key.OpID,
	)
	if err != nil {
		return model.ReaderThoughtVersionKey{}, fmt.Errorf("read thought winner key: %w", err)
	}
	return key, nil
}

func validateReaderThoughtOpEnvelope(op model.ReaderThoughtOp) error {
	if op.LogicalClock < 0 || op.LogicalClock > model.ReaderThoughtMaxLogicalClock {
		return fmt.Errorf("%w: logical_clock is outside the safe range", ErrReaderThoughtClockInvalid)
	}
	if (op.RecoveryOf == nil) != (op.ExpectedWinnerKey == nil) {
		return fmt.Errorf("%w: incomplete recovery metadata", ErrInvalidReaderThought)
	}
	if op.Reattach != nil {
		if op.RecoveryOf != nil || op.ExpectedWinnerKey != nil || op.OperationKind != "update" ||
			op.Reattach.ExpectedLastSequence < 0 || op.Reattach.ExpectedHostRevision <= 0 {
			return fmt.Errorf("%w: invalid reattach metadata", ErrInvalidReaderThought)
		}
	}
	return nil
}

// readExistingThoughtOp verifies the full immutable operation envelope. It is
// safe to use before host liveness checks because reader_thought_ops rows are
// append-only; callers still take host -> Thought locks before any new write.
func readExistingThoughtOp(ctx context.Context, db database.Querier, op model.ReaderThoughtOp) (int64, time.Time, bool, error) {
	var existing struct {
		Sequence          int64
		CreatedAt         time.Time
		DeviceID          string
		LogicalClock      int64
		OperationKind     string
		AnnotationID      string
		HostKind          string
		HostID            string
		Target            []byte
		Payload           []byte
		RecoveryOf        []byte
		ExpectedWinnerKey []byte
	}
	// Provenance is part of the immutable envelope, even when this incoming
	// operation is not a recovery. Otherwise a normal operation could reuse a
	// recovery's op_id and be falsely acknowledged as an idempotent retry.
	err := db.QueryRow(ctx, `
		SELECT sequence,created_at,device_id,logical_clock,operation_kind,annotation_id,host_kind,host_id,target,payload,recovery_of,expected_winner_key
		FROM reader_thought_ops
		WHERE op_id=$1`, op.OpID).Scan(
		&existing.Sequence, &existing.CreatedAt, &existing.DeviceID, &existing.LogicalClock, &existing.OperationKind,
		&existing.AnnotationID, &existing.HostKind, &existing.HostID, &existing.Target, &existing.Payload,
		&existing.RecoveryOf, &existing.ExpectedWinnerKey,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, fmt.Errorf("read duplicate thought op: %w", err)
	}
	existingRecoveryOf, recoveryValid := thoughtVersionKeyFromJSON(existing.RecoveryOf)
	existingExpectedWinner, expectedWinnerValid := thoughtVersionKeyFromJSON(existing.ExpectedWinnerKey)
	if existing.DeviceID != op.DeviceID ||
		existing.OperationKind != op.OperationKind ||
		existing.AnnotationID != op.AnnotationID ||
		existing.HostKind != op.HostKind ||
		existing.HostID != op.HostID ||
		!readerJSONEqual(existing.Target, rawJSON(op.Target)) ||
		existing.LogicalClock != op.LogicalClock ||
		!readerJSONEqual(existing.Payload, rawJSON(op.Payload)) ||
		!recoveryValid || !expectedWinnerValid ||
		!thoughtVersionKeysEqual(existingRecoveryOf, op.RecoveryOf) ||
		!thoughtVersionKeysEqual(existingExpectedWinner, op.ExpectedWinnerKey) {
		return 0, time.Time{}, false, fmt.Errorf("%w: op_id=%s", ErrReaderThoughtOpConflict, op.OpID)
	}
	return existing.Sequence, existing.CreatedAt, true, nil
}

func (r *PGXReaderVNextRepository) appendThoughtOp(ctx context.Context, db database.Querier, op model.ReaderThoughtOp) (int64, time.Time, bool, error) {
	if err := validateReaderThoughtOpEnvelope(op); err != nil {
		return 0, time.Time{}, false, err
	}
	var sequence int64
	var createdAt time.Time
	insert := `
		INSERT INTO reader_thought_ops
			(op_id, device_id, operation_kind, annotation_id, host_kind, host_id, target, payload, logical_clock)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb,$9)
		ON CONFLICT (op_id) DO NOTHING
		RETURNING sequence,created_at`
	args := []any{op.OpID, op.DeviceID, op.OperationKind, op.AnnotationID, op.HostKind, op.HostID, rawJSON(op.Target), rawJSON(op.Payload), op.LogicalClock}
	if op.RecoveryOf != nil {
		insert = `
			INSERT INTO reader_thought_ops
			(op_id, device_id, operation_kind, annotation_id, host_kind, host_id, target, payload, logical_clock,recovery_of,expected_winner_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb,$9,$10::jsonb,$11::jsonb)
		ON CONFLICT (op_id) DO NOTHING
		RETURNING sequence,created_at`
		args = append(args, thoughtVersionKeyJSON(op.RecoveryOf), thoughtVersionKeyJSON(op.ExpectedWinnerKey))
	}
	err := db.QueryRow(ctx, insert, args...).Scan(&sequence, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		sequence, createdAt, exists, readErr := readExistingThoughtOp(ctx, db, op)
		if readErr != nil {
			return 0, time.Time{}, false, readErr
		}
		if !exists {
			return 0, time.Time{}, false, fmt.Errorf("read duplicate thought op: conflict row disappeared")
		}
		return sequence, createdAt, true, nil
	} else if err != nil {
		return 0, time.Time{}, false, fmt.Errorf("append thought op: %w", err)
	}
	return sequence, createdAt, false, nil
}

type readerThoughtReattachMarker struct {
	ExpectedLastSequence int64 `json:"expected_last_sequence"`
	ExpectedHostRevision int64 `json:"expected_host_revision"`
}

type existingReaderThoughtReattach struct {
	Sequence          int64
	DeviceID          string
	LogicalClock      int64
	OperationKind     string
	AnnotationID      string
	HostKind          string
	HostID            string
	Target            []byte
	Payload           []byte
	RecoveryOf        []byte
	ExpectedWinnerKey []byte
}

func readerThoughtReattachMarkerFromPayload(raw json.RawMessage) (*readerThoughtReattachMarker, bool) {
	var payload struct {
		Reattach *readerThoughtReattachMarker `json:"reattach"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Reattach == nil ||
		payload.Reattach.ExpectedLastSequence < 0 || payload.Reattach.ExpectedHostRevision <= 0 {
		return nil, false
	}
	return payload.Reattach, true
}

func readerThoughtReattachMatches(existing existingReaderThoughtReattach, op model.ReaderThoughtOp) bool {
	marker, marked := readerThoughtReattachMarkerFromPayload(existing.Payload)
	recoveryOf, recoveryValid := thoughtVersionKeyFromJSON(existing.RecoveryOf)
	expectedWinner, winnerValid := thoughtVersionKeyFromJSON(existing.ExpectedWinnerKey)
	return op.Reattach != nil && marked && recoveryValid && winnerValid && recoveryOf == nil && expectedWinner == nil &&
		existing.DeviceID == op.DeviceID && existing.LogicalClock == op.LogicalClock &&
		existing.OperationKind == "update" && existing.AnnotationID == op.AnnotationID &&
		existing.HostKind == op.HostKind && existing.HostID == op.HostID &&
		readerJSONEqual(existing.Target, rawJSON(op.Target)) &&
		marker.ExpectedLastSequence == op.Reattach.ExpectedLastSequence &&
		marker.ExpectedHostRevision == op.Reattach.ExpectedHostRevision
}

// existingClientReattach verifies a prior server-rebuilt reattach using the
// client command's immutable identity and its persisted CAS marker. It runs
// before lifecycle checks so a lost-response retry remains a duplicate even
// after the tombstone was cleared or a later operation won.
func existingClientReattach(ctx context.Context, db database.Querier, op model.ReaderThoughtOp) (int64, bool, error) {
	var existing existingReaderThoughtReattach
	err := db.QueryRow(ctx, `
		SELECT sequence,device_id,logical_clock,operation_kind,annotation_id,host_kind,host_id,target,payload,recovery_of,expected_winner_key
		FROM reader_thought_ops
		WHERE op_id=$1`, op.OpID).Scan(
		&existing.Sequence, &existing.DeviceID, &existing.LogicalClock, &existing.OperationKind,
		&existing.AnnotationID, &existing.HostKind, &existing.HostID, &existing.Target, &existing.Payload,
		&existing.RecoveryOf, &existing.ExpectedWinnerKey,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read existing client reattach: %w", err)
	}
	if !readerThoughtReattachMatches(existing, op) {
		return 0, false, fmt.Errorf("%w: op_id=%s", ErrReaderThoughtOpConflict, op.OpID)
	}
	return existing.Sequence, true, nil
}

// appendClientReattachThoughtOp turns one durable browser command into a
// normal update operation rebuilt exclusively from the immutable tombstone.
// AppendThoughtOps has already locked the target host, Thought row, and
// advisory key in global order before this method is reached.
//
//nolint:gocyclo // one transaction must preserve duplicate, lifecycle, host CAS, materialization, winner CAS, and tombstone-clear order.
func (r *PGXReaderVNextRepository) appendClientReattachThoughtOp(ctx context.Context, db database.Querier, op model.ReaderThoughtOp) (int64, bool, error) {
	if op.Reattach == nil {
		return 0, false, ErrInvalidReaderThought
	}
	if sequence, found, err := existingClientReattach(ctx, db, op); err != nil || found {
		return sequence, found, err
	}
	item, err := scanReaderThought(db.QueryRow(ctx, `SELECT `+readerThoughtColumns+` FROM reader_thoughts WHERE id=$1 AND deleted=false FOR UPDATE`, op.AnnotationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, ErrNotFound
	}
	if err != nil {
		return 0, false, fmt.Errorf("read thought for client reattach: %w", err)
	}
	if op.Reattach.ExpectedLastSequence != item.LastSequence || thoughtVersionKey(op).Compare(item.WinnerKey) <= 0 {
		return 0, false, ErrRevisionConflict
	}
	var snapshot []byte
	if err := db.QueryRow(ctx, `SELECT snapshot FROM reader_thought_tombstones WHERE thought_id=$1 FOR UPDATE`, op.AnnotationID).Scan(&snapshot); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, ErrReaderThoughtReattachInvalidState
		}
		return 0, false, fmt.Errorf("read client reattach snapshot: %w", err)
	}
	if err := applyReaderThoughtReplaySnapshot(item, snapshot); err != nil {
		return 0, false, fmt.Errorf("decode client reattach snapshot: %w", err)
	}
	command := model.ReaderThoughtReattachCommand{
		ThoughtID: op.AnnotationID, TargetHostKind: op.HostKind, TargetHostID: op.HostID,
		ExpectedLastSequence: op.Reattach.ExpectedLastSequence, ExpectedHostRevision: op.Reattach.ExpectedHostRevision,
	}
	hostRevision, err := readerReattachHost(ctx, db, command)
	if err != nil {
		return 0, false, err
	}
	if hostRevision != op.Reattach.ExpectedHostRevision {
		return 0, false, ErrRevisionConflict
	}
	hostBody, err := readerReattachHostBody(ctx, db, command.TargetHostKind, command.TargetHostID)
	if err != nil {
		return 0, false, err
	}
	target, payload, err := readerReattachThoughtPayloadWithMarker(item, command, hostRevision, hostBody, op.Reattach)
	if err != nil {
		return 0, false, err
	}
	if !readerJSONEqual(target, rawJSON(op.Target)) {
		return 0, false, ErrInvalidReaderThought
	}
	materialized := op
	materialized.Target = target
	materialized.Payload = payload
	materialized.Reattach = nil
	sequence, createdAt, duplicate, err := r.appendThoughtOp(ctx, db, materialized)
	if err != nil || duplicate {
		return sequence, duplicate, err
	}
	materialized.CreatedAt = createdAt
	if err := r.materializeThought(ctx, db, materialized, sequence); err != nil {
		return 0, false, err
	}
	winner, err := r.readThoughtWinnerKey(ctx, db, op.AnnotationID)
	if err != nil {
		return 0, false, err
	}
	if thoughtVersionKey(op).Compare(winner) != 0 {
		return 0, false, ErrRevisionConflict
	}
	if _, err := db.Exec(ctx, `DELETE FROM reader_thought_tombstones WHERE thought_id=$1`, op.AnnotationID); err != nil {
		return 0, false, fmt.Errorf("clear client reattach tombstone: %w", err)
	}
	return sequence, false, nil
}

func thoughtVersionKeyJSON(key *model.ReaderThoughtVersionKey) []byte {
	if key == nil {
		return nil
	}
	value, err := json.Marshal(map[string]any{
		"logical_clock": key.LogicalClock,
		"device_id":     key.DeviceID,
		"op_id":         key.OpID,
	})
	if err != nil {
		return nil
	}
	return value
}

func thoughtVersionKeyFromJSON(raw []byte) (*model.ReaderThoughtVersionKey, bool) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 3 ||
		fields["logical_clock"] == nil || fields["device_id"] == nil || fields["op_id"] == nil {
		return nil, false
	}
	var value struct {
		LogicalClock int64  `json:"logical_clock"`
		DeviceID     string `json:"device_id"`
		OpID         string `json:"op_id"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false
	}
	if value.LogicalClock < 0 || value.LogicalClock > model.ReaderThoughtMaxLogicalClock ||
		value.DeviceID == "" || value.OpID == "" {
		return nil, false
	}
	return &model.ReaderThoughtVersionKey{LogicalClock: value.LogicalClock, DeviceID: value.DeviceID, OpID: value.OpID}, true
}

func thoughtVersionKeysEqual(left, right *model.ReaderThoughtVersionKey) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.LogicalClock == right.LogicalClock &&
		left.DeviceID == right.DeviceID && left.OpID == right.OpID
}

func (r *PGXReaderVNextRepository) validateThoughtRecovery(ctx context.Context, db database.Querier, op model.ReaderThoughtOp) error {
	if op.RecoveryOf == nil && op.ExpectedWinnerKey == nil {
		return nil
	}
	if op.RecoveryOf == nil || op.ExpectedWinnerKey == nil {
		return fmt.Errorf("%w: incomplete recovery metadata", ErrInvalidReaderThought)
	}
	var recoverable bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM reader_thought_supersession_events event
			JOIN reader_thought_ops loser
			  ON loser.sequence=event.loser_sequence
			WHERE event.annotation_id=$1
			  AND loser.logical_clock=$2
			  AND loser.device_id=$3
			  AND loser.op_id=$4
		)`,
		op.AnnotationID,
		op.RecoveryOf.LogicalClock, op.RecoveryOf.DeviceID, op.RecoveryOf.OpID,
	).Scan(&recoverable); err != nil {
		return fmt.Errorf("verify thought recovery provenance: %w", err)
	}
	if !recoverable {
		return fmt.Errorf("%w: recovery_of is not a durable superseded version", ErrInvalidReaderThought)
	}
	var current model.ReaderThoughtVersionKey
	err := db.QueryRow(ctx, `
		SELECT winner_logical_clock,winner_device_id,winner_op_id
		FROM reader_thoughts
		WHERE id=$1
		FOR UPDATE`, op.AnnotationID).Scan(
		&current.LogicalClock, &current.DeviceID, &current.OpID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no current winner", ErrReaderThoughtRecoveryConflict)
	}
	if err != nil {
		return fmt.Errorf("lock thought recovery winner: %w", err)
	}
	if current.Compare(*op.ExpectedWinnerKey) != 0 {
		return fmt.Errorf("%w: expected=%d/%s/%s actual=%d/%s/%s",
			ErrReaderThoughtRecoveryConflict,
			op.ExpectedWinnerKey.LogicalClock, op.ExpectedWinnerKey.DeviceID, op.ExpectedWinnerKey.OpID,
			current.LogicalClock, current.DeviceID, current.OpID,
		)
	}
	return nil
}

// appendDerivedThoughtOp serializes server-owned writers per thought,
// reuses an already accepted idempotent operation's clock, and otherwise
// allocates exactly current_winner_clock+1 in the caller's transaction.
func (r *PGXReaderVNextRepository) appendDerivedThoughtOp(
	ctx context.Context,
	db database.Querier,
	op model.ReaderThoughtOp,
) (model.ReaderThoughtOp, int64, bool, error) {
	var existingClock int64
	err := db.QueryRow(ctx, `
		SELECT logical_clock
		FROM reader_thought_ops
		WHERE op_id=$1`, op.OpID).Scan(&existingClock)
	if err == nil {
		op.LogicalClock = existingClock
		sequence, createdAt, duplicate, appendErr := r.appendThoughtOp(ctx, db, op)
		op.CreatedAt = createdAt
		return op, sequence, duplicate, appendErr
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return model.ReaderThoughtOp{}, 0, false, fmt.Errorf("read derived thought operation: %w", err)
	}
	if err := lockReaderThoughtOp(ctx, db, op.AnnotationID); err != nil {
		return model.ReaderThoughtOp{}, 0, false, err
	}

	var winnerClock int64
	err = db.QueryRow(ctx, `
		SELECT winner_logical_clock
		FROM reader_thoughts
		WHERE id=$1
		FOR UPDATE`, op.AnnotationID).Scan(&winnerClock)
	if errors.Is(err, pgx.ErrNoRows) {
		winnerClock = 0
	} else if err != nil {
		return model.ReaderThoughtOp{}, 0, false, fmt.Errorf("lock thought winner: %w", err)
	}
	if winnerClock < 0 || winnerClock > model.ReaderThoughtMaxLogicalClock {
		return model.ReaderThoughtOp{}, 0, false, fmt.Errorf("%w: persisted winner clock is invalid", ErrReaderThoughtClockInvalid)
	}
	if winnerClock == model.ReaderThoughtMaxLogicalClock {
		return model.ReaderThoughtOp{}, 0, false, ErrReaderThoughtClockExhausted
	}
	op.LogicalClock = winnerClock + 1
	sequence, createdAt, duplicate, err := r.appendThoughtOp(ctx, db, op)
	op.CreatedAt = createdAt
	return op, sequence, duplicate, err
}

func readerJSONEqual(left, right []byte) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

type readerThoughtPayload struct {
	Body   string          `json:"body"`
	Quote  json.RawMessage `json:"quote"`
	Source string          `json:"source"`
	LinkID string          `json:"link_id"`
}

func readReaderThoughtPayload(ctx context.Context, db database.Querier, op model.ReaderThoughtOp) (readerThoughtPayload, *uuid.UUID, error) {
	var payload readerThoughtPayload
	if err := json.Unmarshal(op.Payload, &payload); err != nil {
		return payload, nil, fmt.Errorf("decode thought payload: %w", err)
	}
	if payload.Source == "" {
		payload.Source = "user"
	}
	if op.HostKind != "link" {
		if payload.LinkID != "" {
			return payload, nil, fmt.Errorf("%w: non-link host cannot carry link_id", ErrReaderThoughtLinkMismatch)
		}
		return payload, nil, nil
	}

	hostLinkID, err := uuid.Parse(op.HostID)
	if err != nil {
		return payload, nil, fmt.Errorf("%w: invalid link host id", ErrReaderThoughtLinkMismatch)
	}
	if payload.LinkID != "" {
		payloadLinkID, err := uuid.Parse(payload.LinkID)
		if err != nil || payloadLinkID != hostLinkID {
			return payload, nil, fmt.Errorf("%w: payload link_id differs from host", ErrReaderThoughtLinkMismatch)
		}
	}
	if op.OperationKind != "delete" {
		var exists bool
		if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM links WHERE id=$1 AND deleted_at IS NULL)`, hostLinkID).Scan(&exists); err != nil {
			return payload, nil, fmt.Errorf("check thought link host: %w", err)
		}
		if !exists {
			return payload, nil, ErrReaderThoughtLinkMismatch
		}
	}
	return payload, &hostLinkID, nil
}

func (r *PGXReaderVNextRepository) materializeThought(ctx context.Context, db database.Querier, op model.ReaderThoughtOp, sequence int64) error {
	// Read the previous winner before the conditional upsert.  The event is a
	// write-time fact, not a query-time comparison against a future winner.
	var previous model.ReaderThoughtConflictOperation
	previousOK := false
	var err error
	previous, previousOK, err = r.readThoughtOperation(ctx, db, op.AnnotationID)
	if err != nil {
		return err
	}
	payload, linkID, err := readReaderThoughtPayload(ctx, db, op)
	if err != nil {
		return err
	}
	deleted := op.OperationKind == "delete"
	quote := payload.Quote
	if len(quote) == 0 {
		quote = nil
	}
	target := rawJSON(op.Target)
	if deleted {
		linkID = nil
		target = []byte(`{}`)
		quote = nil
		payload.Body = ""
		payload.Source = ""
	}
	// Keep the legacy sequence predicate syntactically present for the
	// materialized projection contract, but make it an exhaustive partition.
	// The actual winner gate below is solely the stable client version key.
	tag, err := db.Exec(ctx, `
		INSERT INTO reader_thoughts
			(id, host_kind, host_id, link_id, target, quote, body, source, deleted, last_sequence,
			 winner_logical_clock,winner_device_id,winner_op_id)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (id) DO UPDATE SET
			host_kind=CASE WHEN EXCLUDED.deleted THEN reader_thoughts.host_kind ELSE EXCLUDED.host_kind END,
			host_id=CASE WHEN EXCLUDED.deleted THEN reader_thoughts.host_id ELSE EXCLUDED.host_id END,
			link_id=CASE WHEN EXCLUDED.deleted THEN NULL ELSE EXCLUDED.link_id END,
			target=EXCLUDED.target,
			quote=EXCLUDED.quote,
			body=EXCLUDED.body,
			source=EXCLUDED.source,
			deleted=EXCLUDED.deleted, last_sequence=EXCLUDED.last_sequence,
			winner_logical_clock=EXCLUDED.winner_logical_clock,
			winner_device_id=EXCLUDED.winner_device_id,
			winner_op_id=EXCLUDED.winner_op_id,
		updated_at=NOW()
	WHERE (EXCLUDED.winner_logical_clock, convert_to(EXCLUDED.winner_device_id,'UTF8'), convert_to(EXCLUDED.winner_op_id,'UTF8'))
		> (reader_thoughts.winner_logical_clock, convert_to(reader_thoughts.winner_device_id,'UTF8'), convert_to(reader_thoughts.winner_op_id,'UTF8'))`,
		op.AnnotationID, op.HostKind, op.HostID, linkID, target, rawJSON(quote), payload.Body, payload.Source, deleted, sequence, op.LogicalClock, op.DeviceID, op.OpID)
	if err != nil {
		return fmt.Errorf("materialize thought: %w", err)
	}
	if deleted && tag.RowsAffected() > 0 {
		// Delete has no LWW privilege. Only a delete that won the projection may
		// replace a host lifecycle snapshot with the minimal anti-resurrection
		// marker. A delayed losing delete remains an append-only sync event and
		// must leave the frozen history intact.
		if _, err := db.Exec(ctx, `
			INSERT INTO reader_thought_tombstones (thought_id,host_kind,host_id,reason,snapshot)
			VALUES ($1,$2,$3,'user_deleted',jsonb_build_object('id',$1::text,'host_kind',$2::text,'host_id',$3::text,'lifecycle_sequence',$4::bigint))
			ON CONFLICT (thought_id) DO UPDATE
			SET reason='user_deleted',snapshot=jsonb_build_object('id',EXCLUDED.thought_id,'host_kind',EXCLUDED.host_kind,'host_id',EXCLUDED.host_id,'lifecycle_sequence',$4::bigint),created_at=NOW()`,
			op.AnnotationID, op.HostKind, op.HostID, sequence); err != nil {
			return fmt.Errorf("mark thought user deletion: %w", err)
		}
	} else if !deleted && tag.RowsAffected() > 0 && previousOK && previous.OperationKind == "delete" {
		// A user-delete marker belongs to the delete version that created it;
		// it cannot give that version priority over a later operation in the
		// total LWW order. Clearing only user_deleted preserves immutable host
		// lifecycle snapshots while allowing a genuinely newer client version
		// to converge regardless of delivery order.
		if _, err := db.Exec(ctx, `DELETE FROM reader_thought_tombstones WHERE thought_id=$1 AND reason='user_deleted'`, op.AnnotationID); err != nil {
			return fmt.Errorf("clear superseded thought user deletion: %w", err)
		}
	}
	// The projection is maintained here rather than at each command, because
	// every Thought write — add, update, delete, reattach, lifecycle, note
	// reanchor, checkbox writeback, history restore — ends in this function.
	// It runs after the tombstone bookkeeping above so it observes the final
	// liveness of the Thought, not the intermediate one.
	if err := r.replaceThoughtTodoProjectionsOn(ctx, db, op.AnnotationID); err != nil {
		return err
	}
	return r.recordThoughtSupersession(ctx, db, op, sequence, previous, previousOK)
}

func (r *PGXReaderVNextRepository) recordThoughtSupersession(ctx context.Context, db database.Querier, op model.ReaderThoughtOp, sequence int64, previous model.ReaderThoughtConflictOperation, previousOK bool) error {
	if !previousOK {
		return nil
	}
	incoming := thoughtConflictOperation(op, sequence)
	winner, loser := incoming, previous
	if thoughtOperationKey(previous).Compare(thoughtOperationKey(incoming)) > 0 {
		winner, loser = previous, incoming
	}
	if winner.OpID == loser.OpID {
		return nil
	}
	if err := r.appendThoughtSupersessionEvent(ctx, db, op.AnnotationID, loser, winner); err != nil {
		return err
	}
	return nil
}

func thoughtConflictOperation(op model.ReaderThoughtOp, sequence int64) model.ReaderThoughtConflictOperation {
	return model.ReaderThoughtConflictOperation{Sequence: sequence, OpID: op.OpID, DeviceID: op.DeviceID, LogicalClock: op.LogicalClock, OperationKind: op.OperationKind, AnnotationID: op.AnnotationID, HostKind: op.HostKind, HostID: op.HostID, Target: append(json.RawMessage(nil), op.Target...), Payload: append(json.RawMessage(nil), op.Payload...), RecoveryOf: cloneThoughtVersionKey(op.RecoveryOf), ExpectedWinnerKey: cloneThoughtVersionKey(op.ExpectedWinnerKey), CreatedAt: op.CreatedAt}
}

func cloneThoughtVersionKey(key *model.ReaderThoughtVersionKey) *model.ReaderThoughtVersionKey {
	if key == nil {
		return nil
	}
	value := *key
	return &value
}

func thoughtOperationKey(op model.ReaderThoughtConflictOperation) model.ReaderThoughtVersionKey {
	return model.ReaderThoughtVersionKey{LogicalClock: op.LogicalClock, DeviceID: op.DeviceID, OpID: op.OpID}
}

func (r *PGXReaderVNextRepository) readThoughtOperation(ctx context.Context, db database.Querier, annotationID string) (model.ReaderThoughtConflictOperation, bool, error) {
	var op model.ReaderThoughtConflictOperation
	var target, payload []byte
	var recoveryOf, expectedWinner []byte
	err := db.QueryRow(ctx, `SELECT operation.sequence,operation.op_id,operation.device_id,operation.logical_clock,operation.operation_kind,operation.annotation_id,operation.host_kind,operation.host_id,operation.target,operation.payload,operation.recovery_of,operation.expected_winner_key,operation.created_at FROM reader_thoughts thought JOIN reader_thought_ops operation ON operation.sequence=thought.last_sequence WHERE thought.id=$1`, annotationID).Scan(&op.Sequence, &op.OpID, &op.DeviceID, &op.LogicalClock, &op.OperationKind, &op.AnnotationID, &op.HostKind, &op.HostID, &target, &payload, &recoveryOf, &expectedWinner, &op.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ReaderThoughtConflictOperation{}, false, nil
	}
	if err != nil {
		return model.ReaderThoughtConflictOperation{}, false, fmt.Errorf("read thought winner operation: %w", err)
	}
	op.Target, op.Payload = append(json.RawMessage(nil), target...), append(json.RawMessage(nil), payload...)
	parsedRecoveryOf, recoveryValid := thoughtVersionKeyFromJSON(recoveryOf)
	parsedExpectedWinner, expectedWinnerValid := thoughtVersionKeyFromJSON(expectedWinner)
	op.RecoveryOf = parsedRecoveryOf
	op.ExpectedWinnerKey = parsedExpectedWinner
	if !recoveryValid || !expectedWinnerValid || (op.RecoveryOf == nil) != (op.ExpectedWinnerKey == nil) {
		return model.ReaderThoughtConflictOperation{}, false, fmt.Errorf("read thought winner recovery metadata: invalid recovery provenance")
	}
	return op, true, nil
}

func (r *PGXReaderVNextRepository) appendThoughtSupersessionEvent(ctx context.Context, db database.Querier, annotationID string, loser, winner model.ReaderThoughtConflictOperation) error {
	loserJSON, err := json.Marshal(loser)
	if err != nil {
		return fmt.Errorf("encode thought supersession loser: %w", err)
	}
	winnerJSON, err := json.Marshal(winner)
	if err != nil {
		return fmt.Errorf("encode thought supersession winner: %w", err)
	}
	_, err = db.Exec(ctx, `INSERT INTO reader_thought_supersession_events (annotation_id,loser_sequence,winner_sequence,loser,winner_at_detection) VALUES ($1,$2,$3,$4::jsonb,$5::jsonb) ON CONFLICT (loser_sequence) DO NOTHING`, annotationID, loser.Sequence, winner.Sequence, loserJSON, winnerJSON)
	if err != nil {
		return fmt.Errorf("append thought supersession event: %w", err)
	}
	return nil
}

func (r *PGXReaderVNextRepository) ListThoughts(ctx context.Context, query, after string, limit int) ([]model.ReaderThought, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	at, id, err := parseReaderCursor(after)
	if err != nil {
		return nil, "", err
	}
	sql := `SELECT ` + readerThoughtColumns + ` FROM reader_thoughts WHERE deleted=false AND NOT EXISTS (SELECT 1 FROM reader_thought_tombstones tt WHERE tt.thought_id=reader_thoughts.id)`
	args := []any{}
	if strings.TrimSpace(query) != "" {
		sql += fmt.Sprintf(` AND body ILIKE $%d`, len(args)+1)
		args = append(args, "%"+strings.TrimSpace(query)+"%")
	}
	if !at.IsZero() {
		sql += fmt.Sprintf(` AND (updated_at < $%d OR (updated_at = $%d AND id < $%d))`, len(args)+1, len(args)+1, len(args)+2)
		args = append(args, at, id)
	}
	sql += fmt.Sprintf(` ORDER BY updated_at DESC, id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list thoughts: %w", err)
	}
	defer rows.Close()
	items := make([]model.ReaderThought, 0)
	for rows.Next() {
		item, err := scanReaderThought(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan thought: %w", err)
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) == limit {
		last := items[len(items)-1]
		return items, readerCursor(last.UpdatedAt, last.ID), nil
	}
	return items, "", nil
}

const thoughtSearchCursorVersion = 2

type thoughtSearchCursorPayload struct {
	Version          int    `json:"v"`
	UpdatedAt        string `json:"updated_at"`
	ThoughtID        string `json:"thought_id"`
	Scope            string `json:"scope"`
	SnapshotSequence int64  `json:"snapshot_sequence"`
	SnapshotAt       string `json:"snapshot_at"`
	Total            int    `json:"total"`
}

func canonicalThoughtSearchQuery(query string) string {
	return strings.ToLower(strings.TrimSpace(query))
}

func thoughtSearchCursorScope(query string) string {
	digest := sha256.Sum256([]byte(canonicalThoughtSearchQuery(query)))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func thoughtSearchCursor(at time.Time, id, query string, snapshotSequence int64, snapshotAt time.Time, total int) (string, error) {
	payload, err := json.Marshal(thoughtSearchCursorPayload{
		Version:          thoughtSearchCursorVersion,
		UpdatedAt:        at.UTC().Format(time.RFC3339Nano),
		ThoughtID:        id,
		Scope:            thoughtSearchCursorScope(query),
		SnapshotSequence: snapshotSequence,
		SnapshotAt:       snapshotAt.UTC().Format(time.RFC3339Nano),
		Total:            total,
	})
	if err != nil {
		return "", fmt.Errorf("encode thought search cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func parseThoughtSearchCursor(raw, query string) (time.Time, string, int64, time.Time, int, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, "", 0, time.Time{}, -1, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", 0, time.Time{}, 0, fmt.Errorf("%w: malformed thought search cursor", ErrInvalidReaderCursor)
	}
	var payload thoughtSearchCursorPayload
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return time.Time{}, "", 0, time.Time{}, 0, fmt.Errorf("%w: malformed thought search cursor", ErrInvalidReaderCursor)
	}
	if payload.Version != thoughtSearchCursorVersion || payload.ThoughtID == "" || payload.Scope == "" || payload.SnapshotSequence < 0 || payload.Total < 0 {
		return time.Time{}, "", 0, time.Time{}, 0, fmt.Errorf("%w: malformed thought search cursor", ErrInvalidReaderCursor)
	}
	at, err := time.Parse(time.RFC3339Nano, payload.UpdatedAt)
	if err != nil {
		return time.Time{}, "", 0, time.Time{}, 0, fmt.Errorf("%w: malformed thought search cursor", ErrInvalidReaderCursor)
	}
	snapshotAt, err := time.Parse(time.RFC3339Nano, payload.SnapshotAt)
	if err != nil {
		return time.Time{}, "", 0, time.Time{}, 0, fmt.Errorf("%w: malformed thought search cursor", ErrInvalidReaderCursor)
	}
	if subtle.ConstantTimeCompare([]byte(payload.Scope), []byte(thoughtSearchCursorScope(query))) != 1 {
		return time.Time{}, "", 0, time.Time{}, 0, fmt.Errorf("%w: thought search cursor scope mismatch", ErrInvalidReaderCursor)
	}
	return at, payload.ThoughtID, payload.SnapshotSequence, snapshotAt, payload.Total, nil
}

func scanThoughtSearchRows(rows pgx.Rows, limit int, query string, cursorTotal int) ([]model.ReaderThoughtSearch, int, string, error) {
	items := make([]model.ReaderThoughtSearch, 0)
	total := cursorTotal
	totalIsAuthoritative := cursorTotal >= 0
	var snapshotSequence int64
	var snapshotAt time.Time
	for rows.Next() {
		var item model.ReaderThoughtSearch
		var rawLinkID string
		var totalRows int64
		var reason pgtype.Text
		if err := rows.Scan(&item.ID, &item.HostKind, &item.HostID, &rawLinkID, &item.Snippet, &totalRows, &item.UpdatedAt, &item.LifecycleStatus, &reason, &item.HistoryDeepLink, &snapshotSequence, &snapshotAt); err != nil {
			return nil, 0, "", fmt.Errorf("scan thought search result: %w", err)
		}
		if rawLinkID != "" {
			linkID, err := uuid.Parse(rawLinkID)
			if err != nil {
				return nil, 0, "", fmt.Errorf("scan thought search link id: %w", err)
			}
			item.LinkID = &linkID
		}
		if reason.Valid {
			value := reason.String
			item.LifecycleReason = &value
		}
		if itemTotal := int(totalRows); !totalIsAuthoritative && (total < 0 || itemTotal > total) {
			total = itemTotal
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, "", err
	}
	if len(items) <= limit {
		if total < 0 {
			total = 0
		}
		return items, total, "", nil
	}
	items = items[:limit]
	last := items[len(items)-1]
	nextCursor, err := thoughtSearchCursor(last.UpdatedAt, last.ID, query, snapshotSequence, snapshotAt, total)
	if err != nil {
		return nil, 0, "", err
	}
	return items, total, nextCursor, nil
}

func (r *PGXReaderVNextRepository) SearchThoughts(ctx context.Context, query, after string, limit int) ([]model.ReaderThoughtSearch, int, string, error) {
	query = canonicalThoughtSearchQuery(query)
	if query == "" {
		return []model.ReaderThoughtSearch{}, 0, "", nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	at, id, snapshotSequence, snapshotAt, cursorTotal, err := parseThoughtSearchCursor(after, query)
	if err != nil {
		return nil, 0, "", fmt.Errorf("parse thought search cursor: %w", err)
	}
	// Count before applying the keyset condition so every returned page carries
	// the total match hint, rather than only the number still after its cursor.
	sql := `WITH search_authority AS (
		SELECT COALESCE($2::bigint, GREATEST(
			(SELECT COALESCE(max(sequence),0) FROM reader_thought_ops),
			(SELECT COALESCE(max(last_sequence),0) FROM reader_thoughts)
		)) AS snapshot_sequence,
		COALESCE($3::timestamptz,statement_timestamp()) AS snapshot_at
	), search_candidates AS MATERIALIZED (
		-- 唯一目的是让 planner 分别命中 active / tombstone 的两个 trigram 索引。
		-- 两个分支的表达式与下面 WHERE 里那组 ILIKE 完全同构：某一列包含
		-- %query% 的子串，拼接串必然也包含它，所以这里是**必要条件**，
		-- 不是新的过滤器——多进来的行照样被下面原封不动的 OR 复核掉。
		-- 语义因此逐字不变；变的只有扫描方式。
		-- UNION（非 UNION ALL）保证一个 thought_id 至多出现一次，JOIN 不会放大行数。
		SELECT thought.id AS thought_id
		FROM reader_thoughts thought
		WHERE (thought.body || ' ' || thought.source || ' ' || COALESCE(thought.quote::text,'')) ILIKE $1
		UNION
		SELECT tombstone.thought_id
		FROM reader_thought_tombstones tombstone
		WHERE (COALESCE(tombstone.snapshot->>'body','') || ' ' || COALESCE(tombstone.snapshot->>'source','')
			|| ' ' || COALESCE((tombstone.snapshot->'quote')::text,'')) ILIKE $1
	), matching_thoughts AS (
		SELECT thought.id AS thought_id,
			CASE WHEN tombstone.thought_id IS NULL THEN thought.host_kind ELSE tombstone.snapshot->>'host_kind' END AS host_kind,
			CASE WHEN tombstone.thought_id IS NULL THEN thought.host_id ELSE tombstone.snapshot->>'host_id' END AS host_id,
			CASE WHEN tombstone.thought_id IS NULL THEN COALESCE(thought.link_id::text, '') ELSE COALESCE(tombstone.snapshot->>'link_id', '') END AS link_id,
			left(regexp_replace(CASE WHEN tombstone.thought_id IS NULL THEN thought.body ELSE tombstone.snapshot->>'body' END, '\s+', ' ', 'g'), 240) AS snippet,
			count(*) OVER () AS total_count,
			CASE WHEN tombstone.thought_id IS NULL THEN thought.updated_at ELSE (tombstone.snapshot->>'updated_at')::timestamptz END AS updated_at,
			CASE WHEN tombstone.thought_id IS NULL THEN 'active' ELSE 'tombstone' END AS lifecycle_status,
			tombstone.reason AS lifecycle_reason,
			CASE WHEN tombstone.thought_id IS NULL THEN '' ELSE '?tool=history&thought_view=history&thought_id=' || thought.id END AS history_deep_link,
			authority.snapshot_sequence,
			authority.snapshot_at
		FROM reader_thoughts thought
		JOIN search_candidates candidate ON candidate.thought_id=thought.id
		CROSS JOIN search_authority authority
		LEFT JOIN reader_thought_tombstones tombstone ON tombstone.thought_id=thought.id AND (
			CASE WHEN (tombstone.snapshot->>'lifecycle_sequence') ~ '^[0-9]+$'
				THEN (tombstone.snapshot->>'lifecycle_sequence')::bigint <= authority.snapshot_sequence
				ELSE tombstone.created_at <= authority.snapshot_at
			END
		)
		WHERE thought.created_at <= authority.snapshot_at
		-- 排序键也必须落在快照里。分页按 updated_at DESC 做 keyset，而活跃 thought
		-- 的 updated_at 会被编辑顶高（每次写入都 NOW()）；只钉 created_at 的话，
		-- 翻页途中被编辑的那条 updated_at 会大于游标而被 keyset 条件排除，
		-- 结果它在任何一页都不出现，total 却仍然把它算在内。
		AND (CASE WHEN tombstone.thought_id IS NULL THEN thought.updated_at ELSE (tombstone.snapshot->>'updated_at')::timestamptz END) <= authority.snapshot_at
		AND thought.deleted=false AND tombstone.reason IS DISTINCT FROM 'user_deleted' AND (
			tombstone.thought_id IS NULL OR (
				jsonb_typeof(tombstone.snapshot)='object'
				AND tombstone.snapshot ?& ARRAY['snapshot_version','id','host_kind','host_id','link_id','type','body','target','quote','source','created_at','updated_at','original_host_snapshot','original_host_identity','frozen_at']
				AND jsonb_typeof(tombstone.snapshot->'snapshot_version')='number'
				AND tombstone.snapshot->>'snapshot_version'='1'
				AND tombstone.snapshot->>'id'=thought.id
				AND NULLIF(btrim(tombstone.snapshot->>'host_kind'),'') IS NOT NULL
				AND NULLIF(btrim(tombstone.snapshot->>'host_id'),'') IS NOT NULL
				AND tombstone.snapshot->>'type'='thought'
				AND jsonb_typeof(tombstone.snapshot->'link_id') IN ('string','null')
				AND jsonb_typeof(tombstone.snapshot->'body')='string'
				AND jsonb_typeof(tombstone.snapshot->'target')='object'
				AND jsonb_typeof(tombstone.snapshot->'quote') IN ('object','null')
				AND jsonb_typeof(tombstone.snapshot->'source')='string'
				AND jsonb_typeof(tombstone.snapshot->'created_at')='string'
				AND jsonb_typeof(tombstone.snapshot->'updated_at')='string'
				AND jsonb_typeof(tombstone.snapshot->'original_host_identity')='object'
				AND jsonb_typeof(tombstone.snapshot->'frozen_at')='string'
			)
		) AND (
			(tombstone.thought_id IS NULL AND (
				thought.body ILIKE $1 OR thought.source ILIKE $1 OR thought.quote::text ILIKE $1
			)) OR (tombstone.thought_id IS NOT NULL AND (
				(tombstone.snapshot->>'body') ILIKE $1
				OR (tombstone.snapshot->>'source') ILIKE $1
				OR ((tombstone.snapshot->'quote')::text) ILIKE $1
			)))
	)
	SELECT thought_id, host_kind, host_id, link_id, snippet, total_count, updated_at,
		lifecycle_status, lifecycle_reason, history_deep_link, snapshot_sequence, snapshot_at
	FROM matching_thoughts`
	var snapshotSequenceArg any
	var snapshotAtArg any
	if !snapshotAt.IsZero() {
		snapshotSequenceArg = snapshotSequence
		snapshotAtArg = snapshotAt
	}
	args := []any{"%" + query + "%", snapshotSequenceArg, snapshotAtArg}
	if !at.IsZero() {
		sql += ` WHERE (updated_at < $4 OR (updated_at = $4 AND thought_id < $5))`
		args = append(args, at, id)
	}
	sql += fmt.Sprintf(` ORDER BY updated_at DESC, thought_id DESC LIMIT $%d`, len(args)+1)
	// One extra row distinguishes a full final page from a page with another
	// cursor, while the public response still respects the requested limit.
	args = append(args, limit+1)
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, "", fmt.Errorf("search thoughts: %w", err)
	}
	defer rows.Close()
	return scanThoughtSearchRows(rows, limit, query, cursorTotal)
}

func parseThoughtSyncCursor(raw string) (int64, string, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, "", fmt.Errorf("%w: invalid thought sync cursor", ErrInvalidReaderCursor)
	}
	parts := strings.SplitN(string(decoded), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return 0, "", fmt.Errorf("%w: invalid thought sync cursor", ErrInvalidReaderCursor)
	}
	sequence, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || sequence < 0 {
		return 0, "", fmt.Errorf("%w: invalid thought sync cursor", ErrInvalidReaderCursor)
	}
	return sequence, parts[1], nil
}

func thoughtSyncCursor(sequence int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(sequence, 10) + "|" + id))
}

// ListThoughtsSince is the installation-level replay surface for local-first
// clients. It includes tombstones and orders by the server-owned operation
// sequence, so a client never mistakes its IndexedDB auto-increment sequence
// for the authoritative cross-device ordering.
func (r *PGXReaderVNextRepository) ListThoughtsSince(ctx context.Context, after string, limit int) ([]model.ReaderThought, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	sequence, id, err := parseThoughtSyncCursor(after)
	if err != nil {
		return nil, "", err
	}
	sql := `SELECT ` + readerThoughtColumns + `,tt.snapshot,tt.reason,tt.created_at
		FROM reader_thoughts
		LEFT JOIN reader_thought_tombstones tt
			ON tt.thought_id=reader_thoughts.id
		WHERE true`
	args := []any{}
	if sequence > 0 {
		sql += fmt.Sprintf(` AND (last_sequence > $%d OR (last_sequence = $%d AND id > $%d))`, len(args)+1, len(args)+1, len(args)+2)
		args = append(args, sequence, id)
	}
	if sequence == 0 && id != "" {
		return nil, "", fmt.Errorf("%w: invalid thought sync cursor", ErrInvalidReaderCursor)
	}
	limitPosition := len(args) + 1
	sql += fmt.Sprintf(` ORDER BY last_sequence ASC, id ASC LIMIT $%d`, limitPosition)
	args = append(args, limit)
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list thoughts since: %w", err)
	}
	defer rows.Close()
	items := make([]model.ReaderThought, 0)
	for rows.Next() {
		item, err := scanReaderThoughtSyncSnapshot(rows)
		if err != nil {
			return nil, "", fmt.Errorf("decode thought sync snapshot: %w", err)
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) > 0 {
		last := items[len(items)-1]
		return items, thoughtSyncCursor(last.LastSequence, last.ID), nil
	}
	return items, "", nil
}

// ListThoughtConflicts keeps the compatibility endpoint name, but pages the
// independent immutable supersession event sequence.  It must not derive rows
// from the mutable materialized winner: doing so loses late losers and rewrites
// the winner that was observed when an earlier version lost.
func (r *PGXReaderVNextRepository) ListThoughtConflicts(ctx context.Context, after string, limit int) ([]model.ReaderThoughtConflict, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	sequence, marker, err := parseThoughtSyncCursor(after)
	if err != nil {
		return nil, "", err
	}
	if marker != "" && marker != "event" {
		return nil, "", fmt.Errorf("%w: invalid thought conflict cursor", ErrInvalidReaderCursor)
	}
	sql := `SELECT sequence,annotation_id,loser,winner_at_detection FROM reader_thought_supersession_events WHERE true`
	args := []any{}
	if sequence > 0 {
		sql += ` AND sequence > $1`
		args = append(args, sequence)
	}
	limitPosition := len(args) + 1
	sql += fmt.Sprintf(` ORDER BY sequence ASC LIMIT $%d`, limitPosition)
	args = append(args, limit)
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list thought conflicts: %w", err)
	}
	defer rows.Close()
	items := make([]model.ReaderThoughtConflict, 0)
	for rows.Next() {
		var item model.ReaderThoughtConflict
		var loser, winner []byte
		if err := rows.Scan(&item.Sequence, &item.AnnotationID, &loser, &winner); err != nil {
			return nil, "", fmt.Errorf("scan thought conflict: %w", err)
		}
		if err := json.Unmarshal(loser, &item.Loser); err != nil {
			return nil, "", fmt.Errorf("decode thought conflict loser: %w", err)
		}
		if err := json.Unmarshal(winner, &item.Winner); err != nil {
			return nil, "", fmt.Errorf("decode thought conflict winner: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("read thought conflicts: %w", err)
	}
	if len(items) == 0 {
		return items, "", nil
	}
	last := items[len(items)-1]
	return items, thoughtSyncCursor(last.Sequence, "event"), nil
}

func (r *PGXReaderVNextRepository) ListThoughtHistory(ctx context.Context, after string, limit int) ([]model.ReaderThought, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	at, id, err := parseReaderCursor(after)
	if err != nil {
		return nil, "", err
	}
	args := []any{}
	sql := `SELECT ` + readerThoughtColumns + `,tt.snapshot,tt.reason,tt.created_at
		FROM reader_thought_tombstones tt
		JOIN reader_thoughts ON reader_thoughts.id=tt.thought_id
		WHERE reader_thoughts.deleted=false AND tt.reason <> 'user_deleted'`
	if !at.IsZero() {
		sql += fmt.Sprintf(` AND (tt.created_at < $%d OR (tt.created_at = $%d AND tt.thought_id < $%d))`, len(args)+1, len(args)+1, len(args)+2)
		args = append(args, at, id)
	}
	sql += fmt.Sprintf(` ORDER BY tt.created_at DESC,tt.thought_id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list thought history: %w", err)
	}
	defer rows.Close()
	items := make([]model.ReaderThought, 0)
	for rows.Next() {
		item, err := scanReaderThoughtHistorySnapshot(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan thought history: %w", err)
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) == limit {
		last := items[len(items)-1]
		at := last.TombstonedAt
		if at == nil {
			return items, "", nil
		}
		return items, readerCursor(*at, last.ID), nil
	}
	return items, "", nil
}

func (r *PGXReaderVNextRepository) MarkThoughtHostTombstones(ctx context.Context, hostKind, hostID, reason string) error {
	if strings.TrimSpace(hostKind) == "" || strings.TrimSpace(hostID) == "" {
		return ErrReaderTodoHostMissing
	}
	return r.withTx(ctx, func(db database.Querier) error {
		return r.markThoughtHostTombstonesOn(ctx, db, hostKind, hostID, reason)
	})
}

func (r *PGXReaderVNextRepository) markThoughtHostTombstonesOn(ctx context.Context, db database.Querier, hostKind, hostID, reason string) error {
	rows, err := db.Query(ctx, `
		SELECT id
		FROM reader_thoughts
		WHERE host_kind=$1 AND host_id=$2 AND deleted=false
		ORDER BY id
		FOR UPDATE`, hostKind, hostID)
	if err != nil {
		return fmt.Errorf("list thought tombstones: %w", err)
	}
	thoughtIDs := make([]string, 0)
	for rows.Next() {
		var thoughtID string
		if err := rows.Scan(&thoughtID); err != nil {
			rows.Close()
			return fmt.Errorf("scan thought tombstone: %w", err)
		}
		thoughtIDs = append(thoughtIDs, thoughtID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read thought tombstones: %w", err)
	}
	for _, thoughtID := range thoughtIDs {
		if err := r.markThoughtTombstoneOn(ctx, db, thoughtID, reason); err != nil {
			return err
		}
	}
	return nil
}

func (r *PGXReaderVNextRepository) markThoughtTombstoneOn(ctx context.Context, db database.Querier, thoughtID, reason string) error {
	item, err := scanReaderThought(db.QueryRow(ctx, `SELECT `+readerThoughtColumns+`
		FROM reader_thoughts
		WHERE id=$1 AND deleted=false
		FOR UPDATE`, thoughtID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read thought for tombstone: %w", err)
	}
	var currentReason string
	err = db.QueryRow(ctx, `SELECT reason FROM reader_thought_tombstones WHERE thought_id=$1`, thoughtID).Scan(&currentReason)
	if err == nil && currentReason == reason {
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read thought tombstone: %w", err)
	}
	lifecycleSequence, err := r.appendThoughtLifecycleOp(ctx, db, item, "tombstone", reason)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `
		INSERT INTO reader_thought_tombstones (thought_id,host_kind,host_id,reason,snapshot)
		SELECT id,host_kind,host_id,$2,
			jsonb_build_object(
				'snapshot_version',1,
				'id',id,'host_kind',host_kind,'host_id',host_id,'link_id',link_id,
				'type','thought','body',body,'target',target,'quote',quote,'source',source,
				'created_at',created_at,'updated_at',updated_at,
				'original_host_snapshot',CASE host_kind
					WHEN 'link' THEN (SELECT to_jsonb(COALESCE(link.content_document,link.content,'')) FROM links link WHERE link.id=reader_thoughts.link_id)
					WHEN 'note' THEN (SELECT to_jsonb(note.published_content) FROM reader_notes note WHERE note.id=reader_thoughts.host_id::uuid)
					WHEN 'inbox' THEN (SELECT to_jsonb(inbox.body) FROM reader_inbox inbox WHERE inbox.id=reader_thoughts.host_id::uuid)
					ELSE NULL
				END,
				'original_host_identity',jsonb_build_object('kind',host_kind,'id',host_id,'link_id',link_id),
				'frozen_at',CURRENT_TIMESTAMP,
				'lifecycle_sequence',$3::bigint)
		FROM reader_thoughts
		WHERE id=$1 AND deleted=false
		ON CONFLICT (thought_id) DO NOTHING`,
		thoughtID, reason, lifecycleSequence)
	if err != nil {
		return fmt.Errorf("mark thought tombstone: %w", err)
	}
	// The lifecycle op above materializes the Thought, which refreshes the
	// projection while the Thought is still a live source. The tombstone is
	// what actually retires it, so the projection is refreshed again here.
	return r.replaceThoughtTodoProjectionsOn(ctx, db, thoughtID)
}

func readerThoughtLifecycleOpID(action, reason string, item *model.ReaderThought) string {
	seed := fmt.Sprintf("reader-thought-lifecycle:%s:%s:%s:%d", action, reason, item.ID, item.LastSequence)
	return "lifecycle-" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(seed)).String()
}

func (r *PGXReaderVNextRepository) appendThoughtLifecycleOp(ctx context.Context, db database.Querier, item *model.ReaderThought, action, reason string) (int64, error) {
	linkID := ""
	if item.LinkID != nil {
		linkID = item.LinkID.String()
	}
	payload, err := json.Marshal(struct {
		Body   string          `json:"body"`
		Quote  json.RawMessage `json:"quote,omitempty"`
		Source string          `json:"source"`
		LinkID string          `json:"link_id,omitempty"`
	}{Body: item.Body, Quote: item.Quote, Source: item.Source, LinkID: linkID})
	if err != nil {
		return 0, fmt.Errorf("encode thought lifecycle operation: %w", err)
	}
	thoughtOp, sequence, duplicate, err := r.appendDerivedThoughtOp(ctx, db, model.ReaderThoughtOp{
		OpID:          readerThoughtLifecycleOpID(action, reason, item),
		DeviceID:      "reader-lifecycle",
		OperationKind: "update",
		AnnotationID:  item.ID,
		HostKind:      item.HostKind,
		HostID:        item.HostID,
		Target:        item.Target,
		Payload:       payload,
	})
	if err != nil {
		return 0, fmt.Errorf("append thought lifecycle operation: %w", err)
	}
	if !duplicate {
		if err := r.materializeThought(ctx, db, thoughtOp, sequence); err != nil {
			return 0, err
		}
	}
	return sequence, nil
}

// readerReattachHost resolves and locks the target before the Thought row and
// either caller CAS are checked. Its body is read only after both CAS values
// match, immediately before quote reanchoring.
func readerReattachHost(ctx context.Context, db database.Querier, command model.ReaderThoughtReattachCommand) (int64, error) {
	hostRevision, err := readerReattachHostRevision(ctx, db, command.TargetHostKind, command.TargetHostID)
	if err != nil {
		return 0, err
	}
	return hostRevision, nil
}

// readerReattachThoughtPayload rebuilds the target and op payload for the new
// host. The quote is re-anchored against the destination body rather than
// carried over verbatim, so a reattached thought always points at text that
// actually exists on the host it now belongs to.
func readerReattachThoughtPayload(item *model.ReaderThought, command model.ReaderThoughtReattachCommand, hostRevision int64, hostBody string) (json.RawMessage, json.RawMessage, error) {
	return readerReattachThoughtPayloadWithMarker(item, command, hostRevision, hostBody, nil)
}

func readerReattachThoughtPayloadWithMarker(item *model.ReaderThought, command model.ReaderThoughtReattachCommand, hostRevision int64, hostBody string, marker *model.ReaderThoughtReattachOperation) (json.RawMessage, json.RawMessage, error) {
	linkID := ""
	if command.TargetHostKind == "link" {
		linkID = command.TargetHostID
	}
	target, err := rewriteReaderThoughtTargetHost(item.Target, command.TargetHostID, command.TargetHostKind, hostRevision)
	if err != nil {
		return nil, nil, err
	}
	quote, _, err := readerReanchorQuoteForContent(hostBody, item.Quote)
	if err != nil {
		return nil, nil, err
	}
	payload, err := json.Marshal(struct {
		Body     string                       `json:"body"`
		Quote    json.RawMessage              `json:"quote,omitempty"`
		Source   string                       `json:"source"`
		LinkID   string                       `json:"link_id,omitempty"`
		Reattach *readerThoughtReattachMarker `json:"reattach,omitempty"`
	}{
		Body: item.Body, Quote: quote, Source: item.Source, LinkID: linkID,
		Reattach: func() *readerThoughtReattachMarker {
			if marker == nil {
				return nil
			}
			return &readerThoughtReattachMarker{ExpectedLastSequence: marker.ExpectedLastSequence, ExpectedHostRevision: marker.ExpectedHostRevision}
		}(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("encode thought reattach: %w", err)
	}
	return target, payload, nil
}

func rewriteReaderThoughtTargetHost(raw json.RawMessage, hostID, hostKind string, hostRevision int64) (json.RawMessage, error) {
	var target map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &target) != nil || target == nil {
		return nil, ErrInvalidReaderThought
	}
	encodedHostID, err := json.Marshal(hostID)
	if err != nil {
		return nil, fmt.Errorf("encode thought target host: %w", err)
	}
	target["host_id"] = encodedHostID
	if hostRevision <= 0 {
		return nil, ErrRevisionConflict
	}
	var version map[string]json.RawMessage
	if rawVersion, ok := target["version"]; ok && len(rawVersion) > 0 {
		if err := json.Unmarshal(rawVersion, &version); err != nil || version == nil {
			return nil, ErrInvalidReaderThought
		}
	} else {
		version = make(map[string]json.RawMessage)
	}
	encodedRevision, err := json.Marshal(hostRevision)
	if err != nil {
		return nil, fmt.Errorf("encode thought target revision: %w", err)
	}
	switch hostKind {
	case "link":
		version["content_revision"] = encodedRevision
	case "note":
		version["note_revision"] = encodedRevision
	case "inbox":
		version["metadata_revision"] = encodedRevision
	default:
		return nil, ErrInvalidReaderThought
	}
	target["version"], err = json.Marshal(version)
	if err != nil {
		return nil, fmt.Errorf("encode thought target version: %w", err)
	}
	return json.Marshal(target)
}

func readerReattachHostBody(ctx context.Context, db database.Querier, hostKind, hostID string) (string, error) {
	var body string
	var err error
	switch hostKind {
	case "link":
		id, parseErr := uuid.Parse(hostID)
		if parseErr != nil {
			return "", ErrNotFound
		}
		err = db.QueryRow(ctx, `SELECT COALESCE(content_document,content,'') FROM links WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&body)
	case "note":
		id, parseErr := uuid.Parse(hostID)
		if parseErr != nil {
			return "", ErrNotFound
		}
		err = db.QueryRow(ctx, `SELECT published_content FROM reader_notes WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&body)
	case "inbox":
		id, parseErr := uuid.Parse(hostID)
		if parseErr != nil {
			return "", ErrNotFound
		}
		err = db.QueryRow(ctx, `SELECT body FROM reader_inbox WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&body)
	default:
		return "", ErrNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read thought reattach host body: %w", err)
	}
	return body, nil
}

func readerReattachHostRevision(ctx context.Context, db database.Querier, hostKind, hostID string) (int64, error) {
	var revision int64
	kind := model.ReaderHostKind(hostKind)
	id, err := uuid.Parse(hostID)
	if err != nil {
		return 0, ErrNotFound
	}
	if err := lockReaderThoughtHost(ctx, db, kind, id, readerThoughtHostUpdate); err != nil {
		if errors.Is(err, errReaderThoughtHostUnavailable) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	var query string
	switch hostKind {
	case "link":
		query = `SELECT content_revision FROM links WHERE id=$1 AND deleted_at IS NULL`
	case "note":
		query = `SELECT published_revision FROM reader_notes WHERE id=$1 AND deleted_at IS NULL`
	case "inbox":
		query = `SELECT metadata_revision FROM reader_inbox WHERE id=$1 AND deleted_at IS NULL`
	default:
		return 0, ErrNotFound
	}
	err = db.QueryRow(ctx, query, id).Scan(&revision)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("read thought reattach host revision: %w", err)
	}
	return revision, nil
}

func (r *PGXReaderVNextRepository) GetThought(ctx context.Context, id string) (*model.ReaderThought, error) {
	item, err := scanReaderThought(r.db.QueryRow(ctx, `SELECT `+readerThoughtColumns+` FROM reader_thoughts WHERE id=$1 AND deleted=false`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get thought: %w", err)
	}
	var reason string
	var tombstonedAt time.Time
	var snapshot []byte
	if err := r.db.QueryRow(ctx, `SELECT snapshot,reason,created_at FROM reader_thought_tombstones WHERE thought_id=$1`, id).Scan(&snapshot, &reason, &tombstonedAt); err == nil {
		if reason == "user_deleted" {
			return nil, ErrNotFound
		}
		if err := applyReaderThoughtReplaySnapshot(item, snapshot); err != nil {
			return nil, fmt.Errorf("get thought lifecycle snapshot: %w", err)
		}
		item.LifecycleStatus = "tombstone"
		item.LifecycleReason = &reason
		item.TombstonedAt = &tombstonedAt
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("get thought lifecycle: %w", err)
	}
	return item, nil
}
