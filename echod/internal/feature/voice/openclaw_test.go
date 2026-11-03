package voice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	gw "github.com/HuskerMinion/techo5/echod/internal/lib/openclaw"
)

type clawCall struct {
	method string
	params map[string]any
}
type fakeClaw struct {
	mu               sync.Mutex
	calls            []clawCall
	opened           chan struct{}
	ended            chan struct{}
	closed           chan struct{}
	ready            bool
	supportsEnd      bool
	activityEvents   bool
	inputEndMode     string
	omitInputEndMode bool
	events           chan gw.Event
}

func (f *fakeClaw) Ready() bool             { return f.ready }
func (f *fakeClaw) Events() <-chan gw.Event { return f.events }
func (f *fakeClaw) Call(ctx context.Context, method string, params any, result any) error {
	f.mu.Lock()
	f.calls = append(f.calls, clawCall{method, params.(map[string]any)})
	f.mu.Unlock()
	if method == "talk.session.create" {
		if f.opened != nil {
			select {
			case <-f.opened:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		mode := f.inputEndMode
		if mode == "" {
			mode = "client"
		}
		audio := map[string]any{
			"inputEncoding": "pcm16", "inputSampleRateHz": 16000, "outputEncoding": "pcm16", "outputSampleRateHz": 24000, "supportsInputEnd": f.supportsEnd, "activityEvents": f.activityEvents, "inputEndMode": mode,
		}
		if f.omitInputEndMode {
			delete(audio, "inputEndMode")
		}
		data, _ := json.Marshal(map[string]any{"sessionId": "test-session", "readyMark": "media-ready:test", "audio": audio})
		return json.Unmarshal(data, result)
	}
	if method == "talk.session.appendAudio" && params.(map[string]any)["endOfInput"] == true {
		close(f.ended)
	}
	if method == "talk.session.close" {
		close(f.closed)
	}
	return nil
}
func newTestClaw(t *testing.T, f *fakeClaw) (*clawBackend, chan event) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	posted := make(chan event, 20)
	b := newClaw(func(e event) { posted <- e })
	b.playbackReady = func() bool { return true }
	b.client, b.ctx, b.key = f, ctx, "agent:example:speech:device-test"
	t.Cleanup(func() { cancel(); _ = b.Stop() })
	return b, posted
}
func waitClaw(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("OpenClaw worker did not settle")
	}
}

func TestClawRetainsRealAudioDuringCreateAndEndsInputWithoutClosingSession(t *testing.T) {
	f := &fakeClaw{ready: true, supportsEnd: true, opened: make(chan struct{}), ended: make(chan struct{}), closed: make(chan struct{})}
	b, _ := newTestClaw(t, f)
	if err := b.Start("unchanged wake"); err != nil {
		t.Fatal(err)
	}
	audio := []byte{1, 2, 3, 4, 5, 6}
	if err := b.Audio(audio); err != nil {
		t.Fatal(err)
	}
	audio[0] = 99 // capture owns/reuses its buffer
	if err := b.End(); err != nil {
		t.Fatal(err)
	}
	if err := b.Audio([]byte{9, 0}); err != nil {
		t.Fatal(err)
	}
	if err := b.End(); err != nil {
		t.Fatal(err)
	}
	close(f.opened)
	waitClaw(t, f.ended)
	f.mu.Lock()
	calls := append([]clawCall(nil), f.calls...)
	f.mu.Unlock()
	var methods []string
	for _, c := range calls {
		methods = append(methods, c.method)
	}
	if !reflect.DeepEqual(methods, []string{"talk.session.create", "talk.session.acknowledgeMark", "talk.session.appendAudio", "talk.session.appendAudio"}) {
		t.Fatal(methods)
	}
	if calls[0].params["transport"] != "gateway-relay" || calls[0].params["mode"] != "stt-tts" || calls[0].params["sessionKey"] != b.key {
		t.Fatal(calls[0])
	}
	pcm, _ := base64.StdEncoding.DecodeString(calls[2].params["audioBase64"].(string))
	if !reflect.DeepEqual(pcm, []byte{1, 2, 3, 4, 5, 6}) {
		t.Fatal(pcm)
	}
	if calls[3].params["audioBase64"] != "" {
		t.Fatal("end marker must not synthesize silence")
	}
	select {
	case <-f.closed:
		t.Fatal("input end closed the Talk session")
	default:
	}
	_ = b.Stop()
	waitClaw(t, f.closed)
}

