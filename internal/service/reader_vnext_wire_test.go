package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
)

func validThoughtWireInput(operationKind string) dto.ReaderThoughtOpRequest {
	return dto.ReaderThoughtOpRequest{
		ContractVersion: model.ReaderThoughtContractVersion,
		OpID:            "op-1",
		DeviceID:        "device-1",
		LogicalClock:    1,
		OperationKind:   operationKind,
		AnnotationID:    "annotation-1",
		HostKind:        "link",
		HostID:          "link-1",
		Target: json.RawMessage(`{
            "kind": "saved-content",
            "host_id": "link-1",
            "version": {"content_revision": 3}
        }`),
		Payload: json.RawMessage(`{"quote":{"exact":"selected text"}}`),
	}
}

func TestValidateThoughtWireAcceptsSupportedTargets(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		target string
	}{
		{
			name:   "saved content",
			target: `{"kind":"saved-content","host_id":"link-1","version":{"content_revision":3}}`,
		},
		{
			name:   "summary",
			target: `{"kind":"summary","host_id":"link-1","version":{"source_hash":"hash-1"}}`,
		},
		{
			name:   "note",
			target: `{"kind":"note","host_id":"link-1","version":{"note_revision":4}}`,
		},
		{
			name:   "inbox",
			target: `{"kind":"inbox","host_id":"link-1","version":{"metadata_revision":5}}`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			input := validThoughtWireInput("add")
			input.Target = json.RawMessage(tc.target)
			if err := validateThoughtWire(input); err != nil {
				t.Fatalf("validateThoughtWire() error = %v", err)
			}
		})
	}
}

func TestThoughtResponseCarriesFrozenOriginalHostSnapshot(t *testing.T) {
	t.Parallel()

	response := thoughtResponse(model.ReaderThought{
		ID:                   "thought-frozen",
		HostKind:             "link",
		HostID:               "link-frozen",
		Target:               json.RawMessage(`{"kind":"saved-content","host_id":"link-frozen","version":{"content_revision":3}}`),
		Quote:                json.RawMessage(`{"exact":"frozen quote"}`),
		Body:                 "frozen body",
		Source:               "user",
		LifecycleStatus:      "tombstone",
		OriginalHostSnapshot: json.RawMessage(`{"blocks":["frozen original"]}`),
	})

	if string(response.OriginalHostSnapshot) != `{"blocks":["frozen original"]}` {
		t.Fatalf("original_host_snapshot = %s, want frozen snapshot", response.OriginalHostSnapshot)
	}
}

func TestValidateThoughtWireRejectsInvalidTarget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		target string
	}{
		{
			name:   "target is not an object",
			target: `[]`,
		},
		{
			name:   "missing host id",
			target: `{"kind":"saved-content","version":{"content_revision":3}}`,
		},
		{
			name:   "host id does not match operation",
			target: `{"kind":"saved-content","host_id":"other-link","version":{"content_revision":3}}`,
		},
		{
			name:   "saved content missing revision",
			target: `{"kind":"saved-content","host_id":"link-1","version":{}}`,
		},
		{
			name:   "summary missing source hash",
			target: `{"kind":"summary","host_id":"link-1","version":{}}`,
		},
		{
			name:   "note missing revision",
			target: `{"kind":"note","host_id":"link-1","version":{}}`,
		},
		{
			name:   "inbox missing metadata revision",
			target: `{"kind":"inbox","host_id":"link-1","version":{}}`,
		},
		{
			name:   "retired legacy stale target",
			target: `{"kind":"legacy-stale","host_id":"link-1","version":{"source_key":"legacy-1"}}`,
		},
		{
			name:   "unknown kind",
			target: `{"kind":"future","host_id":"link-1","version":{}}`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			input := validThoughtWireInput("add")
			input.Target = json.RawMessage(tc.target)
			if err := validateThoughtWire(input); err == nil {
				t.Fatal("validateThoughtWire() error = nil, want validation error")
			}
		})
	}
}

func TestValidateThoughtWireDeleteMayOmitQuote(t *testing.T) {
	t.Parallel()

	input := validThoughtWireInput("delete")
	input.Payload = json.RawMessage(`{"body":""}`)
	if err := validateThoughtWire(input); err != nil {
		t.Fatalf("delete without quote should be accepted: %v", err)
	}

	input.Payload = json.RawMessage(`{"quote":}`)
	if err := validateThoughtWire(input); err == nil {
		t.Fatal("malformed delete quote should be rejected")
	}
}

