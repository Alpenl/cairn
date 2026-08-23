package service

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/problem"
)

func validThoughtCommand(operationKind string) ReaderThoughtOpCommand {
	return ReaderThoughtOpCommand{
		ContractVersion: model.ReaderThoughtContractVersion,
		OpID:            "op-1",
		DeviceID:        "device-1",
		LogicalClock:    1,
		OperationKind:   operationKind,
		AnnotationID:    "annotation-1",
		HostKind:        "link",
		HostID:          "link-1",
		Target: ReaderThoughtTarget{
			Kind:            "saved-content",
			HostID:          "link-1",
			ContentRevision: 3,
		},
		TargetJSON: []byte(`{
            "kind": "saved-content",
            "host_id": "link-1",
            "version": {"content_revision": 3}
        }`),
		Payload:     ReaderThoughtPayload{HasQuote: true},
		PayloadJSON: []byte(`{"quote":{"exact":"selected text"}}`),
	}
}

func TestValidateThoughtCommandAcceptsSupportedTargets(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		target ReaderThoughtTarget
	}{
		{
			name:   "saved content",
			target: ReaderThoughtTarget{Kind: "saved-content", HostID: "link-1", ContentRevision: 3},
		},
		{
			name:   "summary",
			target: ReaderThoughtTarget{Kind: "summary", HostID: "link-1", SourceHash: "hash-1"},
		},
		{
			name:   "note",
			target: ReaderThoughtTarget{Kind: "note", HostID: "link-1", NoteRevision: 4},
		},
		{
			name:   "inbox",
			target: ReaderThoughtTarget{Kind: "inbox", HostID: "link-1", MetadataRevision: 5},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			input := validThoughtCommand("add")
			input.Target = tc.target
			if err := validateThoughtCommand(input); err != nil {
				t.Fatalf("validateThoughtCommand() error = %v", err)
			}
		})
	}
}

func TestValidateThoughtCommandRejectsInvalidTarget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		target ReaderThoughtTarget
	}{
		{
			name:   "missing host id",
			target: ReaderThoughtTarget{Kind: "saved-content", ContentRevision: 3},
		},
		{
			name:   "host id does not match operation",
			target: ReaderThoughtTarget{Kind: "saved-content", HostID: "other-link", ContentRevision: 3},
		},
		{
			name:   "saved content missing revision",
			target: ReaderThoughtTarget{Kind: "saved-content", HostID: "link-1"},
		},
		{
			name:   "summary missing source hash",
			target: ReaderThoughtTarget{Kind: "summary", HostID: "link-1"},
		},
		{
			name:   "note missing revision",
			target: ReaderThoughtTarget{Kind: "note", HostID: "link-1"},
		},
		{
			name:   "inbox missing metadata revision",
			target: ReaderThoughtTarget{Kind: "inbox", HostID: "link-1"},
		},
		{
			name:   "retired legacy stale target",
			target: ReaderThoughtTarget{Kind: "legacy-stale", HostID: "link-1"},
		},
		{
			name:   "unknown kind",
			target: ReaderThoughtTarget{Kind: "future", HostID: "link-1"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			input := validThoughtCommand("add")
			input.Target = tc.target
			if err := validateThoughtCommand(input); err == nil {
				t.Fatal("validateThoughtCommand() error = nil, want validation error")
			}
		})
	}
}

func TestValidateThoughtCommandDeleteMayOmitQuote(t *testing.T) {
	t.Parallel()

	input := validThoughtCommand("delete")
	input.Payload = ReaderThoughtPayload{}
	input.PayloadJSON = []byte(`{"body":""}`)
	if err := validateThoughtCommand(input); err != nil {
		t.Fatalf("delete without quote should be accepted: %v", err)
	}
}

func TestValidateThoughtCommandAcceptsArchiveThoughtBoundaries(t *testing.T) {
	t.Parallel()

	longAnnotation := validThoughtCommand("add")
	longAnnotation.AnnotationID = strings.Repeat("a", 129)
	if err := validateThoughtCommand(longAnnotation); err != nil {
		t.Fatalf("129-byte annotation_id should be accepted: %v", err)
	}

	deleteWithArchivedHost := validThoughtCommand("delete")
	deleteWithArchivedHost.HostKind = "inbox"
	deleteWithArchivedHost.HostID = "purged-inbox:legacy-42"
	deleteWithArchivedHost.Target = ReaderThoughtTarget{Kind: "inbox", HostID: "purged-inbox:legacy-42", MetadataRevision: 1}
	deleteWithArchivedHost.TargetJSON = []byte(`{"kind":"inbox","host_id":"purged-inbox:legacy-42","version":{"metadata_revision":1}}`)
	deleteWithArchivedHost.Payload = ReaderThoughtPayload{}
	deleteWithArchivedHost.PayloadJSON = []byte(`{}`)
	if err := validateThoughtCommand(deleteWithArchivedHost); err != nil {
		t.Fatalf("delete with non-UUID persisted host should be accepted: %v", err)
	}
}

