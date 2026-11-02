package voice

import (
	"github.com/HuskerMinion/techo5/echod/internal/hardware/led"
	"math"
	"testing"
	"time"
)

func TestListeningMeterRisesWithLevelWithinExistingColorHeadroom(t *testing.T) {
	base := led.Color{R: 200, G: 120, B: 60}
	for _, at := range []time.Duration{0, 300 * time.Millisecond, 600 * time.Millisecond} {
		var previous byte
		for _, level := range []float64{0, .001, .02, .2, 1} {
			frame := listeningMeterFrame(base, at, level, false)
			if len(frame) != led.Segments {
				t.Fatal("meter lost ring segments")
			}
			for _, color := range frame {
				if color.R < previous || color.R > base.R || color.G > base.G || color.B > base.B {
					t.Fatal("meter is not monotone or overrides manual brightness")
				}
			}
			previous = frame[0].R
		}
	}
	if listeningMeterFrame(base, 0, 0, false)[0].R >= listeningMeterFrame(base, 600*time.Millisecond, 0, false)[0].R {
		t.Fatal("quiet listening stopped fading")
	}
	if listeningMeterFrame(base, 0, 0, true)[0].R <= listeningMeterFrame(base, 0, 0, false)[0].R {
		t.Fatal("a marginal raw gate crossing is invisible")
	}
	if got := listeningMeterFrame(led.Color{}, 0, 1, true)[0]; got != (led.Color{}) {
		t.Fatal("meter lit a deliberately black color")
	}
	if got := listeningMeterFrame(base, 0, math.NaN(), false)[0]; got != listeningMeterFrame(base, 0, 0, false)[0] {
		t.Fatal("invalid level changed the ring")
	}
}

func TestListeningMeterRetiresAtProcessingAndDoesNotChangeOpeningGuard(t *testing.T) {
	c, _, _ := nativeListeningTurn(t)
	c.showListening()
	if c.claim.Showing().Animate == nil || c.noSpeech == nil {
		t.Fatal("native listening did not keep meter and opening guard")
	}
	c.handle(event{kind: evSpeaking, text: c.turn.ID()})
	c.think()
	if got := c.claim.Showing(); got.Animate != nil || got.Effect != led.EffectComet {
		t.Fatal("listening meter leaked into processing")
	}
	c.handle(event{kind: evCancel})
	if got := c.claim.Showing(); got.Animate != nil || len(got.Frame) != 0 || got.Effect != "" {
		t.Fatal("meter survived turn end")
	}
}
