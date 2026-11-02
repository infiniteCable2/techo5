package voice

import (
	"encoding/json"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/hardware/speaker"
	gw "github.com/HuskerMinion/techo5/echod/internal/lib/openclaw"
)

type captionMark struct {
	known              bool
	generation, sample int64
}
type captionPresentation struct {
	canonical, drop, textChanged bool
	text                         string
	mark                         captionMark
}

func (b *clawBackend) presentation(t *clawTurn, raw json.RawMessage) captionPresentation {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.turn != t || t.timings == nil {
		return captionPresentation{}
	}
	r := captionPresentation{canonical: true}
	var e gw.OutputPresentationEvent
	if json.Unmarshal(raw, &e) != nil {
		return r
	}
	block, changed := t.timings.Apply(raw)
	if e.Type == "clear" {
		r.drop = !changed
	}
	if e.Type == "audio" {
		if e.OutputGeneration != nil && *e.OutputGeneration != t.timings.Generation() {
			r.drop = true
			return r
		}
		if changed && e.OutputGeneration != nil && e.AudioStartSample != nil {
			r.mark = captionMark{true, *e.OutputGeneration, *e.AudioStartSample}
		}
	}
	if block != nil {
		r.textChanged = true
		r.text = t.timings.Text()
	}
	return r
}

func (b *clawBackend) hasTimings() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.turn != nil && b.turn.timings != nil
}

func (b *clawBackend) captionPosition(mark captionMark) (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !mark.known || b.turn == nil || b.turn.timings == nil {
		return 0, false
	}
	return b.turn.timings.AbsoluteAt(mark.generation, mark.sample)
}

type captionPlaybackRange struct {
	start, end int64
	mark       captionMark
}
type captionPlaybackClock struct {
	received, submitted, heard int64
	ranges                     []captionPlaybackRange
	emptyAt, lastEmit          time.Time
	emptyDelay                 time.Duration
}

func (p *captionPlaybackClock) add(samples int, mark captionMark) {
	start := p.received
	p.received += int64(samples)
	if !mark.known {
		return
	}
	if n := len(p.ranges); n > 0 {
		last := &p.ranges[n-1]
		if last.end == start && last.mark.generation == mark.generation && last.mark.sample+last.end-last.start == mark.sample {
			last.end = p.received
			return
		}
	}
	// Fragments only affect optional following, never the existing audio queue.
	if len(p.ranges) < 512 {
		p.ranges = append(p.ranges, captionPlaybackRange{start, p.received, mark})
	}
}

// Position is based on submitted audio minus software queue and the card's
// measured latency. Once the queue runs dry, decay that last reading rather
// than repeatedly charging a ring now containing silence/background audio.
func (p *captionPlaybackClock) poll(now time.Time, queued int, latency time.Duration, emit func(captionMark)) {
	if p.submitted == 0 || emit == nil {
		return
	}
	latency = max(latency, 0)
	if queued > 0 {
		p.emptyAt = time.Time{}
	} else {
		if p.emptyAt.IsZero() {
			p.emptyAt = now
			p.emptyDelay = latency
		}
		latency = max(p.emptyDelay-now.Sub(p.emptyAt), 0)
	}
	heard := p.submitted - int64(max(queued, 0))*clawOutputRate/speaker.Rate - int64(latency)*clawOutputRate/int64(time.Second)
	p.heard = max(p.heard, min(p.submitted, max(heard, 0)))
	for len(p.ranges) > 0 && p.ranges[0].end <= p.heard {
		p.ranges = p.ranges[1:]
	}
	if len(p.ranges) == 0 || p.heard < p.ranges[0].start || (!p.lastEmit.IsZero() && now.Sub(p.lastEmit) < 100*time.Millisecond) {
		return
	}
	r := p.ranges[0]
	p.lastEmit = now
	emit(captionMark{true, r.mark.generation, r.mark.sample + p.heard - r.start})
}
