package voice

import (
	"github.com/HuskerMinion/techo5/echod/internal/hardware/mic"
	"testing"
	"time"
)

func requireAECLearning(t *testing.T, want bool) {
	t.Helper()
	if got := mic.Get().Adapting(); got != want {
		t.Fatalf("builtin learning=%v, want %v", got, want)
	}
}

func TestAECLearningTracksNativeCaptureReplyAndCancellation(t *testing.T) {
	c, _, _ := nativeListeningTurn(t)
	requireAECLearning(t, false)
	c.think()
	requireAECLearning(t, true)
	c.speak("")
	requireAECLearning(t, true)
	c.handle(event{kind: evCancel})
	requireAECLearning(t, true)
}

func TestAECLearningFreezesAgainForNativeFollowup(t *testing.T) {
	c, _, _ := connectedReply(t)
	requireAECLearning(t, true)
	c.handle(event{kind: evRunEnd})
	c.handle(event{kind: evPlayed, reply: 41, at: time.Now()})
	flushClawSends(t, c)
	c.handle(next(t, c, evClawCaptureReady))
	if c.phase != phaseListening || !c.followUp {
		t.Fatal("native followup did not enter capture")
	}
	requireAECLearning(t, false)
	c.handle(event{kind: evCancel})
	requireAECLearning(t, true)
}

func TestAECLearningResumesAfterOpeningSilenceTimeout(t *testing.T) {
	c, at, _ := nativeListeningTurn(t)
	requireAECLearning(t, false)
	*at = at.Add(10 * time.Second)
	c.handle(event{kind: evNoSpeechTimeout})
	if c.phase != phaseIdle {
		t.Fatal("opening silence timeout did not close capture")
	}
	requireAECLearning(t, true)
}
