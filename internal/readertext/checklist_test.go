package readertext

import "testing"

func TestBlockReferenceSurvivesNearbyInsertionsAndDeletions(t *testing.T) {
	before := List("# Plan\n\n- [ ] First\nContext\n")[0]
	inserted := List("# Plan\n\n- [ ] First\nInserted nearby\nContext\n")[0]
	deleted := List("# Plan\n\n- [ ] First\n")[0]

	if inserted.BlockRef != before.BlockRef || deleted.BlockRef != before.BlockRef {
		t.Fatalf("block refs = %q, %q, %q; nearby edits must not move the anchor", before.BlockRef, inserted.BlockRef, deleted.BlockRef)
	}
}

func TestListIgnoresFencedChecklistLines(t *testing.T) {
	blocks := List("```md\n- [ ] example\n```\n# Plan\n- [ ] real\n")
	if len(blocks) != 1 || blocks[0].Text != "real" {
		t.Fatalf("blocks = %+v, want only the unfenced task", blocks)
	}
}

func TestRepeatedStableAnchorsUseOccurrence(t *testing.T) {
	source := "# Plan\n- [ ] same\nfirst context\n- [x] same\nsecond context\n"
	blocks := List(source)
	if len(blocks) != 2 || blocks[0].BlockRef != blocks[1].BlockRef || blocks[0].Occurrence != 1 || blocks[1].Occurrence != 2 {
		t.Fatalf("blocks = %+v, want one ref with occurrences 1 and 2", blocks)
	}

	updated := Update(source, blocks[1].BlockRef, 2, false)
	want := "# Plan\n- [ ] same\nfirst context\n- [ ] same\nsecond context\n"
	if updated.Status != Updated || updated.Source != want {
		t.Fatalf("Update() = %+v, want only the second marker changed", updated)
	}
}

func TestUpdateOnlyChangesCheckboxByte(t *testing.T) {
	source := "- [ ] keep  \r\ntext\n"
	block := List(source)[0]
	updated := Update(source, block.BlockRef, block.Occurrence, true)
	if updated.Status != Updated || updated.Source != "- [x] keep  \r\ntext\n" {
		t.Fatalf("Update() = %+v, want only checkbox byte changed", updated)
	}
}
