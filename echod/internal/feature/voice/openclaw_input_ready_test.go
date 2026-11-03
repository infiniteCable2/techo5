package voice

import (
	"context"
	"encoding/base64"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/mic"
)

type inputReadyGateway struct {
	*followupGateway
	onAcknowledged func()
	acknowledged   chan struct{}
	firstAppend    chan struct{}
	appendOnce     sync.Once
	closed         chan struct{}
	closeOnce      sync.Once
}

func (f *inputReadyGateway) Call(ctx context.Context, method string, params any, result any) error {
	if err := f.followupGateway.Call(ctx, method, params, result); err != nil {
		return err
	}
	switch method {
	case "talk.session.acknowledgeMark":
		f.onAcknowledged()
		close(f.acknowledged)
	case "talk.session.appendAudio":
		f.appendOnce.Do(func() { close(f.firstAppend) })
	case "talk.session.close":
		f.closeOnce.Do(func() { close(f.closed) })
	}
	return nil
}

func inputReadyFixture(t *testing.T, available *bool) (*clawBackend, *inputReadyGateway, <-chan struct{}, func()) {
	t.Helper()
	f := &inputReadyGateway{
		followupGateway: newFollowupGateway(), acknowledged: make(chan struct{}),
		firstAppend: make(chan struct{}), closed: make(chan struct{}),
	}
	f.activityEvents = true
	ctx, cancel := context.WithCancel(context.Background())
	b := newClaw(func(event) {})
	b.client, b.ctx, b.key = f, ctx, "agent:example:voice:input-ready"
	b.playbackReady = func() bool { return true }
	waiting := make(chan struct{}, 1)
	b.inputWaiting = func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}
	f.onAcknowledged = func() {
		b.mu.Lock()
		id := b.turn.id
		b.mu.Unlock()
		snapshot := activitySnapshot("listening", "idle", 0, 0, 0)
		snapshot.Input.Available = available
		b.receive(activityPacket(id, 1, "session.activity", "", snapshot))
	}
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			_ = b.Stop()
			waitClaw(t, f.closed)
		})
	}
	if err := b.Start(""); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(stop)
	waitClaw(t, f.acknowledged)
	return b, f, waiting, stop
}

func inputReadySnapshot(b *clawBackend, seq int, available *bool) {
	b.mu.Lock()
	id := b.turn.id
	b.mu.Unlock()
	snapshot := activitySnapshot("listening", "submitted", 0, 0, 0)
	snapshot.Input.Available = available
	b.receive(activityPacket(id, seq, "session.activity", "", snapshot))
}

func requireNoInputAppend(t *testing.T, f *inputReadyGateway) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call.method == "talk.session.appendAudio" {
			t.Fatal("input escaped its readiness wait", call.params)
		}
	}
}

