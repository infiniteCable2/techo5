package voice

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/feature/activity"
)

func TestClawActivityCaptureWithoutOutputSettlesThroughActualGatewayAndOutputQueue(t *testing.T) {
	for _, outcome := range []string{"no-input", "transcribed"} {
		t.Run(outcome, func(t *testing.T) {
			c := thinkingTurn(t)
			f := &fakeClaw{ready: true, supportsEnd: true, activityEvents: true, ended: make(chan struct{}), closed: make(chan struct{})}
			b, _ := newTestClaw(t, f)
			c.claw, c.be = b, b
			b.post = c.post
			b.observe = c.post
			c.turn = c.log.Begin(1, "example", activity.WakeWord)
			c.enter(phaseListening)
			if err := b.Start(""); err != nil {
				t.Fatal(err)
			}
			if err := b.Audio([]byte{1, 0}); err != nil {
				t.Fatal(err)
			}
			c.think()
			if err := b.End(); err != nil {
				t.Fatal(err)
			}
			waitClaw(t, f.ended)
			b.mu.Lock()
			capture, session := b.turn.captureID, b.turn.id
			b.mu.Unlock()
			if outcome == "transcribed" {
				// The native owner establishes admission before recognition completes.
				pending := activitySnapshot("listening", "idle", 0, 0, 0)
				pending.Work.Pending = 1
				b.receive(activityPacket(session, 1, "session.activity", "", pending))
			}
			b.receive(activityPacket(session, 2, "input.outcome", capture, inputOutcome("capture", outcome)))
			if outcome == "transcribed" {
				b.receive(activityPacket(session, 3, "session.activity", "", activitySnapshot("listening", "idle", 0, 0, 1)))
				for len(c.events) > 0 {
					handleNext(t, c)
				}
				if c.phase != phaseThinking || c.claw.turn == nil {
					t.Fatal("recognition completed a still-running native operation")
				}
			}
			b.receive(activityPacket(session, 4, "session.activity", "", activitySnapshot("listening", "idle", 0, 0, 0)))
			for c.phase != phaseIdle {
				handleNext(t, c)
			}
			waitClaw(t, f.closed)
			f.mu.Lock()
			defer f.mu.Unlock()
			var methods []string
			for _, call := range f.calls {
				methods = append(methods, call.method)
			}
			if !reflect.DeepEqual(methods, []string{"talk.session.create", "talk.session.acknowledgeMark", "talk.session.appendAudio", "talk.session.appendAudio", "talk.session.close"}) {
				t.Fatal("observation dispatched additional work", methods)
			}
		})
	}
}

func activityPacket(session string, seq int, kind, capture string, payload any) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"sessionId": session, "type": "event", "talkEvent": map[string]any{
		"sessionId": session, "seq": seq, "type": kind, "captureId": capture, "payload": payload,
	}})
	return raw
}

func activitySnapshot(input, output string, pending, queued, running int) clawActivitySnapshot {
	var s clawActivitySnapshot
	s.Version, s.Connection = 1, "ready"
	s.Input.Phase, s.Input.Pending = input, pending
	s.Work.Queued, s.Work.Running = queued, running
	s.Output.State = output
	return s
}

func inputOutcome(scope, outcome string) any {
	return map[string]any{"version": 1, "scope": scope, "outcome": outcome}
}

func activityConversation(t *testing.T) *conversation {
	t.Helper()
	c, _, _ := nativeListeningTurn(t)
	c.claw.turn.captureID = "capture-current"
	c.claw.turn.activity.supported = true
	// The real receive boundary is exercised; scheduling is explicit so each
	// assertion can also cover a previously posted notification becoming stale.
	c.claw.post = func(event) {}
	c.handle(event{kind: evSpeaking, text: c.turn.ID()})
	c.think()
	return c
}

func handleActivityEnd(t *testing.T, c *conversation) {
	t.Helper()
	c.claw.mu.Lock()
	turn := c.claw.turn
	queued := turn.output
	turn.output = nil
	gen := turn.generation
	c.claw.mu.Unlock()
	for _, out := range queued {
		if out.event.kind != evClawActivityEnd {
			t.Fatal("unexpected output in quiet fixture")
		}
		e := out.event
		e.clawGeneration, e.clawPlayback = gen, out.epoch
		c.handle(e)
	}
}

