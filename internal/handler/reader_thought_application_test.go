package handler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/problem"
	"webtag/internal/service"
)

type readerThoughtApplicationStore struct {
	service.ReaderThoughtStore
	ops       []model.ReaderThoughtOp
	acks      []model.ReaderThoughtAck
	thoughts  []model.ReaderThought
	conflicts []model.ReaderThoughtConflict
}

func (s *readerThoughtApplicationStore) AppendThoughtOps(_ context.Context, ops []model.ReaderThoughtOp) ([]model.ReaderThoughtAck, error) {
	s.ops = append([]model.ReaderThoughtOp(nil), ops...)
	return s.acks, nil
}

func (s *readerThoughtApplicationStore) ListThoughts(context.Context, string, string, int) ([]model.ReaderThought, string, error) {
	return s.thoughts, "thought-next", nil
}

func (s *readerThoughtApplicationStore) ListThoughtConflicts(context.Context, string, int) ([]model.ReaderThoughtConflict, string, error) {
	return s.conflicts, "conflict-next", nil
}

func TestReaderThoughtRoutesMapCommandAndAck(t *testing.T) {
	t.Parallel()

	store := &readerThoughtApplicationStore{acks: []model.ReaderThoughtAck{{
		OpID: "op-1", Sequence: 7, Disposition: "applied",
		SubmittedKey: model.ReaderThoughtVersionKey{LogicalClock: 3, DeviceID: "device-1", OpID: "op-1"},
		WinnerKey:    model.ReaderThoughtVersionKey{LogicalClock: 4, DeviceID: "device-2", OpID: "op-2"},
	}}}
	applications := readerTestApplications(store)
	routes := NewReaderThoughtRoutes(applications.Thoughts)
	response, err := routes.PushThoughtOps(context.Background(), dto.ReaderThoughtOpsRequest{Ops: []dto.ReaderThoughtOpRequest{{
		ContractVersion: model.ReaderThoughtContractVersion,
		OpID:            "op-1", DeviceID: "device-1", LogicalClock: 3,
		OperationKind: "add", AnnotationID: "thought-1", HostKind: "link", HostID: "link-1",
		Target:  json.RawMessage(`{"kind":"saved-content","host_id":"link-1","version":{"content_revision":1}}`),
		Payload: json.RawMessage(`{"body":"body","quote":{"exact":"quote"}}`),
	}}})
	if err != nil {
		t.Fatalf("PushThoughtOps() error = %v", err)
	}
	if len(store.ops) != 1 || store.ops[0].OpID != "op-1" || store.ops[0].CreatedAt.IsZero() {
		t.Fatalf("stored ops = %#v", store.ops)
	}
	if string(store.ops[0].Target) != `{"kind":"saved-content","host_id":"link-1","version":{"content_revision":1}}` ||
		string(store.ops[0].Payload) != `{"body":"body","quote":{"exact":"quote"}}` {
		t.Fatalf("stored wire payload = target %s payload %s", store.ops[0].Target, store.ops[0].Payload)
	}
	if len(response) != 1 || response[0].ContractVersion != model.ReaderThoughtContractVersion ||
		response[0].CurrentWinnerKey.OpID != "op-2" || response[0].SubmittedKey.OpID != "op-1" {
		t.Fatalf("PushThoughtOps() = %#v", response)
	}
}

func TestReaderThoughtRoutesRejectInvalidWireShapeBeforeStore(t *testing.T) {
	t.Parallel()

	valid := dto.ReaderThoughtOpRequest{
		ContractVersion: model.ReaderThoughtContractVersion,
		OpID:            "op-1", DeviceID: "device-1", LogicalClock: 3,
		OperationKind: "add", AnnotationID: "thought-1", HostKind: "link", HostID: "link-1",
		Target:  json.RawMessage(`{"kind":"saved-content","host_id":"link-1","version":{"content_revision":1}}`),
		Payload: json.RawMessage(`{"body":"body","quote":{"exact":"quote"}}`),
	}
	cases := []struct {
		name string
		edit func(*dto.ReaderThoughtOpRequest)
		code string
	}{
		{
			name: "target is malformed JSON",
			edit: func(op *dto.ReaderThoughtOpRequest) { op.Target = json.RawMessage(`{"kind":`) },
			code: "invalid_thought_payload",
		},
		{
			name: "target is not object",
			edit: func(op *dto.ReaderThoughtOpRequest) { op.Target = json.RawMessage(`[]`) },
			code: "invalid_thought_target",
		},
		{
			name: "payload has malformed quote",
			edit: func(op *dto.ReaderThoughtOpRequest) { op.Payload = json.RawMessage(`{"quote":}`) },
			code: "invalid_thought_payload",
		},
		{
			name: "reattach contains client body",
			edit: func(op *dto.ReaderThoughtOpRequest) {
				op.OperationKind = "update"
				op.Payload = json.RawMessage(`{"body":"must not be sent","reattach":{"expected_last_sequence":11,"expected_host_revision":1}}`)
			},
			code: "invalid_thought_payload",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := &readerThoughtApplicationStore{}
			applications := readerTestApplications(store)
			op := valid
			tc.edit(&op)
			_, err := NewReaderThoughtRoutes(applications.Thoughts).PushThoughtOps(
				context.Background(),
				dto.ReaderThoughtOpsRequest{Ops: []dto.ReaderThoughtOpRequest{op}},
			)
			var status *problem.Error
			if !errors.As(err, &status) || status.Code() != tc.code {
				t.Fatalf("PushThoughtOps() error = %v, want %s", err, tc.code)
			}
			if len(store.ops) != 0 {
				t.Fatalf("stored ops = %#v, want invalid wire rejected before store", store.ops)
			}
		})
	}
}