func TestValidateThoughtWireAcceptsArchiveThoughtBoundaries(t *testing.T) {
	t.Parallel()

	longAnnotation := validThoughtWireInput("add")
	longAnnotation.AnnotationID = strings.Repeat("a", 129)
	if err := validateThoughtWire(longAnnotation); err != nil {
		t.Fatalf("129-byte annotation_id should be accepted: %v", err)
	}

	deleteWithArchivedHost := validThoughtWireInput("delete")
	deleteWithArchivedHost.HostKind = "inbox"
	deleteWithArchivedHost.HostID = "purged-inbox:legacy-42"
	deleteWithArchivedHost.Target = json.RawMessage(`{
        "kind": "inbox",
        "host_id": "purged-inbox:legacy-42",
        "version": {"metadata_revision": 1}
    }`)
	deleteWithArchivedHost.Payload = json.RawMessage(`{}`)
	if err := validateThoughtWire(deleteWithArchivedHost); err != nil {
		t.Fatalf("delete with non-UUID persisted host should be accepted: %v", err)
	}
}

func TestValidateThoughtWireRequiresQuoteForAddAndUpdate(t *testing.T) {
	t.Parallel()

	for _, operationKind := range []string{"add", "update"} {
		input := validThoughtWireInput(operationKind)
		input.Payload = json.RawMessage(`{"body":"missing quote"}`)
		if err := validateThoughtWire(input); err == nil {
			t.Fatalf("%s without quote should be rejected", operationKind)
		}
	}
}

func TestValidateThoughtWireAcceptsClientOwnedReattachCommand(t *testing.T) {
	t.Parallel()

	input := validThoughtWireInput("update")
	input.ContractVersion = model.ReaderThoughtContractVersion
	input.LogicalClock = 17
	input.Payload = json.RawMessage(`{
        "reattach": {
            "expected_last_sequence": 11,
            "expected_host_revision": 3
        }
    }`)
	if err := validateThoughtWire(input); err != nil {
		t.Fatalf("validateThoughtWire() error = %v", err)
	}
	command, err := readerThoughtReattachOperation(input)
	if err != nil {
		t.Fatalf("readerThoughtReattachOperation() error = %v", err)
	}
	if command == nil || command.ExpectedLastSequence != 11 || command.ExpectedHostRevision != 3 {
		t.Fatalf("readerThoughtReattachOperation() = %#v, want reattach CAS command", command)
	}
}

func TestValidateThoughtWireRejectsMalformedClientReattachCommand(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*dto.ReaderThoughtOpRequest)
	}{
		{
			name: "contains client body",
			mutate: func(input *dto.ReaderThoughtOpRequest) {
				input.Payload = json.RawMessage(`{"body":"must not be sent","reattach":{"expected_last_sequence":11,"expected_host_revision":3}}`)
			},
		},
		{
			name: "wrong target kind",
			mutate: func(input *dto.ReaderThoughtOpRequest) {
				input.Target = json.RawMessage(`{"kind":"summary","host_id":"link-1","version":{"source_hash":"hash"}}`)
			},
		},
		{
			name: "target revision differs from expected revision",
			mutate: func(input *dto.ReaderThoughtOpRequest) {
				input.Target = json.RawMessage(`{"kind":"saved-content","host_id":"link-1","version":{"content_revision":4}}`)
			},
		},
		{
			name: "not an update",
			mutate: func(input *dto.ReaderThoughtOpRequest) {
				input.OperationKind = "add"
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			input := validThoughtWireInput("update")
			input.ContractVersion = model.ReaderThoughtContractVersion
			input.LogicalClock = 17
			input.Payload = json.RawMessage(`{"reattach":{"expected_last_sequence":11,"expected_host_revision":3}}`)
			tc.mutate(&input)
			if err := validateThoughtWire(input); err == nil {
				t.Fatal("validateThoughtWire() error = nil, want malformed reattach rejection")
			}
		})
	}
}

func TestValidateThoughtWireRejectsInvalidOperationEnvelope(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*dto.ReaderThoughtOpRequest)
	}{
		{name: "missing op id", mutate: func(input *dto.ReaderThoughtOpRequest) { input.OpID = " " }},
		{name: "missing device id", mutate: func(input *dto.ReaderThoughtOpRequest) { input.DeviceID = " " }},
		{name: "negative logical clock", mutate: func(input *dto.ReaderThoughtOpRequest) { input.LogicalClock = -1 }},
		{name: "unknown operation", mutate: func(input *dto.ReaderThoughtOpRequest) { input.OperationKind = "replace" }},
		{name: "missing annotation id", mutate: func(input *dto.ReaderThoughtOpRequest) { input.AnnotationID = " " }},
		{name: "missing host kind", mutate: func(input *dto.ReaderThoughtOpRequest) { input.HostKind = " " }},
		{name: "missing host id", mutate: func(input *dto.ReaderThoughtOpRequest) { input.HostID = " " }},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			input := validThoughtWireInput("add")
			tc.mutate(&input)
			if err := validateThoughtWire(input); err == nil {
				t.Fatal("validateThoughtWire() error = nil, want envelope validation error")
			}
		})
	}
}