func TestNativeActivityTerminalWaitsForFreshQuietAggregate(t *testing.T) {
	for _, outcome := range []string{"no-input", "rejected", "transcribed", "failed", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			c := activityConversation(t)
			c.claw.receive(activityPacket("example", 1, "session.activity", "", activitySnapshot("listening", "idle", 0, 0, 0)))
			c.claw.receive(activityPacket("example", 2, "input.outcome", "capture-current", inputOutcome("capture", outcome)))
			c.handle(event{kind: evClawActivity})
			if c.phase != phaseThinking {
				t.Fatal("an older snapshot settled a newer outcome")
			}
			c.claw.receive(activityPacket("example", 3, "session.activity", "", activitySnapshot("transcribing", "idle", 1, 0, 0)))
			c.handle(event{kind: evClawActivity})
			if c.phase != phaseThinking || c.shown.NativeStatus != "Transcribing…" {
				t.Fatal("pending recognition was discarded")
			}
			c.claw.receive(activityPacket("example", 4, "session.activity", "", activitySnapshot("listening", "idle", 0, 0, 0)))
			c.handle(event{kind: evClawActivity})
			handleActivityEnd(t, c)
			if c.phase != phaseIdle || c.deadline != nil || c.claw.turn != nil {
				t.Fatal("terminal input waited for MaxThink")
			}
		})
	}
}

func TestNativeActivityIgnoresStaleCaptureAndUtteranceOutcomes(t *testing.T) {
	for _, kind := range []struct{ capture, scope string }{{"capture-old", "capture"}, {"capture-current", "utterance"}} {
		c := activityConversation(t)
		c.claw.receive(activityPacket("example", 1, "input.outcome", kind.capture, inputOutcome(kind.scope, "no-input")))
		c.claw.receive(activityPacket("example", 2, "session.activity", "", activitySnapshot("listening", "idle", 0, 0, 0)))
		c.handle(event{kind: evClawActivity})
		if c.phase != phaseThinking {
			t.Fatal("an unrelated outcome ended the capture")
		}
	}
}

func TestNativeActivityQueuedNoticeUsesLatestFactsAndCannotCancelWork(t *testing.T) {
	c := activityConversation(t)
	c.claw.receive(activityPacket("example", 1, "input.outcome", "capture-current", inputOutcome("capture", "rejected")))
	c.claw.receive(activityPacket("example", 2, "session.activity", "", activitySnapshot("listening", "idle", 0, 0, 0)))
	// A posted quiet notice is handled only after newer native work exists.
	c.claw.receive(activityPacket("example", 3, "session.activity", "", activitySnapshot("listening", "preparing", 0, 0, 1)))
	c.claw.receive(activityPacket("example", 2, "session.activity", "", activitySnapshot("listening", "idle", 0, 0, 0)))
	c.handle(event{kind: evClawActivity})
	handleActivityEnd(t, c)
	if c.phase != phaseThinking || c.shown.NativeStatus != "Thinking…" {
		t.Fatal("stale telemetry discarded newer work")
	}
	budget := c.deadline
	c.handle(event{kind: evClawActivity})
	if c.deadline != budget {
		t.Fatal("duplicate observation extended watchdog")
	}
}

func TestNativeActivityUnknownAndUnsupportedKeepWatchdog(t *testing.T) {
	for _, outcome := range []string{"unknown"} {
		c := activityConversation(t)
		c.claw.receive(activityPacket("example", 1, "input.outcome", "capture-current", inputOutcome("capture", outcome)))
		c.claw.receive(activityPacket("example", 2, "session.activity", "", activitySnapshot("listening", "idle", 0, 0, 0)))
		c.handle(event{kind: evClawActivity})
		if c.phase != phaseThinking || c.deadline == nil {
			t.Fatal("unknown input was invented as empty")
		}
	}
	c := activityConversation(t)
	c.claw.turn.activity.supported = false
	c.claw.receive(activityPacket("example", 1, "input.outcome", "capture-current", inputOutcome("capture", "no-input")))
	c.claw.receive(activityPacket("example", 2, "session.activity", "", activitySnapshot("listening", "idle", 0, 0, 0)))
	c.handle(event{kind: evClawActivity})
	if c.phase != phaseThinking || c.deadline == nil || c.shown.NativeStatus != "" {
		t.Fatal("unnegotiated telemetry changed legacy fallback")
	}
}

