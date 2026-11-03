package voice

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/feature/wakeword"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/mic"
)

// Only the acoustic leaf is synthetic. The real stream consumes its atomic
// history/subscription boundary, VAD and framing, then the native Gateway adapter.
type numberedCapture struct {
	*mic.Source
	frames     chan []int16
	pre        []int16
	mu         sync.Mutex
	history    time.Duration
	at         time.Time
	subscribed chan struct{}
}

func (s *numberedCapture) ListenSince(name string, after time.Time) (<-chan []int16, []int16, func(), error) {
	age := s.at.Sub(after)
	if age < 0 || age > mic.Confirming {
		return nil, nil, nil, errors.New("synthetic history unavailable")
	}
	frames, pre, stop := s.ListenHistory(name, age)
	return frames, pre, stop, nil
}

func (s *numberedCapture) ListenHistory(_ string, d time.Duration) (<-chan []int16, []int16, func()) {
	s.mu.Lock()
	s.history = d
	s.mu.Unlock()
	n := min(len(s.pre), int(d*time.Duration(mic.Rate)/time.Second))
	pre := append([]int16(nil), s.pre[len(s.pre)-n:]...)
	close(s.subscribed)
	return s.frames, pre, func() {}
}

type observedCaptureCue struct {
	entered, ready chan struct{}
	end            time.Time
}

func (c *observedCaptureCue) Wait(ctx context.Context) (time.Time, error) {
	close(c.entered)
	select {
	case <-ctx.Done():
		return time.Time{}, ctx.Err()
	case <-c.ready:
		return c.end, nil
	}
}

func numberedPCM(values ...int16) []int16 {
	var pcm []int16
	for _, value := range values {
		for range nativeInputFrameSamples {
			pcm = append(pcm, value)
		}
	}
	return pcm
}

func TestNativeStreamKeepsFirstPostCueSamplesAfterLateStartup(t *testing.T) {
	for _, mode := range []string{"wake", "followup", "silent-followup"} {
		t.Run(mode, func(t *testing.T) {
			c := thinkingTurn(t)
			if err := config.Set().Wake(0).Tone(config.ToneHA); err != nil {
				t.Fatal(err)
			}
			if mode == "silent-followup" {
				if err := config.Set().Wake(0).FollowUpTone(config.ToneNone); err != nil {
					t.Fatal(err)
				}
			}
			followUp := mode != "wake"
			f := &fakeClaw{ready: true, supportsEnd: true, opened: make(chan struct{}), ended: make(chan struct{}), closed: make(chan struct{})}
			b, _ := newTestClaw(t, f)
			if err := b.Start(""); err != nil {
				t.Fatal(err)
			}
			capture := &numberedCapture{Source: mic.New(), frames: make(chan []int16), pre: numberedPCM(-101, -102, 201, 202), subscribed: make(chan struct{})}
			c.source, c.be, c.claw = capture, b, b
			c.clawSends = &clawSendScope{active: true}
			// The cue-owned safe boundary is .500, setup finishes .540. Last
			// 40 ms of history are user audio; older signal/reply must stay out.
			end := time.Unix(100, 500*int64(time.Millisecond))
			c.now = func() time.Time { return end.Add(40 * time.Millisecond) }
			capture.at = c.now()
			c.opened = end
			var cue captureCue
			observed := &observedCaptureCue{entered: make(chan struct{}), ready: make(chan struct{}), end: end}
			if wakeword.Tones(0, followUp) {
				cue = observed
			}
			ctx, cancel := context.WithCancel(context.Background())
			senderDone, streamDone := make(chan struct{}), make(chan struct{})
			go func() { defer close(senderDone); c.sends(ctx) }()
			go func() {
				defer close(streamDone)
				c.stream(ctx, b, 0, followUp, false, "synthetic-input", 0, c.clawSends, cue, end)
			}()
			t.Cleanup(func() { c.clawSends.retire(); cancel(); <-streamDone; <-senderDone })
			if cue != nil {
				waitClaw(t, observed.entered)
				select {
				case <-capture.subscribed:
					t.Fatal("capture ignored its speaker-owned cue boundary")
				default:
				}
				close(observed.ready)
			}
			waitClaw(t, capture.subscribed)
			capture.mu.Lock()
			history := capture.history
			capture.mu.Unlock()
			if history != 40*time.Millisecond {
				t.Fatal("history was not anchored after the cue/admission", history)
			}
			for _, value := range []int16{203, 204} {
				select {
				case capture.frames <- numberedPCM(value):
				case <-streamDone:
					t.Fatal("capture ended early")
				case <-time.After(time.Second):
					t.Fatal("capture did not consume input")
				}
			}
			close(capture.frames)
			waitClaw(t, streamDone)
			barrier := make(chan struct{})
			c.sendClaw(c.clawSends, "capture barrier", func() error { close(barrier); return nil })
			waitClaw(t, barrier)
			// Capture remains buffered through the ordinary delayed create/ACK.
			close(f.opened)
			waitClaw(t, f.ended)
			f.mu.Lock()
			calls := append([]clawCall(nil), f.calls...)
			f.mu.Unlock()
			var pcm []int16
			ends := 0
			for _, call := range calls {
				if call.method != "talk.session.appendAudio" {
					continue
				}
				p := call.params
				if p["endOfInput"] == true {
					ends++
					continue
				}
				if ends != 0 {
					t.Fatal("PCM followed its end")
				}
				bytes, err := base64.StdEncoding.DecodeString(p["audioBase64"].(string))
				if err != nil || len(bytes) != nativeInputFrameSamples*2 {
					t.Fatal("invalid PCM", err)
				}
				for i := 0; i < len(bytes); i += 2 {
					pcm = append(pcm, int16(uint16(bytes[i])|uint16(bytes[i+1])<<8))
				}
			}
			if ends != 1 || !reflect.DeepEqual(pcm, numberedPCM(201, 202, 203, 204)) {
				t.Fatalf("post-cue prefix changed or feedback replayed: samples=%d ends=%d", len(pcm), ends)
			}
			_ = b.Stop()
			waitClaw(t, f.closed)
		})
	}
}

func TestNativeStreamRejectsUnrecoverablePostCueHistory(t *testing.T) {
	c := thinkingTurn(t)
	b := newClaw(func(event) {})
	end := time.Unix(100, 0)
	c.now = func() time.Time { return end.Add(3 * time.Second) }
	c.source = &numberedCapture{Source: mic.New(), frames: make(chan []int16), subscribed: make(chan struct{}), at: c.now()}
	ready := make(chan struct{})
	close(ready)
	cue := &observedCaptureCue{entered: make(chan struct{}), ready: ready, end: end}
	c.stream(context.Background(), b, 0, false, false, "late-input", 0, &clawSendScope{active: true}, cue, end)
	e := <-c.events
	if e.kind != evError || e.code != "openclaw-capture-history" {
		t.Fatal("lost onset was not reported explicitly", e)
	}
}

func TestNativeStreamCancellationWhileCuePendingDoesNotOpenCapture(t *testing.T) {
	c := thinkingTurn(t)
	capture := &numberedCapture{Source: mic.New(), frames: make(chan []int16), subscribed: make(chan struct{})}
	c.source = capture
	b := newClaw(func(event) {})
	cue := &observedCaptureCue{entered: make(chan struct{}), ready: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.stream(ctx, b, 0, false, false, "cancelled-input", 0, &clawSendScope{active: true}, cue, time.Time{})
	}()
	waitClaw(t, cue.entered)
	cancel()
	waitClaw(t, done)
	select {
	case <-capture.subscribed:
		t.Fatal("cancelled cue opened capture")
	default:
	}
}
