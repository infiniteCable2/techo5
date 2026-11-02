package voice

import (
	"github.com/HuskerMinion/techo5/echod/internal/hardware/speaker"
	"reflect"
	"testing"
	"time"
)

func TestNativeRingFollowsLocalSpeechWaitingAndDrainedPauses(t *testing.T) {
	var p nativePresentation
	var got []bool
	emit := func(on bool) { got = append(got, on) }
	at := time.Unix(100, 0)
	p.add(clawOutputRate, false) // One second of waiting music.
	p.add(clawOutputRate, true)  // Then one second of actual speech.
	p.submitted = 2 * clawOutputRate
	p.poll(at, 2*speaker.Rate, emit)
	p.poll(at.Add(time.Second), speaker.Rate/2, emit)
	if !reflect.DeepEqual(got, []bool{true}) {
		t.Fatal("speech did not follow the played timeline", got)
	}
	p.poll(at.Add(2*time.Second), 0, emit)
	if len(got) != 1 {
		t.Fatal("queue drained before its hardware tail had sounded")
	}
	p.poll(at.Add(2*time.Second+speaker.HardwareTail), 0, emit)
	if !reflect.DeepEqual(got, []bool{true, false}) {
		t.Fatal("silent processing gap stayed solid", got)
	}
	p.add(clawOutputRate, false)
	p.submitted += clawOutputRate
	p.poll(at.Add(3*time.Second), speaker.Rate/2, emit)
	if len(got) != 2 {
		t.Fatal("waiting music was classified as speech")
	}
	p.add(clawOutputRate, true)
	p.submitted += clawOutputRate
	p.poll(at.Add(4*time.Second), speaker.Rate/2, emit)
	if !reflect.DeepEqual(got, []bool{true, false, true}) {
		t.Fatal("resumed speech did not relight solid ring", got)
	}
}

func TestNativePauseChangesOnlyPresentationAndFencesOldReply(t *testing.T) {
	c := thinkingTurn(t)
	c.be = c.claw
	c.enter(phaseReplying)
	c.reply = reply{id: 7}
	c.handle(event{kind: evClawAudible, reply: 7, audible: true})
	if !c.nativeAudible || c.deadline != nil {
		t.Fatal("audible speech kept the processing deadline")
	}
	c.handle(event{kind: evClawAudible, reply: 7, audible: false})
	if c.phase != phaseReplying || c.nativeAudible || c.deadline == nil || c.pending != nil {
		t.Fatal("pause ended the turn or opened follow-up")
	}
	c.handle(event{kind: evClawAudible, reply: 6, audible: true})
	if c.nativeAudible || c.deadline == nil {
		t.Fatal("old reply changed the current pause")
	}
	c.handle(event{kind: evClawAudible, reply: 7, audible: true})
	if !c.nativeAudible || c.deadline != nil {
		t.Fatal("new speech did not leave processing")
	}
}

func TestNativeDoneMarksSpanWithoutEndingTurnAndAllowsNextSpeech(t *testing.T) {
	c, _, _ := nativeListeningTurn(t)
	b := c.claw
	b.turn.id = "current"
	b.turn.outputWake = make(chan struct{}, 1)
	b.receive([]byte(`{"sessionId":"current","type":"event","talkEvent":{"type":"output.audio.started"}}`))
	b.receive([]byte(`{"sessionId":"current","type":"event","talkEvent":{"type":"output.audio.done"}}`))
	b.receive([]byte(`{"sessionId":"current","type":"event","talkEvent":{"type":"output.audio.started"}}`))
	if q := b.turn.output; len(q) != 3 || q[0].event.kind != evClawSpeaking || q[1].event.kind != evClawSilent || q[2].event.kind != evClawSpeaking {
		t.Fatal("speech span done ended the response or hid resumed speech")
	}
}

func TestNativeWakeDuringReplyRetiresOldSessionBeforeNewCapture(t *testing.T) {
	c, _, old := nativeListeningTurn(t)
	c.enter(phaseReplying)
	c.reply = reply{id: 7}
	if c.start(nextTurn{slot: 0, woke: true}) {
		t.Fatal("interruption skipped orderly retirement")
	}
	if old.Err() == nil || c.claw.turn != nil || c.phase != phaseIdle || c.pending == nil || !c.pending.woke {
		t.Fatal("wake during output did not retire its native session and retain new capture")
	}
	c.turnEvent(event{kind: evClawAudible, reply: 7, audible: true})
	if c.nativeAudible {
		t.Fatal("old reply relit after wake interruption")
	}
}

func TestNativeStopClosesTheGatewaySession(t *testing.T) {
	f := &fakeClaw{ready: true, supportsEnd: true, ended: make(chan struct{}), closed: make(chan struct{})}
	b, _ := newTestClaw(t, f)
	if err := b.Start(""); err != nil {
		t.Fatal(err)
	}
	if err := b.End(); err != nil {
		t.Fatal(err)
	}
	waitClaw(t, f.ended)
	if err := b.Stop(); err != nil {
		t.Fatal(err)
	}
	waitClaw(t, f.closed)
	if b.Ready() != true || b.turn != nil {
		t.Fatal("stopping a turn lost the paired connection or retained capture")
	}
}
