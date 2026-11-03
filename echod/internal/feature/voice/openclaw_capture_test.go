package voice

import (
	"context"
	"encoding/base64"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/mic"
)

// Only the acoustic leaf is synthetic. The real stream consumes its atomic
// history/subscription boundary, VAD and framing, then the native Gateway adapter.
type numberedCapture struct {
	*mic.Source
	frames chan []int16
	pre []int16
	mu sync.Mutex
	history time.Duration
	subscribed chan struct{}
}

func (s *numberedCapture) ListenHistory(_ string, d time.Duration) (<-chan []int16, []int16, func()) {
	s.mu.Lock()
	s.history = d
	s.mu.Unlock()
	var pre []int16
	if d > 0 { pre = append([]int16(nil), s.pre...) }
	close(s.subscribed)
	return s.frames, pre, func() {}
}

func numberedPCM(values ...int16) []int16 {
	var pcm []int16
	for _, value := range values {
		for range nativeInputFrameSamples { pcm = append(pcm, value) }
	}
	return pcm
}

func TestNativeStreamPreservesImmediateCaptureWithoutReplayingReplyHistory(t *testing.T) {
	for _, followUp := range []bool{false, true} {
		t.Run(map[bool]string{false: "wake", true: "followup"}[followUp], func(t *testing.T) {
			c := thinkingTurn(t)
			if err := config.Set().Wake(0).Tone(config.ToneHA); err != nil { t.Fatal(err) }
			// An absent follow-up override inherits the configured wake tone.
			f := &fakeClaw{ready: true, supportsEnd: true, opened: make(chan struct{}), ended: make(chan struct{}), closed: make(chan struct{})}
			b, _ := newTestClaw(t, f)
			if err := b.Start(""); err != nil { t.Fatal(err) }
			capture := &numberedCapture{Source: mic.New(), frames: make(chan []int16), pre: numberedPCM(-101, -102), subscribed: make(chan struct{})}
			c.source, c.be, c.claw = capture, b, b
			c.clawSends = &clawSendScope{active: true}
			c.now = func() time.Time { return time.Unix(100, 0) }
			ctx, cancel := context.WithCancel(context.Background())
			senderDone := make(chan struct{})
			go func() { defer close(senderDone); c.sends(ctx) }()
			streamDone := make(chan struct{})
			go func() { defer close(streamDone); c.stream(ctx, b, 0, followUp, false, "synthetic-input", 0, c.clawSends) }()
			t.Cleanup(func() { c.clawSends.retire(); cancel(); <-streamDone; <-senderDone })
			waitClaw(t, capture.subscribed)
			for _, value := range []int16{201, 202, 203} {
				select {
				case capture.frames <- numberedPCM(value):
				case <-streamDone: t.Fatal("capture ended before immediate input")
				case <-time.After(time.Second): t.Fatal("capture did not consume input")
				}
			}
			close(capture.frames)
			waitClaw(t, streamDone)
			// All capture must be retained while the ordinary create RPC is held.
			barrier := make(chan struct{})
			c.sendClaw(c.clawSends, "capture barrier", func() error { close(barrier); return nil })
			waitClaw(t, barrier)
			close(f.opened)
			waitClaw(t, f.ended)
			f.mu.Lock()
			calls := append([]clawCall(nil), f.calls...)
			f.mu.Unlock()
			var pcm []int16
			ends := 0
			for _, call := range calls {
				if call.method != "talk.session.appendAudio" { continue }
				p := call.params
				if p["endOfInput"] == true { ends++; continue }
				if ends != 0 { t.Fatal("PCM followed the end marker") }
				bytes, err := base64.StdEncoding.DecodeString(p["audioBase64"].(string))
				if err != nil || len(bytes) != nativeInputFrameSamples*2 { t.Fatal("invalid PCM frame", err) }
				for i := 0; i < len(bytes); i += 2 { pcm = append(pcm, int16(uint16(bytes[i]) | uint16(bytes[i+1])<<8)) }
			}
			want := numberedPCM(201, 202, 203)
			if !followUp { want = append(numberedPCM(-101, -102), want...) }
			if ends != 1 || !reflect.DeepEqual(pcm, want) { t.Fatalf("capture prefix/history changed: got %d samples, want %d; ends=%d", len(pcm), len(want), ends) }
			_ = b.Stop()
			waitClaw(t, f.closed)
		})
	}
}
