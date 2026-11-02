package voice

import (
	"context"
	"encoding/base64"
	"testing"
	"testing/synctest"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/feature/wakeword"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/speaker"
)

// Model the sink's backpressure without storing minutes of decoded samples.
type timedReplyPlayer struct{ frames int }

func (p *timedReplyPlayer) Queued() int { return 0 }
func (p *timedReplyPlayer) Play(samples []int16) {
	frames := len(samples) / speaker.Channels
	p.frames += frames
	time.Sleep(time.Duration(frames) * time.Second / speaker.Rate)
}
func (p *timedReplyPlayer) PlayVoice([]int16) { panic("native reply used legacy voice output") }

func TestNativeTwoMinuteBurstPlaysFullyBeforeTurnCompletion(t *testing.T) {
	// Initialize shared hardware/log services outside the virtual clock. Their
	// process-lifetime goroutines are not part of this finite playback fixture.
	c, _, _ := nativeListeningTurn(t)
	c.disarmNoSpeech()
	c.claw.turn.cancel()
	synctest.Test(t, func(t *testing.T) {
		previousEvents := c.events
		c.events = make(chan event, 128)
		defer func() { c.events = previousEvents }()
		c.now = time.Now
		defer func() { c.disarm(); c.now = time.Now }()
		c.enter(phaseReplying)
		s := newStreamRate(0, 0, 0, clawOutputRate)
		c.reply = reply{id: 7, stream: s}
		c.arm(wakeword.MaxThink(c.slot))
		b := c.claw
		ctx, cancel := context.WithCancel(context.Background())
		target := &clawTurn{ctx: ctx, cancel: cancel, generation: 1, id: "example", outputWake: make(chan struct{}, 1)}
		b.turn = target
		audio := []byte(`{"sessionId":"example","type":"audio","audioBase64":"` + base64.StdEncoding.EncodeToString(make([]byte, 960)) + `"}`)
		// TTS can arrive much faster than its two minutes of playback.
		b.receive([]byte(`{"sessionId":"example","type":"event","talkEvent":{"type":"output.audio.started"}}`))
		for range 6000 {
			b.receive(audio)
		}
		b.receive([]byte(`{"sessionId":"example","type":"event","talkEvent":{"type":"output.audio.done"}}`))
		b.receive([]byte(`{"sessionId":"example","type":"event","talkEvent":{"type":"turn.ended"}}`))
		playDone, transportDone := make(chan error, 1), make(chan struct{})
		p := &timedReplyPlayer{}
		go func() { playDone <- s.play(target.ctx, p, func() {}) }()
		go func() { defer close(transportDone); b.playback(target) }()
		defer func() { _ = b.Stop(); <-transportDone }()
		at := time.Now()
		ended, played := false, false
		for !ended || !played {
			select {
			case e := <-c.events:
				if e.kind == evError {
					t.Fatal("healthy two-minute output failed", e.code)
				}
				c.handle(e)
				ended = ended || e.kind == evRunEnd
				if c.phase != phaseReplying || c.pending != nil {
					t.Fatal("stream completion ended the turn before local playback")
				}
			case <-c.deadlineChannel():
				c.handle(event{kind: evTimeout})
				if c.phase != phaseReplying {
					t.Fatal("active stream hit a total output limit")
				}
			case err := <-playDone:
				if err != nil {
					t.Fatal(err)
				}
				played = true
			case <-time.After(3 * time.Minute):
				t.Fatal("output did not finish")
			}
		}
		if s.bytes != 6000*960 || p.frames != 120*speaker.Rate || time.Since(at) < 2*time.Minute {
			t.Fatal("audio was truncated or unpaced", s.bytes, p.frames, time.Since(at))
		}
		if !c.nativeFinished || c.deadline != nil || !s.ended {
			t.Fatal("true completion did not allow the buffered tail to drain")
		}
	})
}

// Read exactly the same current phase timer as the conversation loop.
func (c *conversation) deadlineChannel() <-chan time.Time {
	if c.deadline == nil {
		return nil
	}
	return c.deadline.C
}