func TestClawRefusesGatewayWithoutExplicitEndpointBeforeSendingAudio(t *testing.T) {
	f := &fakeClaw{ready: true, closed: make(chan struct{})}
	b, posted := newTestClaw(t, f)
	_ = b.Start("")
	select {
	case e := <-posted:
		if e.code != "openclaw-speech-contract" {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing contract refusal")
	}
	waitClaw(t, f.closed)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.method == "talk.session.appendAudio" || c.method == "talk.session.acknowledgeMark" {
			t.Fatal(c.method)
		}
	}
}

func TestClawFencesReplacedSessionEvents(t *testing.T) {
	f := &fakeClaw{ready: true}
	b, posted := newTestClaw(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.gen = 3
	b.turn = &clawTurn{ctx: ctx, cancel: cancel, generation: 3, id: "current"}
	b.receive(json.RawMessage(`{"sessionId":"old","type":"audio","audioBase64":"AAAAAA=="}`))
	select {
	case <-posted:
		t.Fatal("old session reached playback")
	default:
	}
	b.receive(json.RawMessage(`{"sessionId":"current","type":"transcript","role":"user","final":true,"text":"example"}`))
	if e := <-posted; e.kind != evHeard || e.clawGeneration != 3 {
		t.Fatal(e)
	}
	_ = b.Stop()
	b.receive(json.RawMessage(`{"sessionId":"current","type":"transcript","role":"user","final":true,"text":"example"}`))
	select {
	case <-posted:
		t.Fatal("closed session reached conversation")
	default:
	}
	if err := b.Audio([]byte{0}); err == nil {
		t.Fatal("odd PCM accepted")
	}
	if err := b.Audio([]byte{0, 0}); err == nil {
		t.Fatal("closed capture accepted")
	}
}

func TestClawDoesNotAcknowledgeUnavailablePlayback(t *testing.T) {
	f := &fakeClaw{ready: true, supportsEnd: true, closed: make(chan struct{})}
	b, posted := newTestClaw(t, f)
	b.playbackReady = func() bool { return false }
	if err := b.Start(""); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-posted:
		if e.code != "openclaw-playback-unavailable" {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unavailable sink was not rejected")
	}
	waitClaw(t, f.closed)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.method == "talk.session.acknowledgeMark" {
			t.Fatal("unavailable hardware acknowledged")
		}
	}
}

func TestClawPacesNativeBurstWithoutLosingAudioOrEndingEarly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	posted := make(chan event, 256)
	b := newClaw(func(e event) { posted <- e })
	target := &clawTurn{ctx: ctx, cancel: cancel, generation: 1, outputWake: make(chan struct{}, 1)}
	b.gen, b.turn = 1, target
	// More than the existing player's 64-frame queue, sent immediately by native
	// Talk, must survive the transport and precede the end of the turn.
	for range 70 {
		b.queueOutput(target, event{kind: evStreamAudio, audio: make([]byte, 960)})
	}
	b.queueOutput(target, event{kind: evStreamEnd})
	b.queueOutput(target, event{kind: evRunEnd})
	done := make(chan struct{})
	go func() { defer close(done); b.playback(target) }()
	defer func() { cancel(); <-done }()
	started := time.Now()
	for i := range 72 {
		select {
		case e := <-posted:
			want := evStreamAudio
			if i == 70 {
				want = evStreamEnd
			}
			if i == 71 {
				want = evRunEnd
			}
			if e.kind != want || e.clawPlayback != 1 || e.clawGeneration != 1 {
				t.Fatalf("event %d: %+v", i, e)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("native audio burst was lost")
		}
	}
	if time.Since(started) < time.Second {
		t.Fatal("burst was handed unpaced to the existing player")
	}
}

