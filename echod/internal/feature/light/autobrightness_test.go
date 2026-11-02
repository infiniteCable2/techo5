package light

import (
	"math"
	"testing"
)

func TestRoomBrightnessIsBoundedSmoothAndRejectsInvalidReadings(t *testing.T) {
	previous := 0.0
	for _, lux := range []float64{0, 1, 10, 50, 250, 5000} {
		v, ok := roomScale(lux)
		if !ok || v < .2 || v > 1 || v < previous {
			t.Fatal("room curve is not bounded and monotone", lux, v)
		}
		previous = v
	}
	for _, lux := range []float64{-1, math.NaN(), math.Inf(1)} {
		if _, ok := roomScale(lux); ok {
			t.Fatal("invalid sensor reading accepted")
		}
	}
	if dim := settleScale(1, .2); dim <= .2 || dim >= 1 {
		t.Fatal("darkness was applied abruptly")
	}
	if settleScale(.2, 1)-.2 <= 1-settleScale(1, .2) {
		t.Fatal("brightening is not faster than dimming")
	}
}
