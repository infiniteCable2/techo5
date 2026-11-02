package openclaw

import (
	"encoding/json"
	"os"
	"testing"
)

func timingFixture(t *testing.T) (*WordTimings, []json.RawMessage) {
	t.Helper()
	raw, err := os.ReadFile("testdata/word-timings-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	var ready OutputPresentationEvent
	if err := json.Unmarshal(f.Events[0], &ready); err != nil {
		t.Fatal(err)
	}
	if ready.OutputWordTimings == nil {
		t.Fatal("fixture has no explicit acceptance")
	}
	w := NewWordTimings(ready.SessionID, ready.RelaySessionID, *ready.OutputWordTimings)
	if w == nil {
		t.Fatal("valid contract was not accepted")
	}
	return w, f.Events
}

func applyTiming(t *testing.T, w *WordTimings, e OutputPresentationEvent) bool {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	_, changed := w.Apply(b)
	return changed
}

func loadTimingAudio(t *testing.T, w *WordTimings, events []json.RawMessage) OutputPresentationEvent {
	t.Helper()
	var words OutputPresentationEvent
	for _, raw := range events {
		var e OutputPresentationEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		if e.Type == "clear" {
			continue
		}
		if e.Type == "outputWordTimings" {
			words = e
		}
		w.Apply(raw)
	}
	return words
}

func TestWordTimingsPiecewisePlayout(t *testing.T) {
	w, events := timingFixture(t)
	for _, raw := range events {
		var e OutputPresentationEvent
		_ = json.Unmarshal(raw, &e)
		if e.Type == "clear" {
			continue
		}
		w.Apply(raw)
		if e.Type == "outputWordTimings" {
			if _, ok := w.At(4, 10800); ok {
				t.Fatal("prepared source claimed not-yet-emitted audio")
			}
		}
	}
	for _, sample := range []int64{4320, 4799, 9600, 10079} {
		got, ok := w.At(4, sample)
		if !ok || got.BlockID != "text-1" || got.Start != 0 || got.End != 5 {
			t.Fatalf("crossing word missing at %d: %+v %v", sample, got, ok)
		}
	}
	for _, sample := range []int64{4800, 6000, 9599} {
		if _, ok := w.At(4, sample); ok {
			t.Fatalf("music sample %d moved the cursor", sample)
		}
	}
	got, ok := w.At(4, 10800)
	if !ok || got.Start != 12 || got.End != 19 {
		t.Fatal("number expansion was not mapped to original Unicode span")
	}
	if _, ok := w.At(4, 14400); ok {
		t.Fatal("unemitted suffix claimed audio")
	}
}

func TestWordTimingsScopeAndFinality(t *testing.T) {
	w, events := timingFixture(t)
	words := loadTimingAudio(t, w, events)
	if applyTiming(t, w, words) {
		t.Fatal("duplicate final was applied twice")
	}
	words.Revision++
	words.Words[0].DisplaySpan = []int{21, 26}
	if applyTiming(t, w, words) {
		t.Fatal("new revision replaced a final snapshot")
	}
	got, ok := w.At(4, 4500)
	if !ok || got.Start != 0 {
		t.Fatal("final snapshot changed")
	}
	future := int64(99)
	words.OutputGeneration = &future
	if applyTiming(t, w, words) || w.generation != 4 {
		t.Fatal("metadata switched generation")
	}
	next := int64(5)
	if !applyTiming(t, w, OutputPresentationEvent{Type: "clear", SessionID: w.session, RelaySessionID: w.relay, OutputGeneration: &next}) {
		t.Fatal("authoritative clear was ignored")
	}
	if _, ok := w.At(4, 4500); ok {
		t.Fatal("old generation survived clear")
	}
	words.OutputGeneration = &next
	words.SessionID = "another-session"
	if applyTiming(t, w, words) {
		t.Fatal("foreign session entered receiver")
	}
}

func TestWordTimingsMalformedAndCanceled(t *testing.T) {
	for _, name := range []string{"unknown segment", "wrong text revision", "word beyond source", "invalid display span", "cancelled segment"} {
		t.Run(name, func(t *testing.T) {
			w, events := timingFixture(t)
			var words OutputPresentationEvent
			for _, raw := range events {
				var e OutputPresentationEvent
				_ = json.Unmarshal(raw, &e)
				if e.Type == "outputWordTimings" {
					words = e
					continue
				}
				if e.Type != "clear" {
					w.Apply(raw)
				}
			}
			switch name {
			case "unknown segment":
				words.SegmentID = "missing"
			case "wrong text revision":
				v := int64(99)
				words.TextRevision = &v
			case "word beyond source":
				words.Words[0].EndSample = *words.SourceSampleCount + 1
			case "invalid display span":
				words.Words[0].DisplaySpan = []int{0, 999}
			case "cancelled segment":
				e := words
				e.Type = "outputSegmentCancelled"
				e.Reason = "cancelled"
				applyTiming(t, w, e)
			}
			if applyTiming(t, w, words) {
				t.Fatal("invalid optional metadata admitted")
			}
			if _, ok := w.At(4, 4500); ok {
				t.Fatal("invalid data moved display")
			}
		})
	}
}

func TestWordTimingsRequireExplicitContract(t *testing.T) {
	gen := int64(0)
	for _, c := range []OutputTimingsContract{{}, {Accepted: true}, {Accepted: true, Version: 1, OutputGeneration: &gen, SampleRate: 16000}} {
		if NewWordTimings("session", "relay", c) != nil {
			t.Fatal("incomplete or declined contract enabled opt-in")
		}
	}
}
