package voice

import (
	"context"
	"testing"
)

type stopAckClaw struct {
	*fakeClaw
	acknowledged chan struct{}
}

func (f *stopAckClaw) Call(ctx context.Context, method string, params any, result any) error {
	err := f.fakeClaw.Call(ctx, method, params, result)
	if err == nil && method == "talk.session.acknowledgeMark" {
		close(f.acknowledged)
	}
	return err
}

// Outside a telephone call, the detector routes StopSlot directly to
// Voice.Interrupt (detect/route_test).
// Exercise the real remainder: a native reply claim, the
// public stop-word entry, evCancel, local silence and native session.close.
func TestStopWordInterruptRetiresNativeReplyAndPlayback(t *testing.T) {
	c := thinkingTurn(t)
	f := &fakeClaw{ready: true, supportsEnd: true, closed: make(chan struct{})}
	b, _ := newTestClaw(t, f)
	transport := &stopAckClaw{fakeClaw: f, acknowledged: make(chan struct{})}
	b.client = transport
	c.claw = b
	c.be = b
	if err := b.Start("synthetic"); err != nil {
		t.Fatal(err)
	}
	waitClaw(t, transport.acknowledged)
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