func TestTalkInputReadyPreparesCaptureBeforeAdmittingPCM(t *testing.T) {
	no, yes := false, true
	b, f, waiting, _ := inputReadyFixture(t, &no)
	ready := make(chan error, 1)
	b.prepareCapture(func(_ int, err error) { ready <- err })
	waitClaw(t, waiting)
	requireNoInputAppend(t, f)
	b.mu.Lock()
	id, capture := b.turn.id, b.turn.captureID
	b.mu.Unlock()
	snapshot := activitySnapshot("listening", "idle", 0, 0, 0)
	snapshot.Input.Available = &yes
	b.receive(activityPacket("another-session", 9, "session.activity", "", snapshot))
	b.receive(activityPacket(id, 1, "session.activity", "", snapshot))
	inputReadySnapshot(b, 2, nil) // Missing telemetry cannot erase known backpressure.
	b.mu.Lock()
	blocked := b.turn.activity.inputAvailable != nil && !*b.turn.activity.inputAvailable
	seq := b.turn.activity.snapshotSeq
	b.mu.Unlock()
	if !blocked || seq != 2 {
		t.Fatal("stale, wrong-session or omitted availability released input")
	}
	requireNoInputAppend(t, f)
	select {
	case err := <-ready:
		t.Fatal("capture admitted inside protected window", err)
	default:
	}
	inputReadySnapshot(b, 3, &yes)
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("capture readiness did not settle")
	}
	pcm := []byte{1, 0, 2, 0}
	if err := b.Audio(pcm); err != nil {
		t.Fatal(err)
	}
	pcm[0] = 99
	if err := b.End(); err != nil {
		t.Fatal(err)
	}
	if got := waitFollowupID(t, f.ends); got != id {
		t.Fatal("input was redirected", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var appends []clawCall
	for _, call := range f.calls {
		if call.method == "talk.session.appendAudio" {
			appends = append(appends, call)
		}
	}
	if len(appends) != 2 {
		t.Fatal("buffered input was lost or duplicated", appends)
	}
	actual, err := base64.StdEncoding.DecodeString(appends[0].params["audioBase64"].(string))
	if err != nil || !reflect.DeepEqual(actual, []byte{1, 0, 2, 0}) {
		t.Fatal("copied PCM changed", actual, err)
	}
	if appends[0].params["captureId"] != capture || appends[1].params["captureId"] != capture || appends[1].params["audioBase64"] != "" || appends[1].params["endOfInput"] != true {
		t.Fatal("capture boundary or order changed", appends)
	}
}

func TestTalkInputReadyCancellationRetiresPreparation(t *testing.T) {
	no := false
	b, f, waiting, stop := inputReadyFixture(t, &no)
	ready := make(chan error, 1)
	b.prepareCapture(func(_ int, err error) { ready <- err })
	waitClaw(t, waiting)
	stop()
	select {
	case err := <-ready:
		if err == nil {
			t.Fatal("cancelled preparation admitted capture")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled preparation did not settle")
	}
	requireNoInputAppend(t, f)
}

func TestTalkInputReadyNeverReplaysProtectedWindowPCM(t *testing.T) {
	no := false
	b, f, _, _ := inputReadyFixture(t, &no)
	if err := b.Audio([]byte{1, 0}); err != nil {
		t.Fatal(err)
	}
	waitClaw(t, f.closed)
	requireNoInputAppend(t, f)
}

func TestTalkInputReadyDoesNotGateEmptyEndMarkers(t *testing.T) {
	no := false
	b, f, _, _ := inputReadyFixture(t, &no)
	if err := b.End(); err != nil {
		t.Fatal(err)
	}
	waitFollowupID(t, f.ends)
	f.mu.Lock()
	defer f.mu.Unlock()
	last := f.calls[len(f.calls)-1]
	if last.method != "talk.session.appendAudio" || last.params["audioBase64"] != "" || last.params["endOfInput"] != true {
		t.Fatal("empty input boundary waited for media readiness", last)
	}
}

func TestTalkInputReadyRechecksCaptureConnectionAndSourceAfterWaiting(t *testing.T) {
	for _, change := range []string{"capture", "connection", "source"} {
		t.Run(change, func(t *testing.T) {
			no, yes := false, true
			b, f, waiting, _ := inputReadyFixture(t, &no)
			failed := make(chan error, 1)
			b.prepareCapture(func(_ int, err error) { failed <- err })
			waitClaw(t, waiting)
			switch change {
			case "capture":
				b.mu.Lock()
				b.turn.captureID += "-replacement"
				b.mu.Unlock()
				inputReadySnapshot(b, 2, &yes)
			case "connection":
				f.mu.Lock()
				f.connection++
				f.mu.Unlock()
				b.checkConnection()
			case "source":
				b.mu.Lock()
				b.turn.selected = &config.OpenClaw{URL: "wss://changed.example", SessionKey: "agent:example:changed"}
				b.mu.Unlock()
				b.checkConnection()
			}
			select {
			case e := <-failed:
				if e == nil {
					t.Fatal("changed capture/authority admitted")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("changed preparation did not settle")
			}
			requireNoInputAppend(t, f)
		})
	}
}

func TestTalkInputReadyKeepsLegacyAndStartedCaptureBehavior(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "started-capture", true: "legacy-missing"}[legacy], func(t *testing.T) {
			yes, no := true, false
			available := &yes
			if legacy {
				available = nil
			}
			b, f, _, _ := inputReadyFixture(t, available)
			if err := b.Audio([]byte{1, 0}); err != nil {
				t.Fatal(err)
			}
			// This is the actual first PCM RPC, not a scheduling delay.
			waitClaw(t, f.firstAppend)
			if !legacy {
				inputReadySnapshot(b, 2, &no)
			}
			if err := b.Audio([]byte{2, 0}); err != nil {
				t.Fatal(err)
			}
			if err := b.End(); err != nil {
				t.Fatal(err)
			}
			waitFollowupID(t, f.ends)
			f.mu.Lock()
			defer f.mu.Unlock()
			var appends int
			for _, call := range f.calls {
				if call.method == "talk.session.appendAudio" {
					appends++
				}
			}
			if appends != 3 {
				t.Fatal("a started or legacy capture was interrupted", appends)
			}
		})
	}
}