func TestNativeActivityUnknownOrAnotherOpenInputCannotProveQuiet(t *testing.T) {
	for _, phase := range []string{"unknown", "receiving", "candidate", "speech", "transcribing"} {
		c := activityConversation(t)
		c.claw.receive(activityPacket("example", 1, "input.outcome", "capture-current", inputOutcome("capture", "no-input")))
		snapshot := activitySnapshot(phase, "idle", 0, 0, 0)
		snapshot.Input.CaptureID = "another-capture"
		c.claw.receive(activityPacket("example", 2, "session.activity", "", snapshot))
		c.handle(event{kind: evClawActivity})
		if len(c.claw.turn.output) != 0 || c.phase != phaseThinking || c.deadline == nil {
			t.Fatal("unobserved/open input settled a capture", phase)
		}
	}
}

func TestNativeActivityCannotEndLocallyBufferedReply(t *testing.T) {
	c, _, _ := connectedReply(t, true)
	c.claw.turn.captureID = "capture-current"
	c.claw.receive(activityPacket(c.claw.turn.id, 1, "input.outcome", "capture-current", inputOutcome("capture", "rejected")))
	c.claw.receive(activityPacket(c.claw.turn.id, 2, "session.activity", "", activitySnapshot("listening", "idle", 0, 0, 0)))
	c.handle(event{kind: evClawActivity})
	seenMarker := false
	for !seenMarker {
		select {
		case e := <-c.events:
			c.handle(e)
			seenMarker = e.kind == evClawActivityEnd
		case <-time.After(time.Second):
			t.Fatal("actual quiet marker did not reach playback queue")
		}
	}
	if c.phase != phaseReplying || c.pending != nil || c.reply.id != 41 {
		t.Fatal("telemetry revoked local playback ownership")
	}
	c.handle(event{kind: evPlayed, reply: 41, at: time.Now()})
	flushClawSends(t, c)
	c.handle(next(t, c, evClawCaptureReady))
	if c.phase != phaseListening || !c.followUp {
		t.Fatal("local drain no longer owns followup")
	}
}