func TestValidateThoughtWireEnforcesVersionedLamportClock(t *testing.T) {
	t.Parallel()

	for _, operationKind := range []string{"add", "update", "delete"} {
		for _, logicalClock := range []int64{1, 41, model.ReaderThoughtMaxLogicalClock} {
			input := validThoughtWireInput(operationKind)
			input.ContractVersion = model.ReaderThoughtContractVersion
			input.LogicalClock = logicalClock
			if err := validateThoughtWire(input); err != nil {
				t.Fatalf("validateThoughtWire(%s, %d) error = %v", operationKind, logicalClock, err)
			}
		}
	}

	for _, logicalClock := range []int64{0, -1, model.ReaderThoughtMaxLogicalClock + 1} {
		input := validThoughtWireInput("update")
		input.ContractVersion = model.ReaderThoughtContractVersion
		input.LogicalClock = logicalClock
		if err := validateThoughtWire(input); err == nil {
			t.Fatalf("v1 logical_clock %d should be rejected", logicalClock)
		}
	}

	missingVersion := validThoughtWireInput("update")
	missingVersion.ContractVersion = 0
	if err := validateThoughtWire(missingVersion); err == nil {
		t.Fatal("missing contract_version should be rejected")
	}

	unsupported := validThoughtWireInput("update")
	unsupported.ContractVersion = model.ReaderThoughtContractVersion + 1
	unsupported.LogicalClock = 1
	if err := validateThoughtWire(unsupported); err == nil {
		t.Fatal("unsupported contract_version should be rejected")
	}
}

func TestValidateThoughtWireRequiresCompleteValidRecoveryMetadata(t *testing.T) {
	t.Parallel()

	input := validThoughtWireInput("update")
	input.ContractVersion = model.ReaderThoughtContractVersion
	input.LogicalClock = 9
	input.RecoveryOf = &dto.ReaderThoughtVersionKeyResponse{
		LogicalClock: 4,
		DeviceID:     "loser-device",
		OpID:         "loser-op",
	}
	if err := validateThoughtWire(input); err == nil {
		t.Fatal("recovery without expected current winner should be rejected")
	} else {
		var status *httperr.Error
		if !errors.As(err, &status) || status.HTTPStatus() != http.StatusUnprocessableEntity ||
			status.HTTPErrorCode() != "invalid_thought_recovery" {
			t.Fatalf("incomplete recovery error = %v, want 422 invalid_thought_recovery", err)
		}
	}

	input.ExpectedCurrentWinnerKey = &dto.ReaderThoughtVersionKeyResponse{
		LogicalClock: 8,
		DeviceID:     "winner-device",
		OpID:         "winner-op",
	}
	if err := validateThoughtWire(input); err != nil {
		t.Fatalf("complete recovery metadata should be accepted: %v", err)
	}

	input.ExpectedCurrentWinnerKey.OpID = " "
	if err := validateThoughtWire(input); err == nil {
		t.Fatal("non-canonical recovery key should be rejected")
	}
}

func TestValidateThoughtWireRejectsNonCanonicalIdentifiers(t *testing.T) {
	t.Parallel()

	for _, opID := range []string{
		" leading-space",
		"trailing-space ",
		"contains\x00nul",
		string([]byte{0xff}),
		strings.Repeat("a", 129),
	} {
		input := validThoughtWireInput("add")
		input.ContractVersion = model.ReaderThoughtContractVersion
		input.LogicalClock = 1
		input.OpID = opID
		if err := validateThoughtWire(input); err == nil {
			t.Fatalf("non-canonical op_id %q should be rejected", opID)
		}
	}
}

func TestReaderServiceRejectsInvalidDesiredStateCommandsBeforeStore(t *testing.T) {
	t.Parallel()

	service := NewReaderVNextService(nil, nil)
	if _, err := service.PushThoughtOps(context.Background(), dto.ReaderThoughtOpsRequest{}); err == nil {
		t.Fatal("PushThoughtOps() error = nil for empty batch")
	}
	if _, err := service.CreateTodo(context.Background(), dto.ReaderTodoCreateRequest{Text: "  "}); err == nil {
		t.Fatal("CreateTodo() error = nil for blank text")
	}
	if _, err := service.PatchEngagement(context.Background(), "00000000-0000-0000-0000-000000000001", dto.ReaderEngagementRequest{}); err == nil {
		t.Fatal("PatchEngagement() error = nil for empty desired state")
	}
	progress := float32(math.NaN())
	if _, err := service.PatchEngagement(context.Background(), "00000000-0000-0000-0000-000000000001", dto.ReaderEngagementRequest{Progress: &progress}); err == nil {
		t.Fatal("PatchEngagement() error = nil for NaN progress")
	}
}
