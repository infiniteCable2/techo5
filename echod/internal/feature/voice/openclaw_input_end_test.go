package voice

import (
	"reflect"
	"testing"
	"time"
)

func TestClawClientInputEndBindsFramesAndEndWithoutTelemetry(t *testing.T) {
	f := &fakeClaw{ready: true, supportsEnd: true, ended: make(chan struct{}), closed: make(chan struct{})}
	b, _ := newTestClaw(t, f)
	if err := b.Start(""); err != nil { t.Fatal(err) }
	if err := b.Audio([]byte{1, 0}); err != nil { t.Fatal(err) }
	if err := b.End(); err != nil { t.Fatal(err) }
	if err := b.End(); err != nil { t.Fatal(err) }
	if err := b.Audio([]byte{9, 0}); err != nil { t.Fatal(err) }
	waitClaw(t, f.ended)
	if err := b.End(); err != nil { t.Fatal(err) }
	_ = b.Stop(); waitClaw(t, f.closed)
	f.mu.Lock(); calls := append([]clawCall(nil), f.calls...); f.mu.Unlock()
	var methods []string
	for _, call := range calls { methods = append(methods, call.method) }
	if !reflect.DeepEqual(methods, []string{"talk.session.create", "talk.session.acknowledgeMark", "talk.session.appendAudio", "talk.session.appendAudio", "talk.session.close"}) { t.Fatal("late audio or duplicate end escaped", methods) }
	if !reflect.DeepEqual(calls[0].params["capabilities"], []string{"activity-v1", "input-end"}) { t.Fatal("client completion capability missing", calls[0].params) }
	if _, override := calls[0].params["inputEndMode"]; override { t.Fatal("device overrode the server source selection") }
	capture, _ := calls[2].params["captureId"].(string)
	if capture == "" || len(capture) > 128 || calls[3].params["captureId"] != capture || calls[3].params["endOfInput"] != true || calls[3].params["audioBase64"] != "" { t.Fatal("capture identity depended on optional activity telemetry") }
}

func TestClawClientInputEndRefusesUnselectedOwnerBeforeAcknowledgement(t *testing.T) {
	for _, mode := range []string{"missing", "server", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			f := &fakeClaw{ready: true, supportsEnd: true, inputEndMode: mode, omitInputEndMode: mode == "missing", closed: make(chan struct{})}
			b, posted := newTestClaw(t, f)
			if err := b.Start(""); err != nil { t.Fatal(err) }
			if err := b.Audio([]byte{1, 0}); err != nil { t.Fatal(err) }
			if err := b.End(); err != nil { t.Fatal(err) }
			var e event
			select { case e = <-posted: case <-time.After(3*time.Second): t.Fatal("completion owner refusal was not posted") }
			if e.kind != evError || e.code != "openclaw-input-end-contract" { t.Fatal("wrong completion owner was not refused clearly", e) }
			waitClaw(t, f.closed)
			b.mu.Lock(); ready := b.turn.inputReady; b.mu.Unlock()
			if ready { t.Fatal("unselected input owner admitted capture") }
			f.mu.Lock(); calls := append([]clawCall(nil), f.calls...); f.mu.Unlock()
			if len(calls) != 2 || calls[0].method != "talk.session.create" || calls[1].method != "talk.session.close" { t.Fatal("unselected owner acknowledged or received media", calls) }
		})
	}
}

func TestClawClientInputEndSubmitsEmptyCaptureExactlyOnce(t *testing.T) {
	f := &fakeClaw{ready: true, supportsEnd: true, ended: make(chan struct{}), closed: make(chan struct{})}
	b, _ := newTestClaw(t, f)
	if err := b.Start(""); err != nil { t.Fatal(err) }
	if err := b.End(); err != nil { t.Fatal(err) }
	if err := b.End(); err != nil { t.Fatal(err) }
	waitClaw(t, f.ended)
	_ = b.Stop(); waitClaw(t, f.closed)
	f.mu.Lock(); calls := append([]clawCall(nil), f.calls...); f.mu.Unlock()
	if len(calls) != 4 || calls[2].method != "talk.session.appendAudio" || calls[2].params["endOfInput"] != true || calls[2].params["audioBase64"] != "" { t.Fatal("empty capture duplicated its end or synthesized PCM", calls) }
	capture, _ := calls[2].params["captureId"].(string)
	if capture == "" || len(capture) > 128 { t.Fatal("empty capture lost its identity") }
}

func TestTalkClientInputEndIsRenegotiatedAfterReconnect(t *testing.T) {
	c, f, oldID := connectedReply(t)
	oldScope := c.clawSends
	f.mu.Lock(); f.ready = false; f.connection++; f.mu.Unlock()
	c.claw.checkConnection()
	c.handle(next(t, c, evError))
	if waitFollowupID(t, f.closes) != oldID || c.phase != phaseIdle { t.Fatal("disconnect retained its logical Talk") }
	f.mu.Lock(); f.ready = true; f.inputEndMode = "server"; f.mu.Unlock()
	// Even a queued old End cannot act on a later connection or capture.
	c.sendClaw(oldScope, "late disconnected end", c.claw.End)
	c.handle(event{kind: evStart})
	flushClawSends(t, c)
	e := next(t, c, evError)
	if e.code != "openclaw-input-end-contract" { t.Fatal("reconnect reused the previous completion contract", e) }
	c.handle(e)
	if waitFollowupID(t, f.closes) == oldID || c.phase != phaseIdle || c.claw.turn != nil { t.Fatal("rejected reconnect retained capture") }
	f.mu.Lock(); calls := append([]clawCall(nil), f.calls...); creates := f.serial; f.mu.Unlock()
	if creates != 2 { t.Fatal("reconnect reused or retried a logical Talk", creates) }
	seenCreates := 0
	for _, call := range calls {
		if call.method == "talk.session.create" { seenCreates++; continue }
		if seenCreates == 2 && call.method != "talk.session.close" { t.Fatal("rejected reconnect acknowledged or submitted an old end", call.method) }
	}
}
