package guide

import (
	"testing"
	"time"
)

const sample = `<?xml version="1.0" encoding="UTF-8"?>
<tv>
  <channel id="c5"><display-name>5 Disney Channel</display-name></channel>
  <channel id="c4"><display-name>4 Nickelodeon</display-name></channel>
  <programme start="20260714232600 -0400" stop="20260714234900 -0400" channel="c5"><title>Wizards of Waverly Place</title><sub-title>Saving Wiz Tech</sub-title></programme>
  <programme start="20260714234900 -0400" stop="20260715001200 -0400" channel="c5"><title>The Suite Life of Zack and Cody</title></programme>
  <programme start="20260714234300 -0400" stop="20260715000500 -0400" channel="c4"><title>Jimmy Neutron</title></programme>
</tv>`

func at(h, m int) time.Time {
	loc, _ := time.LoadLocation("America/New_York")
	return time.Date(2026, 7, 14, h, m, 0, 0, loc)
}

func TestParsePreservesLineupAndSplitsName(t *testing.T) {
	chans, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(chans) != 2 {
		t.Fatalf("want 2 channels, got %d", len(chans))
	}
	if chans[0].Number != "5" || chans[0].Name != "Disney Channel" {
		t.Fatalf("first channel parsed as %q %q", chans[0].Number, chans[0].Name)
	}
	if chans[1].Number != "4" {
		t.Fatalf("lineup order not preserved: %+v", chans)
	}
}

func TestNowNext(t *testing.T) {
	chans, _ := Parse([]byte(sample))
	disney := chans[0]

	now, next := disney.NowNext(at(23, 40))
	if now == nil || now.Title != "Wizards of Waverly Place" {
		t.Errorf("now = %v, want Wizards", now)
	}
	if next == nil || next.Title != "The Suite Life of Zack and Cody" {
		t.Errorf("next = %v, want Suite Life", next)
	}
}

func TestNowNextBeforeGuideStarts(t *testing.T) {
	chans, _ := Parse([]byte(sample))
	now, next := chans[0].NowNext(at(20, 0))
	if now != nil {
		t.Errorf("expected nothing airing, got %v", now)
	}
	if next == nil || next.Title != "Wizards of Waverly Place" {
		t.Errorf("next = %v, want the first upcoming programme", next)
	}
}
