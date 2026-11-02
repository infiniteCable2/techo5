package voice

import (
	"testing"
	"time"
)

func TestNativeActivityBridgesSyllableGapsAndThenDisarmsOpeningLimit(t *testing.T) {
	c, at, _ := nativeListeningTurn(t)
	var a nativeActivity
	confirmed := false
	for range 6 {
		if a.feed(true, 40*time.Millisecond) {
			confirmed = true
			c.handle(event{kind: evSpeaking, text: c.turn.ID()})
		}
		a.feed(false, 60*time.Millisecond)
	}
	if !confirmed || c.noSpeech != nil {
		t.Fatal("natural syllable gaps prevented speech confirmation")
	}
	*at = at.Add(30 * time.Second)
	c.handle(event{kind: evNoSpeechTimeout})
	if c.phase != phaseListening {
		t.Fatal("opening guard ended a longer spoken request")
	}
}

func TestNativeSeparatedClicksDoNotAccumulateIntoSpeech(t *testing.T) {
	var a nativeActivity
	for range 20 {
		if a.feed(true, 60*time.Millisecond) {
			t.Fatal("isolated noise bursts claimed speech")
		}
		a.feed(false, 500*time.Millisecond)
	}
}

func TestNativeNonemptyPartialTranscriptConfirmsInputButSilenceDoesNot(t *testing.T) {
	c, at, _ := nativeListeningTurn(t)
	c.claw.turn.id = "current"
	c.claw.post = c.handle
	for _, raw := range []string{
		`{"sessionId":"current","type":"event","talkEvent":{"type":"input.audio.delta"}}`,
		`{"sessionId":"current","type":"event","talkEvent":{"type":"transcript.delta","payload":{"text":" "}}}`,
		`{"sessionId":"old","type":"event","talkEvent":{"type":"transcript.delta","payload":{"text":"hello"}}}`,
	} {
		c.claw.receive([]byte(raw))
	}
	if c.noSpeech == nil {
		t.Fatal("silence or stale transcript claimed speech")
	}
	c.claw.receive([]byte(`{"sessionId":"current","type":"event","talkEvent":{"type":"transcript.delta","payload":{"text":"hello"}}}`))
	if c.noSpeech != nil || c.phase != phaseListening {
		t.Fatal("partial speech was ignored or prematurely submitted")
	}
	*at = at.Add(25 * time.Second)
	c.handle(event{kind: evNoSpeechTimeout})
	if c.phase != phaseListening {
		t.Fatal("confirmed speech retained the opening timeout")
	}
}

func TestNativeWaitingMusicCannotEndCaptureBeforeRequestIsFinished(t *testing.T) {
	c, _, _ := nativeListeningTurn(t)
	stopped := false
	c.stopAudio = func() { stopped = true }
	c.handle(event{kind: evStreamAudio, audio: make([]byte, 960)})
	if stopped || c.phase != phaseListening || c.reply.stream != nil || c.noSpeech == nil {
		t.Fatal("waiting music stopped the open microphone")
	}
	c.handle(event{kind: evClawSpeaking})
	c.handle(event{kind: evStreamAudio, audio: make([]byte, 960)})
	if !stopped || c.phase != phaseReplying || c.reply.stream == nil || c.noSpeech != nil {
		t.Fatal("actual agent speech did not take reply ownership")
	}
}
