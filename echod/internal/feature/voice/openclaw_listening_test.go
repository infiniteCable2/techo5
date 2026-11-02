package voice

import (
	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/led"
	"github.com/HuskerMinion/techo5/echod/internal/lib/endpoint"
	"testing"
	"time"
)

func TestNativeEmptyEndpointKeepsListeningAppearance(t *testing.T) {
	c, at, _ := nativeListeningTurn(t)
	c.handle(event{kind: evSpokeEnd, text: c.turn.ID()})
	if c.phase != phaseListening || c.noSpeech == nil {
		t.Fatal("empty endpoint left listening")
	}
	c.showThinking()
	if c.claim.Showing().Effect == led.EffectComet {
		t.Fatal("empty opening rotated like a submitted request")
	}
	*at = at.Add(10 * time.Second)
	c.handle(event{kind: evNoSpeechTimeout})
	if c.phase != phaseIdle {
		t.Fatal("empty opening did not end quietly")
	}
}

func TestNativeCaptureLimitSubmitsSpeechIncludingFollowup(t *testing.T) {
	for _, follow := range []bool{false, true} {
		t.Run(map[bool]string{false: "wake", true: "followup"}[follow], func(t *testing.T) {
			c, _, _ := nativeListeningTurn(t)
			c.followUp = follow
			if err := config.Set().Wake(0).MaxListen(60); err != nil {
				t.Fatal(err)
			}
			c.handle(event{kind: evSpeaking, text: c.turn.ID()})
			c.handle(event{kind: evTimeout})
			if c.phase != phaseThinking || c.noSpeech != nil {
				t.Fatal("capture cap discarded the recorded question")
			}
		})
	}
}

func TestNativeFinalDrainDoesNotFlashProcessingBeforeFollowup(t *testing.T) {
	c := thinkingTurn(t)
	c.be = c.claw
	c.enter(phaseReplying)
	c.reply = reply{id: 9}
	c.handle(event{kind: evClawAudible, reply: 9, audible: true})
	c.handle(event{kind: evStreamEnd})
	c.handle(event{kind: evClawAudible, reply: 9, audible: false})
	if c.claim.Showing().Effect == led.EffectComet || c.phase != phaseReplying || c.pending != nil {
		t.Fatal("final tail flashed processing or opened capture too early")
	}
}

func TestNativeOnePointTwoSecondPauseSpansThinkingPause(t *testing.T) {
	ep := endpoint.New(nativeEndpointSettings(1200))
	feed := func(level int16) bool {
		frame := make([]int16, endpoint.Window)
		for i := range frame {
			frame[i] = level
		}
		return ep.Feed(frame)
	}
	for range 5 {
		if feed(2000) {
			t.Fatal("speech ended")
		}
	}
	for range 8 {
		if feed(100) {
			t.Fatal("short thinking pause ended the request")
		}
	}
	for range 5 {
		if feed(1600) {
			t.Fatal("resumed speech ended")
		}
	}
	for range 11 {
		if feed(100) {
			t.Fatal("new pause ended before 1.2 seconds")
		}
	}
	if !feed(100) {
		t.Fatal("1.2 second endpoint not honored")
	}
	if nativeEndpointSettings(0).Quiet != endpoint.Default.Quiet {
		t.Fatal("old configuration changed default")
	}
}
