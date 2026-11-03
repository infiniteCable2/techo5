package voice

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/feature/activity"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/led"
)

func nativeListeningTurn(t *testing.T) (*conversation, *time.Time, context.Context) {
	t.Helper()
	c := thinkingTurn(t)
	c.be = c.claw
	c.enter(phaseListening)
	c.turn = c.log.Begin(1, "example", activity.WakeWord)
	at := time.Unix(100, 0)
	c.now = func() time.Time { return at }
	ctx, cancel := context.WithCancel(context.Background())
	c.claw.gen = 1
	c.claw.turn = &clawTurn{ctx: ctx, cancel: cancel, generation: 1, id: "example"}
	c.clawSends = &clawSendScope{active: true}
	c.disarm()
	c.armNoSpeech()
	return c, &at, ctx
}

func TestNativeNoInputEndsAtTenSeconds(t *testing.T) {
	c, at, backend := nativeListeningTurn(t)
	if !c.noSpeechUntil.Equal(at.Add(10 * time.Second)) {
		t.Fatal("opening silence window is not ten seconds")
	}
	*at = at.Add(9 * time.Second)
	c.handle(event{kind: evNoSpeechTimeout})
	if c.phase != phaseListening {
		t.Fatal("silence ended before the deadline")
	}
	*at = at.Add(time.Second)
	c.handle(event{kind: evNoSpeechTimeout})
	if c.phase != phaseIdle || c.noSpeech != nil || c.deadline != nil || backend.Err() == nil || c.claw.turn != nil {
		t.Fatal("silence did not retire the timer, capture and native turn")
	}
	if got := c.claim.Showing(); got.Effect != "" || len(got.Frame) != 0 {
		t.Fatal("silence left the turn ring lit")
	}
}

func TestNativeSpeechKeepsTheExistingRequestBudgetAndRejectsLateOldActivity(t *testing.T) {
	c, at, _ := nativeListeningTurn(t)
	budget := c.deadline
	*at = at.Add(9 * time.Second)
	c.handle(event{kind: evSpeaking, text: "previous-turn"})
	if c.noSpeech == nil {
		t.Fatal("old microphone activity cancelled the new opening limit")
	}
	c.handle(event{kind: evSpeaking, text: c.turn.ID()})
	if c.noSpeech != nil || c.deadline == nil || c.deadline == budget {
		t.Fatal("real speech did not replace the opening window with its request budget")
	}
	*at = at.Add(4 * time.Second)
	c.handle(event{kind: evNoSpeechTimeout})
	if c.phase != phaseListening {
		t.Fatal("a spoken request was cut off by the old silence limit")
	}
}

func TestNativeFollowupSpeechNearItsEndGetsAFullRequestBudget(t *testing.T) {
	c, at, _ := nativeListeningTurn(t)
	if err := config.Set().Wake(0).FollowUp(10); err != nil {
		t.Fatal(err)
	}
	c.followUp = true
	c.arm(10 * time.Second)
	c.armNoSpeech()
	before := c.deadline
	*at = at.Add(9 * time.Second)
	c.handle(event{kind: evSpeaking, text: c.turn.ID()})
	if c.noSpeech != nil || c.deadline == before {
		t.Fatal("follow-up speech retained the short idle deadline")
	}
	*at = at.Add(2 * time.Second)
	c.handle(event{kind: evNoSpeechTimeout})
	if c.phase != phaseListening {
		t.Fatal("a valid follow-up was abandoned at ten seconds")
	}
}

func TestNativeEndpointWithoutConfirmedRequestKeepsTheSilenceGuard(t *testing.T) {
	c, at, _ := nativeListeningTurn(t)
	c.handle(event{kind: evSpokeEnd, text: c.turn.ID()})
	if c.phase != phaseListening || c.noSpeech == nil {
		t.Fatal("a relative endpoint alone moved an empty opening to processing")
	}
	*at = at.Add(10 * time.Second)
	c.handle(event{kind: evNoSpeechTimeout})
	if c.phase != phaseIdle {
		t.Fatal("an empty processing request outlived the opening limit")
	}
}

func TestNativeAcceptedTranscriptCancelsTheOpeningGuard(t *testing.T) {
	c, at, _ := nativeListeningTurn(t)
	c.handle(event{kind: evHeard, text: "What time is it?"})
	if c.phase != phaseThinking || c.noSpeech != nil {
		t.Fatal("accepted input left the opening silence deadline armed")
	}
	*at = at.Add(20 * time.Second)
	c.handle(event{kind: evNoSpeechTimeout})
	if c.phase != phaseThinking {
		t.Fatal("the opening limit cut off a real processing request")
	}
}

func TestNativeFollowupAcknowledgementCancelsTheOpeningGuard(t *testing.T) {
	c, at, _ := nativeListeningTurn(t)
	c.followUp = true
	c.woke = false
	c.handle(event{kind: evHeard, text: "Thank you."})
	if c.phase != phaseThinking || c.noSpeech != nil {
		t.Fatal("a brief follow-up acknowledgement retained the silence guard")
	}
	*at = at.Add(11 * time.Second)
	c.handle(event{kind: evNoSpeechTimeout})
	if c.phase != phaseThinking {
		t.Fatal("the opening limit cut off an accepted follow-up")
	}
}

