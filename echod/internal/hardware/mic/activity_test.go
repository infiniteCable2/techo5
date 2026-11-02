package mic

import (
	"math"
	"testing"
)

func TestSpeechActivityDoesNotInheritVisualMeterRelease(t *testing.T) {
	l := newLeveler()
	l.bandFloor = 100
	l.publishLevel(2000)
	if !l.active.Load() || math.Float32frombits(l.level.Load()) == 0 {
		t.Fatal("speech above the existing floor was not published")
	}
	l.publishLevel(100)
	if l.active.Load() {
		t.Fatal("finished speech retained the opening timer activity flag")
	}
	if math.Float32frombits(l.level.Load()) == 0 {
		t.Fatal("visual meter's existing release was changed")
	}
}

func TestSilenceAndStableRoomDoNotTriggerSpeechActivity(t *testing.T) {
	for _, floor := range []float32{1, 100, 1000} {
		l := newLeveler()
		l.bandFloor = floor
		for _, rms := range []float32{0, floor / 2, floor} {
			l.publishLevel(rms)
			if l.active.Load() {
				t.Fatal("room floor counted as speech")
			}
		}
	}
}

func TestShortActivityRemainsObservableWithoutClearingAnotherReader(t *testing.T) {
	l := newLeveler()
	l.bandFloor = 100
	source := &Source{leveler: l}
	l.publishLevel(2000)
	first := source.SpeechActivityFrames()
	l.publishLevel(100)
	if first != 1 || source.SpeechActive() || source.SpeechActivityFrames() != first {
		t.Fatal("short gate crossing disappeared or visual release counted as activity")
	}
	if source.SpeechActivityFrames() != first {
		t.Fatal("one observer consumed another observer's evidence")
	}
	l.publishLevel(2000)
	if source.SpeechActivityFrames() != first+1 {
		t.Fatal("new crossing did not advance the observation counter")
	}
}

func TestActivityUsesConfiguredSensitivityWithoutReleaseHysteresis(t *testing.T) {
	l := newLeveler()
	l.bandFloor = 100
	l.atSensitivity(8)
	atDB := func(db float64) { l.publishLevel(float32(100 * math.Pow(10, db/20))) }
	atDB(7)
	if l.active.Load() {
		t.Fatal("input opened below its onset threshold")
	}
	atDB(9)
	if !l.active.Load() || math.Float32frombits(l.level.Load()) == 0 {
		t.Fatal("configured room gate was not restored")
	}
	atDB(7)
	if l.active.Load() {
		t.Fatal("activity inherited a lowered threshold or visual release")
	}
	l.atSensitivity(12)
	atDB(9)
	if l.active.Load() {
		t.Fatal("activity ignored the configured sensitivity")
	}
}
