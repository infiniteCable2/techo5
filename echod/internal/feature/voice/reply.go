package voice

import (
	"context"
	"log/slog"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/feature/media"
	"github.com/HuskerMinion/techo5/echod/internal/feature/phone"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/speaker"
	"github.com/HuskerMinion/techo5/echod/internal/lib/audio"
)

// replyResume is how much to hold after a dip, rather than the whole buffer again: rebuilding it every
// time turns a shortfall of a few milliseconds into a third of a second of silence.
const replyResume = 80 * time.Millisecond

// chunkQueue is how many chunks may wait for the errand. Home Assistant sends 32 ms at a time, so this
// is a couple of seconds of slack: enough that the conversation never waits to hand one over, and
// bounded because audio nobody has played yet is audio arriving faster than it can be heard.
const chunkQueue = 64

type streamPlayer interface {
	Queued() int
	PlayVoice([]int16)
	Play([]int16)
}

// stream is a reply arriving over the API, chunk by chunk, played as it goes. It belongs to the errand
// that plays it: the conversation writes chunks and reads the counters once the claim is done.
type stream struct {
	chunks       chan streamChunk
	audible      func(bool) // native ring feedback, owned by the playback goroutine
	presentation nativePresentation
	caption      func(captionMark)
	captionClock captionPlaybackClock

	// buffer is how much to collect before playing any of it. Home Assistant paces itself to stay 384
	// ms ahead, which is enough when speech arrives faster than it plays and not when it does not.
	buffer time.Duration
	rate   int

	// Native Talk stays at 24 kHz until this stateful conversion directly to
	// the speaker's 48 kHz. The separate telephone branch alone needs 16 kHz.
	output *audio.Converter
	phone  *audio.Converter

	// held is audio waiting for the cushion to fill, and flowing says the cushion is built and audio
	// is going straight through.
	held    []int16
	flowing bool

	bytes       int
	peak        int
	splicesAt   uint64
	underrunsAt uint64

	// ended is done called: send and done are the conversation's, on its one goroutine.
	ended bool

	// energy and samples are the reply's loudness so far, for the speech normalizer in flush.
	energy  float64
	samples int

	// playingAt is when playback began, once the cushion had filled. Wall clock from here to the queue
	// running dry is what measures gapping; a starved buffer is silence the seam count never sees.
	playingAt time.Time
}

func newStream(splices, underruns uint64, buffer time.Duration) *stream {
	return newStreamRate(splices, underruns, buffer, speaker.VoiceRate)
}

func newStreamRate(splices, underruns uint64, buffer time.Duration, rate int) *stream {
	s := &stream{
		chunks:      make(chan streamChunk, chunkQueue),
		buffer:      buffer,
		rate:        rate,
		splicesAt:   splices,
		underrunsAt: underruns,
	}
	if rate != speaker.VoiceRate {
		s.output = audio.NewConverter(rate, speaker.Rate, 1)
		s.phone = audio.NewConverter(rate, speaker.VoiceRate, 1)
	}
	return s
}

// send hands a chunk to whoever is playing it. A queue this deep only fills if audio is arriving
// faster than it plays, in which case a dropped chunk is the least of it. A chunk after the end is
// dropped: the queue is closed, and a send on it would end the conversation.
func (s *stream) send(data []byte) {
	if s.ended {
		return
	}
	select {
	case s.chunks <- streamChunk{data: data}:
	default:
		slog.Warn("reply chunk dropped", "bytes", len(data))
	}
}

// trySend lets an unpaced transport fail explicitly instead of losing speech.
// Only the conversation goroutine calls it, like send and done.
func (s *stream) trySend(data []byte) bool {
	return s.trySendNative(data, false)
}

func (s *stream) trySendNative(data []byte, speech bool) bool {
	return s.trySendNativeMarked(data, speech, captionMark{})
}

func (s *stream) trySendNativeMarked(data []byte, speech bool, mark captionMark) bool {
	if s.ended {
		return false
	}
	select {
	case s.chunks <- streamChunk{data: data, speech: speech, captionMark: mark}:
		return true
	default:
		return false
	}
}

// done says no more chunks are coming, which is what ends the errand; once.
func (s *stream) done() {
	if s.ended {
		return
	}
	s.ended = true
	close(s.chunks)
}

