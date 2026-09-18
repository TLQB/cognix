package zbridge

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------- deck ops

func TestSlideDeckInsertShiftsFollowing(t *testing.T) {
	d := NewSlideDeck()
	for i := 1; i <= 3; i++ {
		if err := d.Apply(SlideOp{Tool: "insert_page", Position: i, Title: "s" + string(rune('0'+i)), HTML: "<p>"}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	// Insert at 1: previous 1..3 become 2..4.
	if err := d.Apply(SlideOp{Tool: "insert_page", Position: 1, Title: "new", HTML: "<p>"}); err != nil {
		t.Fatalf("insert at 1: %v", err)
	}
	got := d.List()
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	if got[0].Title != "new" || got[0].Position != 1 {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Title != "s1" || got[1].Position != 2 {
		t.Errorf("second = %+v", got[1])
	}
}

func TestSlideDeckUpdateAndRemove(t *testing.T) {
	d := NewSlideDeck()
	for i := 1; i <= 4; i++ {
		d.Apply(SlideOp{Tool: "insert_page", Position: i, Title: "s" + string(rune('0'+i)), HTML: "<p>old"})
	}
	if err := d.Apply(SlideOp{Tool: "update_page", Position: 2, Title: "updated", HTML: "<p>new"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := d.List()
	if got[1].Title != "updated" || got[1].HTML != "<p>new" {
		t.Errorf("update not applied: %+v", got[1])
	}
	// Remove 2 slides starting at 3: positions 3,4 gone; 4 slides total -> 2 left.
	if err := d.Apply(SlideOp{Tool: "remove_slides", Position: 3, Count: 2}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	got = d.List()
	if len(got) != 2 {
		t.Fatalf("after remove len = %d, want 2", len(got))
	}
	if got[0].Position != 1 || got[1].Position != 2 {
		t.Errorf("positions not compacted: %+v", got)
	}
	// Update a missing slide errors.
	if err := d.Apply(SlideOp{Tool: "update_page", Position: 99}); err == nil {
		t.Error("update missing slide: want error")
	}
}

// ---------------------------------------------------------------- parser

const sampleSlidesOutput = `Here is your deck.

<slide-css>
.slide { width: 1280px; height: 720px; }
</slide-css>

<slide 1 title="Overview">
<section class="slide"><h1>Overview</h1></section>
</slide>

<slide 2 title="Architecture">
<section class="slide"><h1>Arch</h1></section>
</slide>

<slide 1 title="[update] Overview v2">
<section class="slide"><h1>Overview v2</h1></section>
</slide>

<slide 2 title="[remove]">
remove
</slide>`

func TestParseSlideBlocks(t *testing.T) {
	ops := ParseSlideBlocks(sampleSlidesOutput)
	// set_css + insert1 + insert2 + update1 + remove2
	if len(ops) != 5 {
		t.Fatalf("ops = %d, want 5: %+v", len(ops), ops)
	}
	if ops[0].Tool != "set_css" || !strings.Contains(ops[0].CSS, "1280px") {
		t.Errorf("set_css op = %+v", ops[0])
	}
	if ops[1].Tool != "insert_page" || ops[1].Position != 1 || ops[1].Title != "Overview" {
		t.Errorf("op1 = %+v", ops[1])
	}
	if ops[2].Position != 2 || ops[2].Title != "Architecture" {
		t.Errorf("op2 = %+v", ops[2])
	}
	if ops[3].Tool != "update_page" || ops[3].Position != 1 || ops[3].Title != "Overview v2" {
		t.Errorf("op3 = %+v", ops[3])
	}
	if ops[4].Tool != "remove_slides" || ops[4].Position != 2 {
		t.Errorf("op4 = %+v", ops[4])
	}
}

func TestParseSlideBlocksEmpty(t *testing.T) {
	if ops := ParseSlideBlocks("no blocks here"); len(ops) != 0 {
		t.Errorf("want 0 ops, got %d", len(ops))
	}
}

// --------------------------------------------- deck roundtrip via parser

func TestDeckFromModelOutput(t *testing.T) {
	d := NewSlideDeck()
	for _, op := range ParseSlideBlocks(sampleSlidesOutput) {
		if err := d.Apply(op); err != nil {
			t.Fatalf("apply %+v: %v", op, err)
		}
	}
	got := d.List()
	if len(got) != 1 {
		t.Fatalf("deck len = %d, want 1 (insert2 then remove2 cancel out)", len(got))
	}
	if got[0].Title != "Overview v2" || !strings.Contains(got[0].HTML, "Overview v2") {
		t.Errorf("final slide = %+v", got[0])
	}
	if !strings.Contains(d.GlobalCSS, "1280px") {
		t.Errorf("global css = %q", d.GlobalCSS)
	}
}