func TestClawPlaybackClockDoesNotAccumulateSchedulerLateness(t *testing.T) {
	start := time.Unix(100, 0)
	var clock clawPlaybackClock
	for i := range 1000 {
		now := start.Add(time.Duration(i) * 20 * time.Millisecond)
		if i > 0 {
			now = now.Add(2 * time.Millisecond)
		}
		if delay := clock.delay(now); delay != 0 {
			t.Fatalf("frame %d delayed another %v despite being due", i, delay)
		}
		clock.advance(960)
		want := start.Add(time.Duration(i+1) * 20 * time.Millisecond)
		if !clock.next.Equal(want) {
			t.Fatalf("frame %d moved the audio timeline by %v", i, clock.next.Sub(want))
		}
	}
}

func TestClawPlaybackClockBoundsCatchUpAfterLongNetworkGap(t *testing.T) {
	start := time.Unix(100, 0)
	var clock clawPlaybackClock
	clock.delay(start)
	clock.advance(960)
	if got := clock.delay(start.Add(5 * time.Millisecond)); got != 15*time.Millisecond {
		t.Fatal("early frame was not paced", got)
	}
	late := start.Add(60 * time.Millisecond)
	if clock.delay(late) != 0 || !clock.next.Equal(start.Add(20*time.Millisecond)) {
		t.Fatal("a small stall discarded the original timeline")
	}
	late = start.Add(time.Second)
	if clock.delay(late) != 0 || !clock.next.Equal(late) {
		t.Fatal("long gap permitted an unbounded catch-up burst")
	}
	clock.advance(960)
	if got := clock.delay(late); got != 20*time.Millisecond {
		t.Fatal("long-gap recovery did not resume normal pacing", got)
	}
}

func TestClawPreservesNativeOutputSamples(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := newClaw(func(event) {})
	b.turn = &clawTurn{ctx: ctx, generation: 1, id: "current", outputWake: make(chan struct{}, 1)}
	pcm := make([]byte, 960)
	for i := range pcm {
		pcm[i] = byte(i*13 + 7)
	}
	raw, _ := json.Marshal(map[string]any{"sessionId": "current", "type": "audio", "audioBase64": base64.StdEncoding.EncodeToString(pcm)})
	b.receive(raw)
	if len(b.turn.output) != 1 || !reflect.DeepEqual(b.turn.output[0].event.audio, pcm) {
		t.Fatal("native 24 kHz audio was resampled or changed in transport")
	}
}

func TestClawClearRevokesQueuedAndPostedAudio(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := newClaw(func(event) {})
	target := &clawTurn{ctx: ctx, cancel: cancel, generation: 2, outputWake: make(chan struct{}, 1)}
	b.gen, b.turn = 2, target
	b.queueOutput(target, event{kind: evStreamAudio, audio: []byte{1, 0}})
	if !b.currentPlayback(2, 1) {
		t.Fatal("initial output generation absent")
	}
	b.queueOutput(target, event{kind: evClawClear})
	b.queueOutput(target, event{kind: evStreamAudio, audio: []byte{2, 0}})
	if b.currentPlayback(2, 1) || !b.currentPlayback(2, 2) {
		t.Fatal("posted old chunks were not revoked")
	}
	if len(target.output) != 2 || target.output[0].event.kind != evClawClear || target.output[1].event.audio[0] != 2 {
		t.Fatal("clear retained superseded queued speech")
	}
}

func TestClawRejectsOversizedGatewayAudio(t *testing.T) {
	f := &fakeClaw{ready: true}
	b, posted := newTestClaw(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.turn = &clawTurn{ctx: ctx, cancel: cancel, generation: 1, id: "current"}
	pcm := base64.StdEncoding.EncodeToString(make([]byte, 962))
	raw, _ := json.Marshal(map[string]any{"sessionId": "current", "type": "audio", "audioBase64": pcm})
	b.receive(raw)
	if e := <-posted; e.kind != evError || e.code != "openclaw-audio" {
		t.Fatal(e)
	}
	if len(b.turn.output) != 0 {
		t.Fatal("oversized audio entered playback backlog")
	}
}
