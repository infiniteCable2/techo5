package voice

import (
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/lib/speechvad"
)

func TestNeuralInputKeepsLongSpeechAndBreathingThenEndsAtConfiguredPause(t *testing.T) {
	c, at, _ := nativeListeningTurn(t)
	v, err := newNativeInput(1200)
	if err != nil {
		t.Fatal(err)
	}
	feed := func(active bool, d time.Duration) bool {
		on, end := v.observe(speechvad.Result{Active: active, Duration: d})
		if on {
			c.handle(event{kind: evSpeaking, text: c.turn.ID()})
		}
		return end
	}
	for range 30 {
		if feed(true, time.Second) || feed(false, time.Second) {
			t.Fatal("breathing ended a longer request")
		}
		*at = at.Add(2 * time.Second)
		c.handle(event{kind: evNoSpeechTimeout})
		if c.noSpeech != nil || c.phase != phaseListening {
			t.Fatal("opening timeout interrupted confirmed speech")
		}
	}
	if feed(false, 199*time.Millisecond) {
		t.Fatal("endpoint preceded the configured pause")
	}
	if !feed(false, time.Millisecond) {
		t.Fatal("completed pause failed to end the turn")
	}
	if feed(false, time.Second) {
		t.Fatal("endpoint was emitted twice")
	}
}

func TestNeuralInputSilenceNeverClaimsSpeechOrSubmits(t *testing.T) {
	v, err := newNativeInput(1200)
	if err != nil {
		t.Fatal(err)
	}
	for range 500 {
		on, end, err := v.feed(make([]int16, 320))
		if err != nil || on || end {
			t.Fatalf("silence triggered native speech/endpoint: %v", err)
		}
	}
	if v.activity.sent {
		t.Fatal("silence bypassed opening timeout")
	}
}

func TestLevelCandidateNeedsVADAndCannotSubmitNoise(t *testing.T) {
	c, at, _ := nativeListeningTurn(t)
	v, err := newNativeInput(1200)
	if err != nil {
		t.Fatal(err)
	}
	for range 500 {
		v.level(true, 20*time.Millisecond)
		on, end := v.observe(speechvad.Result{Duration: 20 * time.Millisecond})
		if on || end {
			t.Fatal("level-only noise claimed or submitted speech")
		}
	}
	if v.activity.sent {
		t.Fatal("level candidate bypassed VAD confirmation")
	}
	*at = at.Add(10 * time.Second)
	c.handle(event{kind: evNoSpeechTimeout})
	if c.phase != phaseIdle {
		t.Fatal("level-only noise retained a silent turn")
	}
}

func TestRecentLevelSpeedsVADConfirmationButNeverHoldsIt(t *testing.T) {
	v, err := newNativeInput(1200)
	if err != nil {
		t.Fatal(err)
	}
	v.level(true, 40*time.Millisecond)
	if on, _ := v.observe(speechvad.Result{Active: true, Duration: 30 * time.Millisecond}); on {
		t.Fatal("one VAD result confirmed speech")
	}
	if on, _ := v.observe(speechvad.Result{Active: true, Duration: 30 * time.Millisecond}); !on {
		t.Fatal("paired level and VAD did not confirm promptly")
	}
	for range 59 {
		v.level(true, 20*time.Millisecond)
		if _, end := v.observe(speechvad.Result{Duration: 20 * time.Millisecond}); end {
			t.Fatal("endpoint preceded configured silence")
		}
	}
	v.level(true, 20*time.Millisecond)
	if _, end := v.observe(speechvad.Result{Duration: 20 * time.Millisecond}); !end {
		t.Fatal("level noise held the neural endpoint open")
	}
}

func TestStaleLevelDoesNotAccelerateVADButQuietSpeechStillConfirms(t *testing.T) {
	v, err := newNativeInput(1200)
	if err != nil {
		t.Fatal(err)
	}
	v.level(true, 40*time.Millisecond)
	v.level(false, levelCandidateLife+time.Millisecond)
	for range 6 {
		if on, _ := v.observe(speechvad.Result{Active: true, Duration: 30 * time.Millisecond}); on {
			t.Fatal("stale level accelerated confirmation")
		}
	}
	if on, _ := v.observe(speechvad.Result{Active: true, Duration: 30 * time.Millisecond}); !on {
		t.Fatal("quiet VAD speech required a room-level crossing")
	}
}

func TestLevelDropsCannotInterruptConfirmedNeuralSpeech(t *testing.T) {
	v, err := newNativeInput(1200)
	if err != nil {
		t.Fatal(err)
	}
	v.level(true, 40*time.Millisecond)
	v.observe(speechvad.Result{Active: true, Duration: 60 * time.Millisecond})
	for range 1500 {
		v.level(false, 20*time.Millisecond)
		if _, end := v.observe(speechvad.Result{Active: true, Duration: 20 * time.Millisecond}); end {
			t.Fatal("quiet ongoing speech ended with the level gate")
		}
	}
}
