package mic

import (
	"errors"
	"sync"
	"time"
)

// History is how much recent audio a turn sends before its live frames. Detection can only fire
// once the wake word has been spoken, and speech runs straight on into the request, so without this
// the first syllables of "turn on the kitchen light" are gone before anything is streaming.
//
// It is a compromise in both directions: too little and the first word of the request is missing,
// too much and the tail of the wake word is transcribed as part of the request, which can stop it
// matching an intent.
const History = 250 * time.Millisecond

// Confirming is how much a wake the device was not sure of sends ahead of it when Home Assistant is asked
// to confirm it: the whole word and what led up to it, which is what a wake word engine needs to hear to
// score the word at all.
const Confirming = 2 * time.Second

// The ring holds the longest of these, whatever is being sent of it.
const historySamples = 2 * Rate

// history is a ring of the most recently captured mono samples.
type history struct {
	mu     sync.Mutex
	buf    [historySamples]int16
	at     int
	filled bool
	written uint64
	spans [historySamples/FrameSamples + 1]captureSpan
	spanAt, spanCount int
	resetAt time.Time
}

// These are read-completion estimates, not hardware ADC timestamps. Keeping
// whole frame spans avoids assigning old samples the time of a later snapshot.
type captureSpan struct {
	first uint64
	count int
	end time.Time
	broken bool
}

func (p captureSpan) start() time.Time {
	return p.end.Add(-time.Duration(p.count)*time.Second/Rate)
}

func (h *history) add(frame []int16) {
	h.addAt(frame, time.Now())
}

func (h *history) addAt(frame []int16, end time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(frame) == 0 { return }
	p := captureSpan{first: h.written, count: len(frame), end: end}
	if h.spanCount > 0 {
		previous := h.spans[(h.spanAt+len(h.spans)-1)%len(h.spans)]
		// Read scheduling is only frame-granular. Larger holes, or a clock
		// reversal, cannot certify a continuous post-cue prefix.
		p.broken = !end.After(previous.end) || p.start().Sub(previous.end) > time.Second/50
	}
	h.spans[h.spanAt] = p
	h.spanAt = (h.spanAt+1)%len(h.spans)
	h.spanCount = min(h.spanCount+1, len(h.spans))

	for _, s := range frame {
		h.buf[h.at] = s
		h.at++
		if h.at == len(h.buf) {
			h.at, h.filled = 0, true
		}
	}
	h.written += uint64(len(frame))
}

// A hardware restart/overrun invalidates timing coverage, but does not change
// the legacy duration-based PCM history contract.
func (h *history) discontinue(at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.spanAt, h.spanCount, h.resetAt = 0, 0, at
}

// recent returns up to d of the most recent audio, oldest first.
func (h *history) recent(d time.Duration) []int16 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.recentLocked(d)
}

func (h *history) recentLocked(d time.Duration) []int16 {
	want := int(d/time.Millisecond) * Rate / 1000

	have := len(h.buf)
	if !h.filled {
		have = h.at
	}
	want = min(want, have)

	out := make([]int16, 0, want)
	// Walk back from the write position, wrapping, then hand it over oldest first.
	start := h.at - want
	if start < 0 {
		start += len(h.buf)
	}
	for i := range want {
		out = append(out, h.buf[(start+i)%len(h.buf)])
	}
	return out
}

func (h *history) since(after time.Time) ([]int16, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	age := time.Since(after)
	if age < 0 || age > Confirming {
		return nil, errors.New("microphone capture boundary is outside retained history")
	}
	if after.Before(h.resetAt) { return nil, errors.New("microphone capture boundary precedes capture restart") }
	if h.spanCount == 0 { return nil, nil }
	oldest := h.written-min(h.written, uint64(len(h.buf)))
	var out []int16
	covered := false
	for i := range h.spanCount {
		p := h.spans[(h.spanAt-h.spanCount+i+len(h.spans))%len(h.spans)]
		if p.first < oldest { continue }
		if !covered {
			if after.Before(p.start()) { return nil, errors.New("microphone capture boundary precedes retained frames") }
			covered = true
		}
		if p.broken && after.Before(p.start()) { return nil, errors.New("microphone capture boundary crosses a capture gap") }
		// The one frame straddling the cue boundary is deliberately omitted.
		if p.start().Before(after) { continue }
		for j := range p.count {
			out = append(out, h.buf[int((p.first+uint64(j))%uint64(len(h.buf)))])
		}
	}
	if !covered { return nil, errors.New("microphone capture boundary has no retained frame coverage") }
	return out, nil
}

func (s *Source) remember(frame []int16, at time.Time) { s.history.addAt(frame, at) }

// Recent returns up to d of the audio captured before now, oldest first. A turn sends this ahead of
// its live frames so the speech that ran on from the wake word is not lost.
func (s *Source) Recent(d time.Duration) []int16 { return s.history.recent(d) }

// ListenHistory subscribes and copies up to d of recent audio at the same capture
// boundary. The first live frame follows the history without a gap or overlap.
// Live delivery has the same bounded queue and drop policy as Listen.
func (s *Source) ListenHistory(name string, d time.Duration) (<-chan []int16, []int16, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	frames, unlisten := s.listenLocked(name, time.Time{})
	var pre []int16
	if d > 0 {
		pre = s.Recent(d)
	}
	return frames, pre, unlisten
}

// ListenSince selects an absolute capture boundary inside the same locked
// snapshot that subscribes. Waiting for capture/AEC cannot move the boundary.
// The ring retains only two seconds; older onsets are explicitly unavailable.
// Selection is conservative to a whole 20ms frame; a straddling frame is omitted.
func (s *Source) ListenSince(name string, after time.Time) (<-chan []int16, []int16, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pre, err := s.history.since(after)
	if err != nil { return nil, nil, nil, err }
	frames, unlisten := s.listenLocked(name, after)
	return frames, pre, unlisten, nil
}
