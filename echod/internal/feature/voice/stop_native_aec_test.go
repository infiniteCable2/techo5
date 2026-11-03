package voice

import (
	"testing"
	"time"
)

// The detector routes StopSlot without a call to Interrupt (detect/route_test).
// Exercise the real remainder: a native reply claim, the
// public stop-word entry, evCancel, local silence and native session.close.
func TestStopWordInterruptRetiresNativeReplyAndPlayback(t *testing.T) {
	c := thinkingTurn(t)
	f := &fakeClaw{ready: true, supportsEnd: true, closed: make(chan struct{})}
	b, _ := newTestClaw(t, f)
	c.claw = b
	c.be = b
	if err := b.Start("synthetic"); err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(3 * time.Second)
	for {
		b.mu.Lock()
		ready := b.turn != nil && b.turn.id != ""
		b.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(end) {
			t.Fatal("native session not ready")
		}
		time.Sleep(time.Millisecond)
	}
	c.speak("")
	claim := c.reply.claim
	if claim == nil || !c.sound.Busy() {
		t.Fatal("native reply does not hold Sound.Busy")
	}
	// No hardware Player.Run is started. The real speech claim owns the stream
	// already; Driver.Silence must cancel it even before hardware is available.
	(&Voice{turn: c}).Interrupt()
	c.handle(next(t, c, evCancel))
	if c.phase != phaseIdle || c.sound.Busy() || c.speaker.Queued() != 0 {
		t.Fatal("stop left reply or queued playback alive")
	}
	b.mu.Lock()
	retired := b.turn == nil
	b.mu.Unlock()
	if !retired {
		t.Fatal("stop left native transport turn alive")
	}
	waitClaw(t, f.closed)
	if !claim.Stopped() {
		t.Fatal("reply claim was not stopped")
	}
}