func TestReaderThoughtRoutesMapReattachCommand(t *testing.T) {
	t.Parallel()

	store := &readerThoughtApplicationStore{}
	applications := readerTestApplications(store)
	_, err := NewReaderThoughtRoutes(applications.Thoughts).PushThoughtOps(context.Background(), dto.ReaderThoughtOpsRequest{Ops: []dto.ReaderThoughtOpRequest{{
		ContractVersion: model.ReaderThoughtContractVersion,
		OpID:            "op-1", DeviceID: "device-1", LogicalClock: 3,
		OperationKind: "update", AnnotationID: "thought-1", HostKind: "link", HostID: "link-1",
		Target:  json.RawMessage(`{"kind":"saved-content","host_id":"link-1","version":{"content_revision":1}}`),
		Payload: json.RawMessage(`{"reattach":{"expected_last_sequence":7,"expected_host_revision":1}}`),
	}}})
	if err != nil {
		t.Fatalf("PushThoughtOps() error = %v", err)
	}
	if len(store.ops) != 1 || store.ops[0].Reattach == nil ||
		store.ops[0].Reattach.ExpectedLastSequence != 7 || store.ops[0].Reattach.ExpectedHostRevision != 1 {
		t.Fatalf("stored reattach op = %#v", store.ops)
	}
}

func TestReaderThoughtRoutesMapPagesAndFrozenSnapshot(t *testing.T) {
	t.Parallel()

	when := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	store := &readerThoughtApplicationStore{thoughts: []model.ReaderThought{{
		ID: "thought-frozen", HostKind: "link", HostID: "link-frozen",
		Target: json.RawMessage(`{"kind":"saved-content","host_id":"link-frozen","version":{"content_revision":3}}`),
		Body:   "frozen body", Source: "user", CreatedAt: when, UpdatedAt: when,
		WinnerKey:            model.ReaderThoughtVersionKey{LogicalClock: 5, DeviceID: "device", OpID: "op"},
		OriginalHostSnapshot: json.RawMessage(`{"blocks":["frozen original"]}`),
	}}}
	applications := readerTestApplications(store)
	response, err := NewReaderThoughtRoutes(applications.Thoughts).ListThoughts(context.Background(), "", "", 30)
	if err != nil {
		t.Fatalf("ListThoughts() error = %v", err)
	}
	if response.ContractVersion != model.ReaderThoughtContractVersion || response.NextCursor != "thought-next" || len(response.Items) != 1 {
		t.Fatalf("ListThoughts() = %#v", response)
	}
	if response.Items[0].LifecycleStatus != "active" || string(response.Items[0].OriginalHostSnapshot) != `{"blocks":["frozen original"]}` {
		t.Fatalf("mapped thought = %#v", response.Items[0])
	}
}

func TestReaderThoughtRoutesMapConflictOperationEnvelope(t *testing.T) {
	t.Parallel()

	recovery := model.ReaderThoughtVersionKey{LogicalClock: 2, DeviceID: "loser", OpID: "op-loser"}
	winner := model.ReaderThoughtVersionKey{LogicalClock: 3, DeviceID: "winner", OpID: "op-winner"}
	operation := model.ReaderThoughtConflictOperation{
		Sequence: 9, OpID: "op-recovery", DeviceID: "device", LogicalClock: 4,
		OperationKind: "update", AnnotationID: "thought-1", HostKind: "link", HostID: "link-1",
		Target: json.RawMessage(`{}`), Payload: json.RawMessage(`{}`),
		RecoveryOf: &recovery, ExpectedWinnerKey: &winner,
	}
	store := &readerThoughtApplicationStore{conflicts: []model.ReaderThoughtConflict{{
		Sequence: 10, AnnotationID: "thought-1", Winner: operation, Loser: operation,
	}}}
	applications := readerTestApplications(store)
	response, err := NewReaderThoughtRoutes(applications.Thoughts).ListThoughtConflicts(context.Background(), "", 30)
	if err != nil {
		t.Fatalf("ListThoughtConflicts() error = %v", err)
	}
	if response.ContractVersion != model.ReaderThoughtContractVersion || response.NextCursor != "conflict-next" || len(response.Items) != 1 {
		t.Fatalf("ListThoughtConflicts() = %#v", response)
	}
	mapped := response.Items[0].WinnerAtDetection
	if mapped.ContractVersion != model.ReaderThoughtContractVersion || mapped.RecoveryOf == nil || mapped.RecoveryOf.OpID != recovery.OpID ||
		mapped.ExpectedCurrentWinnerKey == nil || mapped.ExpectedCurrentWinnerKey.OpID != winner.OpID {
		t.Fatalf("mapped conflict operation = %#v", mapped)
	}
}