// play is the errand: it takes chunks until there are no more and queues them once enough has arrived
// to play through a dip. Returning hands over to the driver, which waits for the audio to be heard.
func (s *stream) play(ctx context.Context, p streamPlayer, started func()) error {
	var watch <-chan time.Time
	if s.audible != nil {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		watch = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-watch:
			s.presentation.poll(now, p.Queued(), s.audible)
			s.pollCaption(now, p)
		case data, ok := <-s.chunks:
			if !ok {
				// Whatever is still held will never be joined by more, so it just plays.
				s.flush(p)
				if s.audible != nil {
					// Keep the short reply's ring following the queue too. No new
					// capture/follow-up opens while this tail is still audible.
					for {
						select {
						case <-ctx.Done():
							return nil
						case now := <-watch:
							queued := p.Queued()
							s.presentation.poll(now, queued, s.audible)
							s.pollCaption(now, p)
							if queued == 0 && !s.presentation.emptyAt.IsZero() && now.Sub(s.presentation.emptyAt) >= speaker.HardwareTail {
								return nil
							}
						}
					}
				}
				return nil
			}
			if s.audible != nil {
				s.presentation.add(len(data.data)/2, data.speech)
			}
			if s.caption != nil {
				s.captionClock.add(len(data.data)/2, data.captionMark)
			}
			s.take(data.data, p, started)
		}
	}
}

func (s *stream) take(data []byte, p streamPlayer, started func()) {
	samples := make([]int16, len(data)/2)
	peak := 0
	for i := range samples {
		samples[i] = int16(uint16(data[i*2]) | uint16(data[i*2+1])<<8)
		peak = max(peak, int(samples[i]), -int(samples[i]))
	}

	// The first chunk is the pipeline delivering, so its limit is done with.
	if s.bytes == 0 {
		slog.Info("reply audio started", "bytes", len(data), "rate_hz", s.rate, "buffer_ms", s.buffer.Milliseconds())
		started()
	}

	// How loud the reply arrives decides whether interpolating it can overflow, and the byte count
	// against how long it takes to say is how a wrong sample rate would show up.
	s.bytes += len(data)
	s.peak = max(s.peak, peak)
	s.energy += media.SumSquares(samples)
	s.samples += len(samples)

	s.held = append(s.held, samples...)
	if p.Queued() == 0 {
		s.flowing = false
	}

	want := s.buffer
	if !s.playingAt.IsZero() {
		want = replyResume
	}
	if !s.flowing && len(s.held) < s.heldSamples(want) {
		return
	}

	if s.playingAt.IsZero() {
		s.playingAt = time.Now()
	}
	s.flowing = true
	s.flush(p)
}

// flush queues what is held, brought toward the speech loudness target. The gain follows the
// reply's loudness so far, so the first flush — which waits for a buffer's worth — already has
// something to measure, and later flushes move it only as the reply's level changes.
func (s *stream) flush(p streamPlayer) {
	out := s.held
	s.held = nil
	if len(out) == 0 {
		return
	}
	if s.audible != nil {
		s.presentation.submitted += int64(len(out))
	}
	if s.caption != nil {
		s.captionClock.submitted += int64(len(out))
	}
	// Short answers may finish before the initial cushion fills. They still
	// need a playback timestamp for duration/gap diagnostics.
	if s.playingAt.IsZero() {
		s.playingAt = time.Now()
	}
	if gain := media.SpeechGain(s.energy, s.samples); gain > 1 {
		out = media.Scale(out, gain)
	}
	// A reply to a turn during a call goes to the far end too, with Assistant replies go into calls on.
	if phone.RepliesIntoCall() {
		call := out
		if s.phone != nil {
			call = s.phone.Run(out, nil)
		}
		phone.Get().Say(call)
	}
	if s.output == nil {
		p.PlayVoice(out)
		return
	}
	mono := s.output.Run(out, make([]int16, 0, len(out)*speaker.Rate/s.rate))
	stereo := make([]int16, 0, len(mono)*speaker.Channels)
	for _, v := range mono {
		stereo = append(stereo, v, v)
	}
	p.Play(stereo)
}

func (s *stream) heldSamples(d time.Duration) int {
	return int(d/time.Millisecond) * s.rate / 1000
}

func (s *stream) pollCaption(now time.Time, p streamPlayer) {
	if s.caption == nil {
		return
	}
	latency := speaker.DefaultLatency
	if source, ok := p.(interface{ Latency() time.Duration }); ok {
		latency = source.Latency()
	}
	s.captionClock.poll(now, p.Queued(), latency, s.caption)
}