func TestValidateThoughtCommandRequiresQuoteForAddAndUpdate(t *testing.T) {
	t.Parallel()

	for _, operationKind := range []string{"add", "update"} {
		input := validThoughtCommand(operationKind)
		input.Payload = ReaderThoughtPayload{}
		input.PayloadJSON = []byte(`{"body":"missing quote"}`)
		if err := validateThoughtCommand(input); err == nil {
			t.Fatalf("%s without quote should be rejected", operationKind)
		}
	}
}

func TestValidateThoughtCommandAcceptsClientOwnedReattachCommand(t *testing.T) {
	t.Parallel()

	input := validThoughtCommand("update")
	input.ContractVersion = model.ReaderThoughtContractVersion
	input.LogicalClock = 17
	input.Payload = ReaderThoughtPayload{
		Reattach:     &model.ReaderThoughtReattachOperation{ExpectedLastSequence: 11, ExpectedHostRevision: 3},
		ReattachOnly: true,
	}
	input.PayloadJSON = []byte(`{"reattach":{"expected_last_sequence":11,"expected_host_revision":3}}`)
	if err := validateThoughtCommand(input); err != nil {
		t.Fatalf("validateThoughtCommand() error = %v", err)
	}
	if input.Payload.Reattach == nil || input.Payload.Reattach.ExpectedLastSequence != 11 || input.Payload.Reattach.ExpectedHostRevision != 3 {
		t.Fatalf("reattach payload = %#v, want reattach CAS command", input.Payload.Reattach)
	}
}

func TestValidateThoughtCommandRejectsMalformedClientReattachCommand(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*ReaderThoughtOpCommand)
	}{
		{
			name: "contains client body",
			mutate: func(input *ReaderThoughtOpCommand) {
				input.Payload.ReattachOnly = false
			},
		},
		{
			name: "wrong target kind",
			mutate: func(input *ReaderThoughtOpCommand) {
				input.Target = ReaderThoughtTarget{Kind: "summary", HostID: "link-1", SourceHash: "hash"}
			},
		},
		{
			name: "target revision differs from expected revision",
			mutate: func(input *ReaderThoughtOpCommand) {
				input.Target = ReaderThoughtTarget{Kind: "saved-content", HostID: "link-1", ContentRevision: 4}
			},
		},
		{
			name: "not an update",
			mutate: func(input *ReaderThoughtOpCommand) {
				input.OperationKind = "add"
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			input := validThoughtCommand("update")
			input.ContractVersion = model.ReaderThoughtContractVersion
			input.LogicalClock = 17
			input.Payload = ReaderThoughtPayload{
				Reattach:     &model.ReaderThoughtReattachOperation{ExpectedLastSequence: 11, ExpectedHostRevision: 3},
				ReattachOnly: true,
			}
			input.PayloadJSON = []byte(`{"reattach":{"expected_last_sequence":11,"expected_host_revision":3}}`)
			tc.mutate(&input)
			if err := validateThoughtCommand(input); err == nil {
				t.Fatal("validateThoughtCommand() error = nil, want malformed reattach rejection")
			}
		})
	}
}

func TestValidateThoughtCommandRejectsInvalidOperationEnvelope(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*ReaderThoughtOpCommand)
	}{
		{name: "missing op id", mutate: func(input *ReaderThoughtOpCommand) { input.OpID = " " }},
		{name: "missing device id", mutate: func(input *ReaderThoughtOpCommand) { input.DeviceID = " " }},
		{name: "negative logical clock", mutate: func(input *ReaderThoughtOpCommand) { input.LogicalClock = -1 }},
		{name: "unknown operation", mutate: func(input *ReaderThoughtOpCommand) { input.OperationKind = "replace" }},
		{name: "missing annotation id", mutate: func(input *ReaderThoughtOpCommand) { input.AnnotationID = " " }},
		{name: "missing host kind", mutate: func(input *ReaderThoughtOpCommand) { input.HostKind = " " }},
		{name: "missing host id", mutate: func(input *ReaderThoughtOpCommand) { input.HostID = " " }},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			input := validThoughtCommand("add")
			tc.mutate(&input)
			if err := validateThoughtCommand(input); err == nil {
				t.Fatal("validateThoughtCommand() error = nil, want envelope validation error")
			}
		})
	}
}