func TestNativePCMRefreshesIdleLimitButSpeechMarkersDoNot(t *testing.T) {
	c, _, backend := nativeListeningTurn(t)
	c.disarmNoSpeech()
	synctest.Test(t, func(t *testing.T) {
		c.now = time.Now
		defer func() { c.disarm(); c.now = time.Now }()
		c.enter(phaseReplying)
		c.reply = reply{id: 7, stream: newStreamRate(0, 0, 0, clawOutputRate)}
		quiet := wakeword.MaxThink(c.slot)
		c.arm(quiet)
		time.Sleep(quiet * 2 / 3)
		c.handle(event{kind: evStreamAudio, audio: make([]byte, 960)})
		time.Sleep(quiet * 2 / 3)
		select {
		case <-c.deadline.C:
			t.Fatal("stream PCM did not refresh the idle budget")
		default:
		}
		c.handle(event{kind: evClawSpeaking}) // A status alone cannot prove progress.
		time.Sleep(quiet/3 + time.Second)
		select {
		case <-c.deadline.C:
		default:
			t.Fatal("stalled output never expired")
		}
	})
	c.handle(event{kind: evTimeout})
	if c.phase != phaseIdle || backend.Err() == nil {
		t.Fatal("true audio inactivity did not retire the turn")
	}
}

func TestNativeQueuedPCMProtectsTimerRaceAndClearRevokesProgress(t *testing.T) {
	c, _, backend := nativeListeningTurn(t)
	c.now = time.Now
	c.disarmNoSpeech()
	c.enter(phaseThinking)
	b := c.claw
	b.queueOutput(b.turn, event{kind: evStreamAudio, audio: make([]byte, 960)})
	c.handle(event{kind: evTimeout})
	if c.phase != phaseThinking || backend.Err() != nil {
		t.Fatal("a selected old timeout discarded queued PCM")
	}
	b.queueOutput(b.turn, event{kind: evClawClear})
	c.handle(event{kind: evTimeout})
	if c.phase != phaseIdle || backend.Err() == nil {
		t.Fatal("cleared output kept a stalled turn alive")
	}
}

func TestNativeActiveTurnOutlivesFiveMinutesAndStillClosesOnStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeClaw{ready: true, supportsEnd: true, ended: make(chan struct{}), closed: make(chan struct{})}
		b, _ := newTestClaw(t, f)
		if err := b.Start(""); err != nil {
			t.Fatal(err)
		}
		ctx := b.turn.ctx
		for range 6 {
			time.Sleep(time.Minute)
			b.receive([]byte(`{"sessionId":"test-session","type":"audio","audioBase64":"AAAAAA=="}`))
			if ctx.Err() != nil {
				t.Fatal("healthy output hit a five-minute turn limit", ctx.Err())
			}
		}
		if err := b.End(); err != nil {
			t.Fatal(err)
		}
		waitClaw(t, f.ended)
		_ = b.Stop()
		waitClaw(t, f.closed)
		if ctx.Err() != context.Canceled {
			t.Fatal("explicit Stop no longer cancels the active turn")
		}
	})
}

func TestNativeOutputBacklogStillFailsExplicitlyAtItsBound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var failure event
	b := newClaw(func(e event) { failure = e })
	target := &clawTurn{ctx: ctx, cancel: cancel, generation: 1, outputWake: make(chan struct{}, 1)}
	b.gen, b.turn = 1, target
	for range clawOutputLimit + 1 {
		b.queueOutput(target, event{kind: evStreamAudio, audio: []byte{0, 0}})
	}
	if len(target.output) != clawOutputLimit || failure.kind != evError || failure.code != "openclaw-output-overflow" {
		t.Fatal("backlog grew without bound or silently discarded speech", len(target.output), failure.code)
	}
}

func TestNativeControlBacklogCannotExtendAudioIdleBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := newClaw(func(event) {})
	target := &clawTurn{ctx: ctx, cancel: cancel, generation: 1}
	b.gen, b.turn = 1, target
	b.queueOutput(target, event{kind: evStreamAudio, audio: []byte{0, 0}})
	b.queueOutput(target, event{kind: evClawSilent})
	at := time.Now().Add(2 * time.Minute)
	if b.outputRemaining(at, time.Minute) == 0 {
		t.Fatal("unplayed PCM was treated as an idle stream")
	}
	target.output = target.output[1:] // Only the control marker remains pending.
	if b.outputRemaining(at, time.Minute) != 0 {
		t.Fatal("control backlog kept an audio stall alive")
	}
}
