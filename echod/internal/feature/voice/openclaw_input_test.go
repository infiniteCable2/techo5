package voice

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"sync"
	"testing"
)

type framedTalkGateway struct {
	*fakeClaw
	frameMu sync.Mutex
	bytes   int
	invalid bool
}

func (f *framedTalkGateway) Call(ctx context.Context, method string, params any, result any) error {
	if method == "talk.session.appendAudio" {
		p := params.(map[string]any)
		pcm, err := base64.StdEncoding.DecodeString(p["audioBase64"].(string))
		if err != nil {
			return err
		}
		f.frameMu.Lock()
		f.bytes += len(pcm)
		if p["endOfInput"] == true && f.bytes%640 != 0 {
			f.invalid = true
			f.frameMu.Unlock()
			return errors.New("complete real audio frames required")
		}
		f.frameMu.Unlock()
	}
	return f.fakeClaw.Call(ctx, method, params, result)
}

func TestNativeCapturePreservesFractionalInputAndOrdersRealTailBeforeEnd(t *testing.T) {
	f := &fakeClaw{ready: true, supportsEnd: true, ended: make(chan struct{}), closed: make(chan struct{})}
	b, _ := newTestClaw(t, f)
	strict := &framedTalkGateway{fakeClaw: f}
	b.client = strict
	if err := b.Start(""); err != nil {
		t.Fatal(err)
	}
	scope := &clawSendScope{active: true}
	c := &conversation{be: b, clawSends: scope, out: make(chan func() error, 512), events: make(chan event, 8), stopAudio: func() {}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.sends(ctx) }()
	t.Cleanup(func() { scope.retire(); cancel(); <-done })
	framed := nativeInputFrames{emit: func(pcm []byte) { c.sendAudio(b, pcm, scope) }}
	// Exact sample count from the real failed capture; intentionally fragmented.
	original := make([]int16, 41182)
	for i := range original {
		original[i] = int16(i*13 + 17)
	}
	for start := 0; start < len(original); {
		n := min([]int{17, 320, 1, 647}[start%4], len(original)-start)
		framed.feed(original[start : start+n])
		start += n
	}
	c.stopStreaming()
	barrier := make(chan struct{})
	c.sendClaw(scope, "capture barrier", func() error { close(barrier); return nil })
	waitClaw(t, barrier)
	b.mu.Lock()
	earlyEnd := b.turn.inputEnded
	b.mu.Unlock()
	if earlyEnd {
		t.Fatal("stopStreaming committed End before the real partial frame was complete")
	}
	tail := make([]int16, 320)
	for i := range tail {
		tail[i] = int16(-1200 - i)
	}
	frames := make(chan []int16, 1)
	frames <- tail
	var observed []int16
	c.finishNativeInput(&framed, frames, b, scope, func(part []int16) { observed = append(observed, part...) })
	waitClaw(t, f.ended)
	if framed.used != 0 || framed.tailSamples != 98 || !reflect.DeepEqual(observed, tail[:98]) {
		t.Fatal("capture did not finish with exactly the missing real samples")
	}
	f.mu.Lock()
	calls := append([]clawCall(nil), f.calls...)
	f.mu.Unlock()
	var received []int16
	ends := 0
	for _, call := range calls {
		if call.method != "talk.session.appendAudio" {
			continue
		}
		pcm, _ := base64.StdEncoding.DecodeString(call.params["audioBase64"].(string))
		if call.params["endOfInput"] == true {
			ends++
			if len(pcm) != 0 {
				t.Fatal("End synthesized audio")
			}
			continue
		}
		if ends > 0 || len(pcm) != 640 {
			t.Fatal("capture frame was partial or followed its input end")
		}
		for i := 0; i < len(pcm); i += 2 {
			received = append(received, int16(uint16(pcm[i])|uint16(pcm[i+1])<<8))
		}
	}
	want := append(append([]int16(nil), original...), tail[:98]...)
	if ends != 1 || !reflect.DeepEqual(received, want) {
		t.Fatal("real capture samples were cut, padded, repeated or reordered")
	}
	strict.frameMu.Lock()
	invalid := strict.invalid
	strict.frameMu.Unlock()
	if invalid {
		t.Fatal("strict Gateway rejected the native input boundary")
	}
}

func TestNativeInputAlignedBoundaryNeedsNoFurtherMicrophoneFrame(t *testing.T) {
	framed := nativeInputFrames{emit: func(pcm []byte) {
		if len(pcm) != 640 {
			t.Fatal("bad frame")
		}
	}}
	framed.feed(make([]int16, 640))
	if err := framed.complete(&clawSendScope{active: true}, nil, func([]int16) { t.Fatal("aligned input requested a tail") }); err != nil {
		t.Fatal(err)
	}
}

func TestNativeInputRetirementCannotFillOrLeakIntoNextTurn(t *testing.T) {
	scope := &clawSendScope{active: true}
	retired := scope.retiredSignal()
	framed := nativeInputFrames{emit: func([]byte) { t.Fatal("retired capture sent audio") }}
	framed.feed([]int16{12})
	frames := make(chan []int16, 1)
	frames <- make([]int16, 320)
	scope.retire()
	scope.retire()
	if err := framed.complete(scope, frames, func([]int16) { t.Fatal("retired capture read new audio") }); !errors.Is(err, errNativeInputRetired) {
		t.Fatal(err)
	}
	if scope.retiredSignal() != retired || len(frames) != 1 {
		t.Fatal("retirement lost its fence or consumed a new frame")
	}
}

func TestNativeInputMissingRealTailFailsWithoutPaddingOrEnd(t *testing.T) {
	scope := &clawSendScope{active: true}
	c := &conversation{out: make(chan func() error, 8), events: make(chan event, 8)}
	framed := nativeInputFrames{emit: func([]byte) { t.Fatal("partial capture was padded") }}
	framed.feed([]int16{1, 2, 3})
	frames := make(chan []int16)
	close(frames)
	c.finishNativeInput(&framed, frames, newClaw(func(event) {}), scope, func([]int16) {})
	if len(c.out) != 0 {
		t.Fatal("missing real tail still queued an input end")
	}
	select {
	case event := <-c.events:
		if event.code != "openclaw-input-frame" || event.clawSend != scope {
			t.Fatal("frame failure lost its capture owner")
		}
	default:
		t.Fatal("missing real tail claimed success")
	}
}
