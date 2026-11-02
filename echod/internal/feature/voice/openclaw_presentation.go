package voice

import (
	"github.com/HuskerMinion/techo5/echod/internal/hardware/speaker"
	"time"
)

type streamChunk struct {
	data        []byte
	speech      bool
	captionMark captionMark
}

type nativeSpan struct {
	end    int64 // input samples at the native 24 kHz rate
	speech bool
}

// Native speech markers label the audio; they never end its stream or a turn.
// The local queue and hardware tail determine when that label reaches the room.
// A dry queue returns to processing without canceling the session or follow-up.
type nativePresentation struct {
	received, submitted int64
	spans               []nativeSpan
	emptyAt             time.Time
	last                bool
}

func (p *nativePresentation) add(samples int, speech bool) {
	p.received += int64(samples)
	if n := len(p.spans); n > 0 && p.spans[n-1].speech == speech {
		p.spans[n-1].end = p.received
	} else {
		p.spans = append(p.spans, nativeSpan{p.received, speech})
	}
}

func (p *nativePresentation) poll(now time.Time, queued int, emit func(bool)) {
	if emit == nil {
		return
	}
	tail := speaker.HardwareTail
	if queued > 0 {
		p.emptyAt = time.Time{}
	} else {
		if p.emptyAt.IsZero() {
			p.emptyAt = now
		}
		tail = max(tail-now.Sub(p.emptyAt), 0)
	}
	heard := p.submitted - int64(queued)*clawOutputRate/speaker.Rate - int64(tail)*clawOutputRate/int64(time.Second)
	on := false
	if heard >= 0 && heard < p.submitted {
		for len(p.spans) > 0 && p.spans[0].end <= heard {
			p.spans = p.spans[1:]
		}
		if len(p.spans) > 0 {
			on = p.spans[0].speech
		}
	}
	if on != p.last {
		p.last = on
		emit(on)
	}
}
