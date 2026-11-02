package voice

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestTalkSessionErrorEndsProcessingAndClearsTurnRing(t *testing.T) {
	c := thinkingTurn(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.be = c.claw
	c.claw.gen = 7
	c.claw.turn = &clawTurn{ctx: ctx, cancel: cancel, generation: 7, id: "test-session"}
	c.claw.receive(json.RawMessage(`{"sessionId":"test-session","type":"event","talkEvent":{"type":"session.error"}}`))
	handleNext(t, c)
	if c.phase != phaseIdle || c.deadline != nil || c.claim.Showing().Effect != "" {
		t.Fatal("terminal Talk error retained processing or its ring")
	}
	if c.claw.turn != nil || ctx.Err() == nil {
		t.Fatal("terminal Talk error did not retire its backend turn")
	}
}

func TestTalkClearRetiresReplyAndRejectsOldPlaybackCompletion(t *testing.T) {
	c := thinkingTurn(t)
	c.be = c.claw
	old := newStream(0, 0, 0)
	c.phase = phaseReplying
	c.reply = reply{id: 42, stream: old}
	c.handle(event{kind: evClawClear})
	if !old.ended || c.reply.stream != nil || c.phase != phaseThinking {
		t.Fatal("clear retained the ended stream instead of waiting for fresh output")
	}
	c.handle(event{kind: evPlayed, reply: 42})
	if c.phase != phaseThinking {
		t.Fatal("old playback completion ended the native turn")
	}
	c.handle(event{kind: evStreamAudio, audio: []byte{1, 0, 2, 0}})
	if c.phase != phaseReplying || c.reply.stream == nil || c.reply.stream == old || c.reply.stream.ended {
		t.Fatal("fresh Talk output did not acquire a fresh playback stream")
	}
	if c.reply.stream.rate != clawOutputRate || c.reply.stream.output == nil {
		t.Fatal("fresh native output entered the legacy 16 kHz reply path")
	}
	c.handle(event{kind: evPlaying, reply: 42})
	if c.deadline == nil {
		t.Fatal("old playback start consumed the replacement output deadline")
	}
}

func TestTalkQueuedOldAudioAndEndCannotReachReplacementTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := newClaw(func(event) {})
	oldTurn := &clawTurn{ctx: ctx, input: make(chan clawInput, 4)}
	b.turn = oldTurn
	c := &conversation{out: make(chan func() error, 8), events: make(chan event, 8)}
	oldScope := &clawSendScope{active: true}
	c.sendAudio(b, []byte{1, 0}, oldScope)
	c.sendClaw(oldScope, "end", b.End)
	oldScope.retire()
	newTurn := &clawTurn{ctx: ctx, input: make(chan clawInput, 4)}
	b.turn = newTurn
	newScope := &clawSendScope{active: true}
	c.sendAudio(b, []byte{2, 0}, newScope)
	for len(c.out) > 0 {
		if err := (<-c.out)(); err != nil {
			t.Fatal(err)
		}
	}
	if len(oldTurn.input) != 0 || len(newTurn.input) != 1 {
		t.Fatal("old queued input reached the replacement native turn")
	}
	got := <-newTurn.input
	if got.end || len(got.audio) != 2 || got.audio[0] != 2 {
		t.Fatal("replacement input was changed by old audio or End")
	}
}

func TestTalkSendFailureDoesNotDrainNewWakeAndIsFencedAfterRetirement(t *testing.T) {
	c := &conversation{out: make(chan func() error, 8), events: make(chan event, 8), phase: phaseThinking}
	oldScope := &clawSendScope{active: true}
	newScope := &clawSendScope{active: true}
	c.sendClaw(oldScope, "old operation", func() error { return errors.New("synthetic transport failure") })
	ranNew := make(chan struct{})
	c.sendClaw(newScope, "new start", func() error { close(ranNew); return nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.sends(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-ranNew:
	case <-time.After(time.Second):
		t.Fatal("old failure drained the queued new wake")
	}
	oldScope.retire()
	select {
	case failure := <-c.events:
		if failure.clawSend != oldScope {
			t.Fatal("send failure lost its local turn owner")
		}
		c.handle(failure)
	case <-time.After(time.Second):
		t.Fatal("transport failure was not reported")
	}
	if c.phase != phaseThinking {
		t.Fatal("retired turn failure changed the replacement turn")
	}
}

func TestTalkUsesExistingLocalEndpointForWakeAndFollowup(t *testing.T) {
	for _, followup := range []bool{false, true} {
		if !deviceEnds(newClaw(func(event) {}), followup) {
			t.Fatalf("native Talk did not preserve local endpoint, followup=%v", followup)
		}
	}
}
