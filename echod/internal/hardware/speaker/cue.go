package speaker

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Cue observes one feedback overlay at the existing playback queue. Completion
// means its audible prefix was handed to the device, with the hardware margin;
// it neither claims the speaker nor schedules another audio producer.
type Cue struct {
	mu sync.Mutex
	done chan struct{}
	end time.Time
	err error
}

type cueMark struct {
	cue *Cue
	remaining int // interleaved queue samples before the audible boundary
	handed bool
	after int // frames after that boundary in its final hardware period
}

var errCueRetired = errors.New("speaker cue retired before its boundary")

func (c *Cue) finish(end time.Time, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select { case <-c.done: return; default: }
	c.end, c.err = end, err
	close(c.done)
}

// Wait returns the original cue boundary, even when its consumer starts late.
func (c *Cue) Wait(ctx context.Context) (time.Time, error) {
	select {
	case <-ctx.Done(): return time.Time{}, ctx.Err()
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.end, c.err
	}
}

func (p *Player) retireCueLocked() {
	if p.cue != nil {
		p.cue.cue.finish(time.Time{}, errCueRetired)
		p.cue = nil
	}
}

// Called with the same lock that takes the period from the playback queue.
func (p *Player) takeCueLocked(take int) *cueMark {
	mark := p.cue
	if mark == nil || mark.handed { return nil }
	if mark.remaining > take { mark.remaining -= take; return nil }
	mark.after = period - mark.remaining/Channels
	mark.handed = true
	return mark
}

// Only the real successful device handoff can complete a cue. A render alone,
// or a queue drained/replaced while its period was in flight, cannot do so.
func (p *Player) handedCue(mark *cueMark, at time.Time) {
	if mark == nil { return }
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cue != mark { return }
	p.cue = nil
	mark.cue.finish(at.Add(HardwareTail-time.Duration(mark.after)*time.Second/Rate), nil)
}

// ChimeCue preserves Chime's overlay and waveform, adding a single bounded
// observation of its established audible prefix. Other feedback is unchanged.
func (p *Player) ChimeCue(level float64, audible time.Duration, notes ...Note) *Cue {
	cue := &Cue{done: make(chan struct{})}
	if pb, _ := p.device(); pb == nil {
		cue.finish(time.Time{}, errors.New("speaker cue playback unavailable"))
		return cue
	}
	if p.sink.Load() != nil {
		// The configured sound still plays, but a local card write cannot prove
		// when an external sink has handed it off. Never wait on a false mark.
		p.Chime(level, notes...)
		cue.finish(time.Time{}, errors.New("speaker cue completion unavailable for external sink"))
		return cue
	}
	out := chimeSamples(level, notes...)
	count := min(len(out), int(audible*time.Duration(Rate)/time.Second)*Channels)
	if count <= 0 { cue.finish(time.Time{}, errors.New("speaker cue has no audible samples")); return cue }
	p.mu.Lock()
	defer p.mu.Unlock()
	if pb, _ := p.device(); pb == nil {
		cue.finish(time.Time{}, errors.New("speaker cue playback unavailable"))
		return cue
	}
	p.retireCueLocked()
	p.overlayLocked(out)
	p.cue = &cueMark{cue: cue, remaining: count}
	return cue
}

func (d *Driver) ChimeCue(notes []Note, audible time.Duration) *Cue {
	if d == nil || len(notes) == 0 { return nil }
	return d.p.ChimeCue(toneLevel, audible, notes...)
}
