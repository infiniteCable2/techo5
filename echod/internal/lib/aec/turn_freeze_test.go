package aec

import (
	"math"
	"testing"
)

// A speech-only device freezes a fresh filter before its first reply. Playback
// activity is then true, but no acoustic path has ever been learned. Compare
// against identical held-out input with actual prior adaptation; no hardware.
func TestColdTurnFreezeLeavesOwnPlaybackUncancelled(t *testing.T) {
	ref := noiseAt(128000, 8000, 41)
	h := make([]float64, 41)
	h[30] = .5
	h[40] = .15
	mic := echoed(ref, h)
	frozen, err := New(Config{Taps: 1024, Mu: .1})
	if err != nil {
		t.Fatal(err)
	}
	frozen.SetAdapting(false)
	d, out := run(t, frozen, mic, ref, 8000)
	cold := erle(d, out)
	learned, err := New(Config{Taps: 1024, Mu: .1})
	if err != nil {
		t.Fatal(err)
	}
	d, out = run(t, learned, mic, ref, 8000)
	trained := erle(d, out)
	t.Logf("held-out own playback: cold/frozen %.2f dB, trained/frozen %.2f dB", cold, trained)
	if math.Abs(cold) > .02 {
		t.Fatalf("untrained frozen filter unexpectedly removes echo: %.2f dB", cold)
	}
	if trained < 25 {
		t.Fatalf("reference/path control did not converge: %.2f dB", trained)
	}
}
