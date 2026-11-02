package voice

import (
	"github.com/HuskerMinion/techo5/echod/internal/lib/endpoint"
	"testing"
	"time"
)

func TestNativeSofterOngoingSpeechSurvivesRelativePeakDrop(t *testing.T) {
	settings := nativeEndpointSettings(1200)
	ep := endpoint.New(settings)
	var activity nativeActivity
	step := 20 * time.Millisecond
	feed := func(level int16, aboveRoom bool) bool {
		activity.feed(aboveRoom, step)
		frame := make([]int16, 320)
		for i := range frame {
			frame[i] = level
		}
		return activity.ended(ep.Feed(frame), time.Duration(settings.Quiet)*100*time.Millisecond)
	}
	for range 50 {
		if feed(2000, true) {
			t.Fatal("loud opening ended")
		}
	}
	// Soft speech falls below 30% of the opening peak but still crosses the
	// adaptive room gate on syllables. This matches the observed sparse gate.
	for i := range 1200 {
		if feed(400, i%8 == 0) {
			t.Fatalf("ongoing quieter speech ended after %.2f seconds", 1+float64(i+1)*.02)
		}
	}
	if feed(400, true) {
		t.Fatal("last spoken syllable ended capture")
	}
	for range 59 {
		if feed(20, false) {
			t.Fatal("real pause ended before 1.2 seconds")
		}
	}
	if !feed(20, false) {
		t.Fatal("actual 1.2-second pause did not end the request")
	}
}

func TestNativeSpeechResumptionResetsQuietGate(t *testing.T) {
	var a nativeActivity
	a.feed(true, 200*time.Millisecond)
	a.feed(false, time.Second)
	if a.ended(true, 1200*time.Millisecond) {
		t.Fatal("short pause ended capture")
	}
	a.feed(true, 20*time.Millisecond)
	a.feed(false, time.Second)
	if a.ended(true, 1200*time.Millisecond) {
		t.Fatal("quiet duration leaked across resumed speech")
	}
	a.feed(false, 200*time.Millisecond)
	if !a.ended(true, 1200*time.Millisecond) {
		t.Fatal("actual quiet window was ignored")
	}
}
