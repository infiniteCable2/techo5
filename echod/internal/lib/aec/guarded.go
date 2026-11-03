package aec

import (
	"math"
	"sync/atomic"
)

// Guarded keeps the output filter fixed while a second NLMS filter learns.
// Learned coefficients must improve a subsequent, non-adapting window before
// reaching the output. This is a two-path controller, not a voice detector or a
// guarantee of separating correlated near-end and playback signals.
type Guarded struct {
	stable, candidate *Canceller
	on                atomic.Bool
	wasOn             bool
	training          int
	checking          int
	quietSamples      int
	input, old, next  float64
	sumD, sumE        float64 // actual returned PCM, including promotion fades
}

// At the device's 16 kHz rate these are 200 ms training and 200 ms held-out
// validation. The controller adds no audio buffering or output delay.
const guardWindow = 3200

// NewGuarded uses the same linear filter, taps and adaptation rate as New.
// The output filter never adapts directly; SetAdapting gates candidate learning
// and promotion together, for an explicit listening phase to remain protected.
func NewGuarded(cfg Config) (*Guarded, error) {
	stable, err := New(cfg)
	if err != nil {
		return nil, err
	}
	candidate, err := New(cfg)
	if err != nil {
		return nil, err
	}
	stable.SetAdapting(false)
	g := &Guarded{stable: stable, candidate: candidate}
	g.on.Store(true)
	return g, nil
}

func (g *Guarded) clearWindow() {
	g.training, g.checking, g.quietSamples = 0, 0, 0
	g.input, g.old, g.next = 0, 0, 0
}

func (g *Guarded) resume() {
	// Listening may have advanced only the stable path. Start a new candidate
	// from the accepted filter and matching reference history, never stale PCM.
	copy(g.candidate.w, g.stable.w)
	copy(g.candidate.hist, g.stable.hist)
	g.candidate.at, g.candidate.pow, g.candidate.since = g.stable.at, g.stable.pow, g.stable.since
	g.clearWindow()
}

// Process returns the stable path. Validation is measured on data the candidate
// did not learn from. Mere playback activity or lower in-sample error cannot
// promote a filter.
func (g *Guarded) Process(mic, ref []int16) (out []int16, err error) {
	if len(mic) != len(ref) {
		return nil, ErrLength
	}
	defer func() {
		if err == nil {
			for i, v := range mic {
				g.sumD = g.sumD*(1-1.0/erleTau) + float64(v)*float64(v)
				g.sumE = g.sumE*(1-1.0/erleTau) + float64(out[i])*float64(out[i])
			}
		}
	}()
	if len(mic) == 0 {
		return g.stable.Process(mic, ref)
	}
	on := g.on.Load()
	if on && !g.wasOn {
		g.resume()
	}
	g.wasOn = on
	out, err = g.stable.Process(mic, ref)
	if err != nil || !on {
		return out, err
	}
	var refPower float64
	clipped := false
	for i, v := range ref {
		refPower += float64(v) * float64(v)
		clipped = clipped || v >= 32760 || v <= -32760 || mic[i] >= 32760 || mic[i] <= -32760
	}
	active := refPower > quiet*full*full*float64(len(ref)) && !clipped
	validating := g.training >= guardWindow
	g.candidate.SetAdapting(active && !validating)
	next, err := g.candidate.Process(mic, ref)
	if err != nil {
		return nil, err
	}
	if !active {
		g.quietSamples += len(ref)
		if clipped || g.quietSamples >= 8000 {
			copy(g.candidate.w, g.stable.w)
			g.clearWindow()
		}
		return out, nil
	}
	g.quietSamples = 0
	if !validating {
		g.training += len(ref)
		return out, nil
	}
	for i, v := range mic {
		g.input += float64(v) * float64(v)
		g.old += float64(out[i]) * float64(out[i])
		g.next += float64(next[i]) * float64(next[i])
	}
	g.checking += len(ref)
	if g.checking < guardWindow {
		return out, nil
	}
	// Require 6 dB removal against the input and 3 dB improvement over the
	// accepted filter on held-out data. A good fit to the candidate's training
	// noise alone is insufficient; this is not an absolute double-talk oracle.
	switch {
	case g.input > 0 && g.next*4 < g.input && g.next*2 < g.old:
		// Both predictions are already available for this block. Crossfade
		// into the accepted path instead of switching its gain at a boundary.
		for i := range out {
			mix := float64(i+1) / float64(len(out))
			out[i] = int16((1-mix)*float64(out[i]) + mix*float64(next[i]))
		}
		copy(g.stable.w, g.candidate.w)
	case g.next > g.old*1.25:
		// A candidate damaged by double talk must not poison the accepted path.
		copy(g.candidate.w, g.stable.w)
	}
	g.clearWindow()
	return out, nil
}

// SetAdapting is safe concurrently with Process. Its state is sampled once per
// block; false prevents both learning and promotion from that block onward.
func (g *Guarded) SetAdapting(on bool) { g.on.Store(on) }
func (g *Guarded) Adapting() bool      { return g.on.Load() }
func (g *Guarded) ERLE() float64 {
	if g.sumD <= 0 || g.sumE <= 0 {
		return 0
	}
	return 10 * math.Log10(g.sumD/g.sumE)
}

func (g *Guarded) Reset() {
	g.stable.Reset()
	g.candidate.Reset()
	g.wasOn = false
	g.sumD, g.sumE = 0, 0
	g.clearWindow()
}