func TestValidateThoughtCommandEnforcesVersionedLamportClock(t *testing.T) {
	t.Parallel()

	for _, operationKind := range []string{"add", "update", "delete"} {
		for _, logicalClock := range []int64{1, 41, model.ReaderThoughtMaxLogicalClock} {
			input := validThoughtCommand(operationKind)
			input.ContractVersion = model.ReaderThoughtContractVersion
			input.LogicalClock = logicalClock
			if err := validateThoughtCommand(input); err != nil {
				t.Fatalf("validateThoughtCommand(%s, %d) error = %v", operationKind, logicalClock, err)
			}
		}
	}

	for _, logicalClock := range []int64{0, -1, model.ReaderThoughtMaxLogicalClock + 1} {
		input := validThoughtCommand("update")
		input.ContractVersion = model.ReaderThoughtContractVersion
		input.LogicalClock = logicalClock
		if err := validateThoughtCommand(input); err == nil {
			t.Fatalf("v1 logical_clock %d should be rejected", logicalClock)
		}
	}

	missingVersion := validThoughtCommand("update")
	missingVersion.ContractVersion = 0
	if err := validateThoughtCommand(missingVersion); err == nil {
		t.Fatal("missing contract_version should be rejected")
	}

	unsupported := validThoughtCommand("update")
	unsupported.ContractVersion = model.ReaderThoughtContractVersion + 1
	unsupported.LogicalClock = 1
	if err := validateThoughtCommand(unsupported); err == nil {
		t.Fatal("unsupported contract_version should be rejected")
	}
}

func TestValidateThoughtCommandRequiresCompleteValidRecoveryMetadata(t *testing.T) {
	t.Parallel()

	input := validThoughtCommand("update")
	input.ContractVersion = model.ReaderThoughtContractVersion
	input.LogicalClock = 9
	input.RecoveryOf = &model.ReaderThoughtVersionKey{
		LogicalClock: 4,
		DeviceID:     "loser-device",
		OpID:         "loser-op",
	}
	if err := validateThoughtCommand(input); err == nil {
		t.Fatal("recovery without expected current winner should be rejected")
	} else {
		var status *problem.Error
		if !errors.As(err, &status) || problemHTTPStatus(status) != http.StatusUnprocessableEntity ||
			status.Code() != "invalid_thought_recovery" {
			t.Fatalf("incomplete recovery error = %v, want 422 invalid_thought_recovery", err)
		}
	}

	input.ExpectedCurrentWinnerKey = &model.ReaderThoughtVersionKey{
		LogicalClock: 8,
		DeviceID:     "winner-device",
		OpID:         "winner-op",
	}
	if err := validateThoughtCommand(input); err != nil {
		t.Fatalf("complete recovery metadata should be accepted: %v", err)
	}

	input.ExpectedCurrentWinnerKey.OpID = " "
	if err := validateThoughtCommand(input); err == nil {
		t.Fatal("non-canonical recovery key should be rejected")
	}
}

func TestValidateThoughtCommandRejectsNonCanonicalIdentifiers(t *testing.T) {
	t.Parallel()

	for _, opID := range []string{
		" leading-space",
		"trailing-space ",
		"contains\x00nul",
		string([]byte{0xff}),
		strings.Repeat("a", 129),
	} {
		input := validThoughtCommand("add")
		input.ContractVersion = model.ReaderThoughtContractVersion
		input.LogicalClock = 1
		input.OpID = opID
		if err := validateThoughtCommand(input); err == nil {
			t.Fatalf("non-canonical op_id %q should be rejected", opID)
		}
	}
}

func TestReaderServiceRejectsInvalidDesiredStateCommandsBeforeStore(t *testing.T) {
	t.Parallel()

	service := newReaderTestFeatureSet(ReaderStores{}, nil)
	if _, err := service.PushThoughtOps(context.Background(), ReaderThoughtOpsCommand{}); err == nil {
		t.Fatal("PushThoughtOps() error = nil for empty batch")
	}
	if _, err := service.CreateTodo(context.Background(), ReaderTodoCreateCommand{Text: "  "}); err == nil {
		t.Fatal("CreateTodo() error = nil for blank text")
	}
	linkID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	if _, err := service.PatchEngagement(context.Background(), model.ReaderEngagementPatch{LinkID: linkID}); err == nil {
		t.Fatal("PatchEngagement() error = nil for empty desired state")
	}
	progress := float32(math.NaN())
	if _, err := service.PatchEngagement(context.Background(), model.ReaderEngagementPatch{LinkID: linkID, Progress: &progress}); err == nil {
		t.Fatal("PatchEngagement() error = nil for NaN progress")
	}
}

func TestNewReaderApplicationsRejectsMissingCommandDependencies(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("NewReaderApplications() did not reject missing command dependencies")
		}
	}()
	_ = NewReaderApplications(ReaderStores{}, nil, ReaderApplicationOptions{})
}
