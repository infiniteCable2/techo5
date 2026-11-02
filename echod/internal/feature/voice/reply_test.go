package voice

import (
	"encoding/binary"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/speaker"
)

type replyTestPlayer struct {
	queued int
	pcm    []int16
	voice  []int16
}

func (p *replyTestPlayer) Queued() int { return p.queued }
func (p *replyTestPlayer) Play(pcm []int16) {
	p.pcm = append(p.pcm, pcm...)
	p.queued += len(pcm) / speaker.Channels
}
func (p *replyTestPlayer) PlayVoice(pcm []int16) {
	p.voice = append(p.voice, pcm...)
	p.queued += len(pcm) * speaker.VoiceUpsample
}

func replyTone(rate, frames int, hz float64) []byte {
	pcm := make([]byte, frames*2)
	for i := range frames {
		v := int16(18000 * math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(v))
	}
	return pcm
}

// Exercise the same flush that queues the actual speaker, rather than just the
// converter: a 9 kHz tone must survive, with equal stereo and unchanged duration.
func TestNativeReplyPreservesTrebleAndDurationAcrossUnevenChunks(t *testing.T) {
	config.Use(filepath.Join(t.TempDir(), "state.json"))
	input := replyTone(clawOutputRate, clawOutputRate, 9000)
	play := func(chunks []int) []int16 {
		s := newStreamRate(0, 0, 0, clawOutputRate)
		p := &replyTestPlayer{}
		at := 0
		for _, frames := range chunks {
			s.take(input[2*at:2*(at+frames)], p, func() {})
			at += frames
		}
		s.flush(p)
		if len(p.voice) != 0 || len(p.pcm) != speaker.Rate*speaker.Channels {
			t.Fatalf("native reply used 16 kHz or changed duration: voice=%d pcm=%d", len(p.voice), len(p.pcm))
		}
		return p.pcm
	}
	whole := play([]int{clawOutputRate})
	pieces := play([]int{1, 7, 960, 3, 480, clawOutputRate - 1451})
	if !reflect.DeepEqual(whole, pieces) {
		t.Fatal("chunk boundaries reset the resampler or changed speech")
	}
	var re, im float64
	frames := len(pieces) / speaker.Channels
	const skip = 1024
	for i := skip; i < frames; i++ {
		if pieces[2*i] != pieces[2*i+1] {
			t.Fatal("mono reply was not duplicated to both speaker channels")
		}
		phase := 2 * math.Pi * 9000 * float64(i) / speaker.Rate
		re += float64(pieces[2*i]) * math.Cos(phase)
		im += float64(pieces[2*i]) * math.Sin(phase)
	}
	amp := 2 * math.Hypot(re, im) / float64(frames-skip)
	if amp < 0.85*18000 || amp > 1.05*18000 {
		t.Fatalf("9 kHz treble lost or distorted: amplitude %.1f", amp)
	}
}

func TestNativeReplyBuffersByTimeAndRecoversAfterQueueDrain(t *testing.T) {
	config.Use(filepath.Join(t.TempDir(), "state.json"))
	s := newStreamRate(0, 0, 120*time.Millisecond, clawOutputRate)
	p := &replyTestPlayer{}
	chunk := replyTone(clawOutputRate, 480, 1000) // 20 ms at 24 kHz
	for range 5 {
		s.take(chunk, p, func() {})
	}
	if len(p.pcm) != 0 {
		t.Fatal("24 kHz samples filled a 16 kHz-sized cushion too early")
	}
	s.take(chunk, p, func() {})
	if len(p.pcm) != 120*speaker.Rate*speaker.Channels/1000 {
		t.Fatal("120 ms cushion did not play at its original duration")
	}
	p.queued = 0 // A network stall drained the player.
	before := len(p.pcm)
	for range 3 {
		s.take(chunk, p, func() {})
	}
	if len(p.pcm) != before {
		t.Fatal("underrun recovery did not rebuild its 80 ms cushion")
	}
	s.take(chunk, p, func() {})
	if len(p.pcm)-before != 80*speaker.Rate*speaker.Channels/1000 {
		t.Fatal("underrun recovery lost audio or used the wrong sample rate")
	}
}

func TestShortNativeReplyFlushesTailAndNewReplyHasFreshConverter(t *testing.T) {
	config.Use(filepath.Join(t.TempDir(), "state.json"))
	pcm := replyTone(clawOutputRate, 480, 1000)
	play := func() []int16 {
		s := newStreamRate(0, 0, 650*time.Millisecond, clawOutputRate)
		p := &replyTestPlayer{}
		s.take(pcm, p, func() {})
		if len(p.pcm) != 0 {
			t.Fatal("short reply ignored initial buffering")
		}
		s.done()
		s.flush(p)
		if len(p.pcm) != 960*speaker.Channels {
			t.Fatal("end of short reply discarded the buffered tail")
		}
		if s.playingAt.IsZero() {
			t.Fatal("short reply was omitted from playback duration diagnostics")
		}
		return p.pcm
	}
	if !reflect.DeepEqual(play(), play()) {
		t.Fatal("a new reply inherited an earlier converter's history")
	}
}

func TestLegacyReplyStillUsesSixteenKHzVoicePlayer(t *testing.T) {
	config.Use(filepath.Join(t.TempDir(), "state.json"))
	s := newStream(0, 0, 0)
	p := &replyTestPlayer{}
	s.take(replyTone(speaker.VoiceRate, 320, 1000), p, func() {})
	if s.rate != speaker.VoiceRate || len(p.voice) != 320 || len(p.pcm) != 0 {
		t.Fatal("legacy reply path was changed by native Talk playback")
	}
}