func TestNativeActivityNotificationDoesNotWaitForRequiredAudioConsumer(t *testing.T) {
	c := activityConversation(t)
	blocked := make(chan struct{})
	defer close(blocked)
	c.claw.post = func(event) { <-blocked }
	notices := make(chan event, 2)
	c.claw.observe = func(e event) {
		select {
		case notices <- e:
		default:
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.claw.receive(activityPacket("example", 1, "session.activity", "", activitySnapshot("transcribing", "idle", 1, 0, 0)))
		c.claw.emitFrom(c.claw.turn, clawFence{c.claw.turn.generation, c.claw.turn.outputEpoch}, event{kind: evClawActivityEnd})
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("progress blocked behind required audio consumer")
	}
	select {
	case e := <-notices:
		if e.kind != evClawActivity {
			t.Fatal(e.kind)
		}
	default:
		t.Fatal("missing progress notice")
	}
	select {
	case e := <-notices:
		if e.kind != evClawActivityEnd {
			t.Fatal(e.kind)
		}
	default:
		t.Fatal("missing nonblocking settlement marker")
	}
}

func TestNativeActivityQuietMarkerCannotOverflowValidPCMBacklog(t *testing.T) {
	c := activityConversation(t)
	var required []event
	c.claw.post = func(e event) { required = append(required, e) }
	c.claw.turn.output = make([]clawOutput, clawOutputLimit)
	c.claw.receive(activityPacket("example", 1, "input.outcome", "capture-current", inputOutcome("capture", "no-input")))
	c.claw.receive(activityPacket("example", 2, "session.activity", "", activitySnapshot("listening", "idle", 0, 0, 0)))
	if len(required) != 0 || len(c.claw.turn.output) != clawOutputLimit || c.claw.turn.activity.settlementQueued {
		t.Fatal("telemetry failed a valid audio backlog")
	}
	// With no room, retain watchdog fallback. A subsequent native observation
	// may use space released by the existing playback owner without a retry RPC.
	c.claw.turn.output = nil
	c.claw.receive(activityPacket("example", 3, "session.activity", "", activitySnapshot("listening", "idle", 0, 0, 0)))
	if len(c.claw.turn.output) != 1 || c.claw.turn.output[0].event.kind != evClawActivityEnd {
		t.Fatal("dropped observation retained a stale queue claim")
	}
}

func TestClawActivityNegotiatesAndBindsEveryAppendWithoutExtraRPC(t *testing.T) {
	f := &fakeClaw{ready: true, supportsEnd: true, activityEvents: true, ended: make(chan struct{}), closed: make(chan struct{})}
	b, _ := newTestClaw(t, f)
	b.wantTimings = true
	if err := b.Start(""); err != nil {
		t.Fatal(err)
	}
	if err := b.Audio([]byte{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := b.End(); err != nil {
		t.Fatal(err)
	}
	waitClaw(t, f.ended)
	f.mu.Lock()
	calls := append([]clawCall(nil), f.calls...)
	f.mu.Unlock()
	if len(calls) != 4 || !reflect.DeepEqual(calls[0].params["capabilities"], []string{"activity-v1", "output-word-timings"}) {
		t.Fatal(calls)
	}
	capture, _ := calls[2].params["captureId"].(string)
	if capture == "" || len(capture) > 128 || calls[3].params["captureId"] != capture {
		t.Fatal("PCM and End have different capture identity")
	}
	_ = b.Stop()
	waitClaw(t, f.closed)
}

func TestClawActivityBuffersOnlyBoundedTelemetryBeforeCreateResult(t *testing.T) {
	f := &fakeClaw{ready: true, supportsEnd: true, activityEvents: true, opened: make(chan struct{}), ended: make(chan struct{}), closed: make(chan struct{})}
	b, _ := newTestClaw(t, f)
	if err := b.Start(""); err != nil {
		t.Fatal(err)
	}
	b.receive(activityPacket("foreign", 100, "session.activity", "", activitySnapshot("listening", "idle", 0, 0, 0)))
	b.receive(activityPacket("test-session", 1, "session.activity", "", activitySnapshot("transcribing", "idle", 1, 0, 0)))
	for range 12 {
		b.receive(activityPacket("foreign", 101, "input.outcome", "foreign", inputOutcome("capture", "no-input")))
	}
	b.mu.Lock()
	buffered := len(b.turn.earlyActivity)
	b.mu.Unlock()
	if buffered != 8 {
		t.Fatal("early telemetry was not bounded", buffered)
	}
	if err := b.End(); err != nil {
		t.Fatal(err)
	}
	close(f.opened)
	waitClaw(t, f.ended)
	view, supported := b.activityView()
	if !supported || !view.known || view.snapshot.Input.Pending != 1 || view.settled {
		t.Fatal("initial snapshot lost or foreign initial state accepted")
	}
	_ = b.Stop()
	waitClaw(t, f.closed)
}

func TestNativeActivityMalformedSnapshotCannotProveQuiet(t *testing.T) {
	c := activityConversation(t)
	c.claw.receive(activityPacket("example", 1, "input.outcome", "capture-current", inputOutcome("capture", "no-input")))
	missingPending := map[string]any{"version": 1, "connection": "ready", "input": map[string]any{"phase": "listening"},
		"work": map[string]int{"pending": 0, "queued": 0, "running": 0}, "output": map[string]any{"generation": 0, "state": "idle"}}
	c.claw.receive(activityPacket("example", 2, "session.activity", "", missingPending))
	c.handle(event{kind: evClawActivity})
	if len(c.claw.turn.output) != 0 || c.phase != phaseThinking || c.deadline == nil {
		t.Fatal("missing facts became an empty capture")
	}
}

func TestNativeActivityObservesProgressWithoutAdmittingOrEndingCapture(t *testing.T) {
	c, _, _ := nativeListeningTurn(t)
	c.claw.turn.activity.supported = true
	c.claw.post = func(event) {}
	opening := c.noSpeech
	c.claw.receive(activityPacket("example", 1, "session.activity", "", activitySnapshot("speech", "streaming", 1, 0, 1)))
	c.handle(event{kind: evClawActivity})
	if c.phase != phaseListening || c.noSpeech != opening || c.nativeSpeaking || c.nativeAudible {
		t.Fatal("observation changed capture/speech authority")
	}
	c.handle(event{kind: evSpeaking, text: c.turn.ID()})
	c.think()
	for i, state := range []struct {
		snapshot clawActivitySnapshot
		label    string
	}{
		{activitySnapshot("transcribing", "idle", 1, 0, 0), "Transcribing…"},
		{activitySnapshot("listening", "preparing", 0, 1, 0), "Waiting…"},
		{activitySnapshot("listening", "streaming", 0, 0, 1), "Thinking…"},
	} {
		c.claw.receive(activityPacket("example", i+2, "session.activity", "", state.snapshot))
		c.handle(event{kind: evClawActivity})
		if c.shown.NativeStatus != state.label || c.phase != phaseThinking || c.nativeAudible {
			t.Fatal("incorrect progress projection", state.label)
		}
	}
}

func TestClawActivityCaptureIdentityChangesOnlyAtFollowup(t *testing.T) {
	c, f, id := connectedReply(t, true)
	// The same Gateway fixture advertises the additive contract; no additional
	// create/close or authority RPC exists for the retained connection.
	c.claw.mu.Lock()
	first := c.claw.turn.captureID
	c.claw.mu.Unlock()
	c.handle(event{kind: evPlayed, reply: 41, at: time.Now()})
	flushClawSends(t, c)
	c.handle(next(t, c, evClawCaptureReady))
	c.claw.mu.Lock()
	second := c.claw.turn.captureID
	c.claw.mu.Unlock()
	if first == "" || second == first || c.claw.turn.id != id {
		t.Fatal("followup did not replace only capture identity")
	}
	if err := c.claw.Audio([]byte{2, 0}); err != nil {
		t.Fatal(err)
	}
	if err := c.claw.End(); err != nil {
		t.Fatal(err)
	}
	if got := waitFollowupID(t, f.ends); got != id {
		t.Fatal("followup replaced logical Talk")
	}
	c.think()
	c.enter(phaseReplying)
	c.reply = reply{id: 42, stream: newStreamRate(0, 0, 0, clawOutputRate)}
	for _, packet := range []string{
		`{"sessionId":"` + id + `","type":"event","talkEvent":{"type":"output.audio.started"}}`,
		`{"sessionId":"` + id + `","type":"audio","audioBase64":"AQACAA=="}`,
		`{"sessionId":"` + id + `","type":"event","talkEvent":{"type":"turn.ended"}}`,
	} {
		c.claw.receive(json.RawMessage(packet))
	}
	for !c.nativeFinished {
		handleNext(t, c)
	}
	c.handle(event{kind: evPlayed, reply: 42, at: time.Now()})
	flushClawSends(t, c)
	c.handle(next(t, c, evClawCaptureReady))
	c.claw.mu.Lock()
	third := c.claw.turn.captureID
	c.claw.mu.Unlock()
	if third == first || third == second || !c.followUp {
		t.Fatal("second followup reused a retired input identity")
	}
	// Delayed first-capture outcomes cannot close either subsequent input.
	c.claw.receive(activityPacket(id, 1, "input.outcome", first, inputOutcome("capture", "no-input")))
	c.claw.receive(activityPacket(id, 2, "session.activity", "", activitySnapshot("listening", "idle", 0, 0, 0)))
	c.handle(event{kind: evClawActivity})
	if c.phase != phaseListening {
		t.Fatal("old outcome ended second followup")
	}
	if err := c.claw.Audio([]byte{3, 0}); err != nil {
		t.Fatal(err)
	}
	if err := c.claw.End(); err != nil {
		t.Fatal(err)
	}
	if got := waitFollowupID(t, f.ends); got != id {
		t.Fatal("second followup replaced logical Talk")
	}
	c.handle(event{kind: evCancel})
	if got := waitFollowupID(t, f.closes); got != id {
		t.Fatal("final close changed logical Talk")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	creates, closes := 0, 0
	var captures []string
	for _, call := range f.calls {
		if call.method == "talk.session.create" {
			creates++
		}
		if call.method == "talk.session.close" {
			closes++
		}
		if call.method == "talk.session.appendAudio" {
			capture, _ := call.params["captureId"].(string)
			captures = append(captures, capture)
		}
	}
	if creates != 1 || closes != 1 || !f.confirmed || !reflect.DeepEqual(captures, []string{first, first, second, second, third, third}) {
		t.Fatal("telemetry changed native conversation/RPC ownership", captures)
	}
}
