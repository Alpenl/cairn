package dbintegration

import (
	"encoding/json"
	"strings"
	"testing"

	"webtag/internal/repository"
	"webtag/internal/service"
)

// TestReaderInboxListDoesNotCarryOversizedCaptureBody is the executable
// evidence for the Inbox list/detail split. A capture accepts a 4 MiB body and
// a 1 MiB note, and the Inbox list is read on every open — before the split the
// queue response shipped both.
//
// This test asserts the observable contract on real PostgreSQL:
//   - the list response never contains the capture body or the tail of the note;
//   - the whole page stays small even though the row is multi-megabyte;
//   - the card preview is bounded and still says something useful;
//   - GET /api/inbox/{id} keeps the full body and note detail contract.
func TestReaderInboxListDoesNotCarryOversizedCaptureBody(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	repo := repository.NewPGXReaderVNextRepository(pool)
	reader := service.NewReaderVNextService(repo, nil)

	const bodyMarker = "BODY-MUST-NOT-REACH-THE-INBOX-LIST"
	const noteTailMarker = "NOTE-TAIL-MUST-NOT-REACH-THE-INBOX-LIST"
	// 4 MiB of body and 1 MiB of note: the documented maximums a capture may
	// carry, not a token-sized stand-in.
	body := bodyMarker + strings.Repeat("x", 4*1024*1024)
	note := "Why this capture is worth keeping. " + strings.Repeat("y", 1024*1024) + noteTailMarker

	title := "Oversized capture"
	created, err := repo.CreateInbox(ctx, modelReaderInboxContract(
		"https://inbox-projection.example/oversized", &title, body, note, []string{"research"},
	))
	if err != nil {
		t.Fatalf("CreateInbox oversized: %v", err)
	}
	page, err := reader.ListInbox(ctx, "active", "", 30)
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != created.ID.String() || page.ActiveCount != 1 {
		t.Fatalf("ListInbox page = %#v, want the single pending capture", page)
	}

	payload, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("marshal Inbox list page: %v", err)
	}
	wire := string(payload)
	if strings.Contains(wire, bodyMarker) {
		t.Fatal("Inbox list response carries the capture body")
	}
	if strings.Contains(wire, noteTailMarker) {
		t.Fatal("Inbox list response carries the full user note")
	}
	// A 4 MiB body plus a 1 MiB note used to make this page multi-megabyte.
	// The bound is generous on purpose: it fails on a regression, not on
	// ordinary field additions.
	if len(payload) > 4096 {
		t.Fatalf("Inbox list page = %d bytes for one row, want a bounded card payload", len(payload))
	}

	item := page.Items[0]
	if runes := []rune(item.Preview); len(runes) == 0 || len(runes) > 281 {
		t.Fatalf("card preview = %d runes, want a non-empty bounded preview", len(runes))
	}
	if !strings.HasPrefix(item.Preview, "Why this capture is worth keeping.") {
		t.Fatalf("card preview = %q, want the note text the card used to render", item.Preview)
	}
	if item.Title == nil || *item.Title != title || item.Status != "pending" || item.Expired {
		t.Fatalf("card identity = %#v, want the pending capture card", item)
	}
	if item.MetadataRevision != created.MetadataRevision || len(item.Tags) != 1 || item.Tags[0] != "research" {
		t.Fatalf("card CAS/tag fields = %#v, want the batch-action inputs", item)
	}

	// The detail contract is unchanged: everything the list dropped is still
	// exactly one GET away.
	detail, err := reader.GetInbox(ctx, created.ID.String())
	if err != nil {
		t.Fatalf("GetInbox: %v", err)
	}
	if detail.Body != body || detail.Note != note {
		t.Fatalf("GetInbox body=%d bytes note=%d bytes, want the full capture", len(detail.Body), len(detail.Note))
	}
	if detail.ProposalStatus != "idle" || detail.MetadataRevision != created.MetadataRevision {
		t.Fatalf("GetInbox proposal/revision contract = %#v", detail)
	}
}