func TestTalkFollowupWaitsBeforeCueCaptureAndOpeningSilenceDeadline(t *testing.T) {
	c, f, _ := connectedReply(t, true)
	no, yes := false, true
	inputReadySnapshot(c.claw, 1, &no)
	waiting := make(chan struct{}, 1)
	c.claw.inputWaiting = func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}
	at := time.Unix(100, 0)
	c.now = func() time.Time { return at }
	capture := &numberedCapture{Source: mic.New(), frames: make(chan []int16), subscribed: make(chan struct{}), at: at}
	c.source = capture
	c.handle(event{kind: evPlayed, reply: 41, at: at})
	flushClawSends(t, c)
	waitClaw(t, waiting)
	if c.phase != phaseThinking || c.stopAudio != nil || c.captureCue != nil || c.noSpeech != nil || c.deadline == nil {
		t.Fatal("protected window opened cue/capture or spent the opening-silence deadline")
	}
	select {
	case <-capture.subscribed:
		t.Fatal("microphone opened before native readiness")
	default:
	}
	inputReadySnapshot(c.claw, 2, &yes)
	c.handle(next(t, c, evClawCaptureReady))
	waitClaw(t, capture.subscribed)
	if c.phase != phaseListening || c.noSpeech == nil || c.opened != at {
		t.Fatal("ready capture did not enter listening with a fresh input boundary")
	}
	capture.mu.Lock()
	history := capture.history
	capture.mu.Unlock()
	if history != 0 {
		t.Fatal("protected-window microphone history was replayed", history)
	}
	// Cancel uses the same owner while its sender and capture goroutines exist.
	// It must not wait behind the readiness sender's scope lock.
	c.handle(event{kind: evCancel})
	waitFollowupID(t, f.closes)
}

func TestTalkCancelWhilePreparingDoesNotDeadlockOrAdmitLateCapture(t *testing.T) {
	c, f, _ := connectedReply(t, true)
	no := false
	inputReadySnapshot(c.claw, 1, &no)
	waiting := make(chan struct{}, 1)
	c.claw.inputWaiting = func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}
	c.handle(event{kind: evPlayed, reply: 41, at: time.Now()})
	flushClawSends(t, c)
	waitClaw(t, waiting)
	scope, generation := c.clawSends, c.claw.generation()
	c.handle(event{kind: evCancel})
	waitFollowupID(t, f.closes)
	c.handle(event{kind: evClawCaptureReady, clawSend: scope, clawGeneration: generation, capture: &nextTurn{followUp: true}})
	if c.phase != phaseIdle || c.stopAudio != nil || c.noSpeech != nil || scope.current() {
		t.Fatal("late readiness reopened a cancelled microphone")
	}
}

func TestTalkQueuedCaptureReadyRechecksAvailabilityAndConnection(t *testing.T) {
	for _, change := range []string{"availability", "connection"} {
		t.Run(change, func(t *testing.T) {
			c, f, _ := connectedReply(t, true)
			yes, no := true, false
			inputReadySnapshot(c.claw, 1, &yes)
			c.handle(event{kind: evPlayed, reply: 41, at: time.Now()})
			flushClawSends(t, c)
			ready := next(t, c, evClawCaptureReady)
			waiting := make(chan struct{}, 1)
			c.claw.inputWaiting = func() {
				select {
				case waiting <- struct{}{}:
				default:
				}
			}
			if change == "connection" {
				f.mu.Lock()
				f.connection++
				f.mu.Unlock()
			} else {
				inputReadySnapshot(c.claw, 2, &no)
			}
			c.handle(ready)
			if c.stopAudio != nil || c.captureCue != nil || c.noSpeech != nil {
				t.Fatal("queued readiness opened cue/capture after its source changed")
			}
			if change == "connection" {
				waitFollowupID(t, f.closes)
				if c.phase != phaseIdle {
					t.Fatal("changed connection did not close preparation")
				}
			} else {
				waitClaw(t, waiting)
				if c.phase != phaseThinking || c.deadline == nil {
					t.Fatal("lost readiness discarded preparation watchdog")
				}
				inputReadySnapshot(c.claw, 3, &yes)
				c.handle(next(t, c, evClawCaptureReady))
				if c.phase != phaseListening {
					t.Fatal("renewed readiness did not admit capture")
				}
			}
		})
	}
}