func TestNativeReplyOpensFollowupOnlyAfterLocalPlaybackCompletes(t *testing.T) {
	c, _, _ := connectedReply(t)
	c.lookHere = true // Native Talk is not blocked by the legacy page heuristic.
	c.handle(event{kind: evRunEnd})
	if c.phase != phaseReplying || c.pending != nil {
		t.Fatal("server completion opened capture before local audio ended")
	}
	c.handle(event{kind: evPlayed, reply: 41, at: time.Now()})
 flushClawSends(t,c)
 c.handle(next(t,c,evClawCaptureReady))
	if c.phase != phaseListening || !c.followUp || c.woke || c.noSpeech == nil {
		t.Fatal("finished answer did not open a keyword-free follow-up")
	}
	if remaining := c.noSpeechUntil.Sub(c.now()); remaining < 9*time.Second || remaining > 10*time.Second {
		t.Fatal("follow-up did not get its own ten-second window", remaining)
	}
}

func TestNativeWaitingMusicKeepsProcessingRingAndDeadline(t *testing.T) {
	c := thinkingTurn(t)
	c.be = c.claw
	c.showThinking()
	if c.claim.Showing().Effect != led.EffectComet {
		t.Fatal("processing ring does not rotate")
	}
	c.enter(phaseReplying)
	c.reply = reply{id: 3}
	c.showReplying()
	deadline := c.deadline
	c.handle(event{kind: evPlaying, reply: 3})
	if c.claim.Showing().Effect != led.EffectComet || c.deadline != deadline {
		t.Fatal("waiting music claimed speech or removed the processing deadline")
	}
	c.handle(event{kind: evClawSpeaking})
	if c.claim.Showing().Effect != led.EffectComet {
		t.Fatal("transport receipt claimed locally audible speech")
	}
	c.handle(event{kind: evClawAudible, reply: 3, audible: true})
	if got := c.claim.Showing(); got.Effect != "" || len(got.Frame) != led.Segments || c.deadline != nil {
		t.Fatal("real speech did not show a standing ring")
	}
	frame := c.claim.Showing().Frame
	for _, color := range frame {
		if color != c.ring.Base() {
			t.Fatal("standing speech ring changed the configured color")
		}
	}
	c.handle(event{kind: evClawClear})
	if c.phase != phaseThinking || c.nativeSpeaking || c.claim.Showing().Effect != led.EffectComet || c.deadline == nil {
		t.Fatal("clear did not return to processing")
	}
}

func TestNativeSpeechMarkersFollowQueuedAudioAndResetOnClear(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := newClaw(func(event) {})
	b.turn = &clawTurn{ctx: ctx, cancel: cancel, generation: 1, id: "current", outputWake: make(chan struct{}, 1)}
	b.receive(json.RawMessage(`{"sessionId":"current","type":"audio","audioBase64":"AAAAAA=="}`))
	b.receive(json.RawMessage(`{"sessionId":"old","type":"event","talkEvent":{"type":"output.audio.started"}}`))
	b.receive(json.RawMessage(`{"sessionId":"current","type":"event","talkEvent":{"type":"output.audio.started"}}`))
	b.receive(json.RawMessage(`{"sessionId":"current","type":"event","talkEvent":{"type":"output.audio.delta"}}`))
	b.receive(json.RawMessage(`{"sessionId":"current","type":"audio","audioBase64":"AAAAAA=="}`))
	if q := b.turn.output; len(q) != 3 || q[0].event.kind != evStreamAudio || q[1].event.kind != evClawSpeaking || q[2].event.kind != evStreamAudio {
		t.Fatal("speech marker was lost, duplicated or moved ahead of waiting audio")
	}
	b.receive(json.RawMessage(`{"sessionId":"current","type":"clear"}`))
	b.receive(json.RawMessage(`{"sessionId":"current","type":"event","talkEvent":{"type":"output.audio.delta"}}`))
	if q := b.turn.output; len(q) != 2 || q[0].event.kind != evClawClear || q[1].event.kind != evClawSpeaking {
		t.Fatal("cleared output could not identify resumed speech")
	}
}

func TestNativeActivityIgnoresClicksAndConfirmsSustainedInputOnce(t *testing.T) {
	var a nativeActivity
	for range 100 {
		if a.feed(false, 20*time.Millisecond) {
			t.Fatal("silence counted as activity")
		}
	}
	for range 3 {
		if a.feed(true, 20*time.Millisecond) {
			t.Fatal("short click counted as a request")
		}
	}
	a.feed(false, nativeActivityGap)
	for range 9 {
		if a.feed(true, 20*time.Millisecond) {
			t.Fatal("activity confirmed too soon")
		}
	}
	if !a.feed(true, 20*time.Millisecond) {
		t.Fatal("sustained input was not confirmed")
	}
	if a.feed(true, time.Second) {
		t.Fatal("activity was emitted repeatedly")
	}
}
