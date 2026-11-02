package voice

import (
	"context"
	"math"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/feature/wakeword"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/led"
)

// Listening keeps its calm fade, with headroom for activity above the adaptive
// room floor. This is presentation only: it never confirms or extends a turn.
// The LED driver's existing room-light scale still applies to every frame.
func listeningMeterFrame(base led.Color, elapsed time.Duration, level float64, gate bool) []led.Color {
	if math.IsNaN(level) || math.IsInf(level, 0) {
		level = 0
	}
	level = max(0, min(level, 1))
	wave := (1 - math.Cos(2*math.Pi*float64(elapsed%(1200*time.Millisecond))/float64(1200*time.Millisecond))) / 2
	floor := .32 + .12*wave
	boost := min(1, 3*math.Pow(level, .35))
	if gate {
		boost = max(boost, .70)
	}
	factor := floor + (1-floor)*boost
	dim := func(v byte) byte { return byte(math.Round(float64(v) * factor)) }
	return led.Solid(led.Color{R: dim(base.R), G: dim(base.G), B: dim(base.B)})
}

func (c *conversation) showListening() {
	effect := wakeword.Effect(c.slot)
	if effect == "" {
		c.claim.Clear()
		return
	}
	if !c.nativeTalk() || effect != led.EffectPulse {
		c.claim.Play(effect, c.ring.Base())
		return
	}
	base, source := c.ring.Base(), c.source
	c.claim.Show(led.Content{Animate: func(ctx context.Context, r *led.Ring) error {
		start := time.Now()
		var gateAt time.Time
		lastGate := source.SpeechActivityFrames()
		tick := time.NewTicker(led.FrameInterval)
		defer tick.Stop()
		for {
			if ctx.Err() != nil {
				return nil
			}
			now := time.Now()
			count := source.SpeechActivityFrames()
			if count != lastGate || source.SpeechActive() {
				gateAt = now
				lastGate = count
			}
			// The display retains a short gate crossing so a 20 ms sample is
			// visible at 25 fps. This visual hold never feeds input detection.
			recent := !gateAt.IsZero() && now.Sub(gateAt) < 160*time.Millisecond
			if err := r.SetSegments(listeningMeterFrame(base, now.Sub(start), source.Level(), recent)); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return nil
			case <-tick.C:
			}
		}
	}})
}
