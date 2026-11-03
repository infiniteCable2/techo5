package voice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/feature/activity"
	gw "github.com/HuskerMinion/techo5/echod/internal/lib/openclaw"
)

// This mock models only the Gateway boundary: its pending spoken question is
// keyed by the returned logical Talk id, and close destroys that question.
// It neither supplies a permission cache nor asserts server policy behavior.
type followupGateway struct {
	mu             sync.Mutex
	calls          []clawCall
	ends           chan string
	closes         chan string
	serial         int
	connection     uint64
	ready          bool
	pending        map[string]bool
	confirmed      bool
	input          map[string][]byte
	failAppend     bool
	activityEvents bool
}

func newFollowupGateway() *followupGateway {
	return &followupGateway{ends: make(chan string, 8), closes: make(chan string, 8), ready: true, pending: map[string]bool{}, input: map[string][]byte{}}
}
func (f *followupGateway) Ready() bool             { f.mu.Lock(); defer f.mu.Unlock(); return f.ready }
func (f *followupGateway) Events() <-chan gw.Event { return nil }
func (f *followupGateway) ConnectionGeneration() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connection
}
func (f *followupGateway) Call(_ context.Context, method string, params any, result any) error {
	p := params.(map[string]any)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, clawCall{method, p})
	id, _ := p["sessionId"].(string)
	switch method {
	case "talk.session.create":
		f.serial++
		data, _ := json.Marshal(map[string]any{"sessionId": fmt.Sprintf("conversation-%d", f.serial), "readyMark": "media-ready:example", "audio": map[string]any{
			"inputEncoding": "pcm16", "inputSampleRateHz": 16000, "outputEncoding": "pcm16", "outputSampleRateHz": 24000, "supportsInputEnd": true, "activityEvents": f.activityEvents,
		}})
		return json.Unmarshal(data, result)
	case "talk.session.appendAudio":
		if f.failAppend {
			return errors.New("synthetic append failure")
		}
		pcm, _ := base64.StdEncoding.DecodeString(p["audioBase64"].(string))
		f.input[id] = append(f.input[id], pcm...)
		if p["endOfInput"] == true {
			if len(f.input[id]) > 0 {
				switch f.input[id][0] {
				case 1:
					f.pending[id] = true // synthetic request needing a spoken answer
				case 2:
					f.confirmed = f.pending[id]
					delete(f.pending, id) // synthetic "Yes"
				}
			}
			f.input[id] = nil
			f.ends <- id
		}
	case "talk.session.close":
		delete(f.pending, id)
		f.closes <- id
	default:
		if method != "talk.session.acknowledgeMark" {
			return errors.New("unexpected non-Talk RPC")
		}
	}
	return nil
}

func waitFollowupID(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(3 * time.Second):
		t.Fatal("Talk operation did not settle")
		return ""
	}
}

func connectedReply(t *testing.T, activityEvents ...bool) (*conversation, *followupGateway, string) {
	t.Helper()
	c := thinkingTurn(t)
	cfg := config.Get()
	cfg.OpenClaw = config.OpenClaw{URL: "wss://gateway.example", SessionKey: "agent:example:voice:echo-example"}
	path := filepath.Join(t.TempDir(), "state.json")
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	config.Use(path)
	if err := config.Set().Wake(0).FollowUp(10); err != nil {
		t.Fatal(err)
	}
	// This fixture tests transport/drain ownership, not a physical wake cue.
	if err := config.Set().Wake(0).Tone(config.ToneNone); err != nil {
		t.Fatal(err)
	}
	f := newFollowupGateway()
	f.activityEvents = len(activityEvents) > 0 && activityEvents[0]
	ctx, cancel := context.WithCancel(context.Background())
	c.claw.client, c.claw.ctx, c.claw.key = f, ctx, cfg.OpenClaw.SessionKey
	c.claw.playbackReady = func() bool { return true }
	c.be, c.clawConfig = c.claw, cfg.OpenClaw
	c.clawSends = &clawSendScope{active: true}
	c.turn = c.log.Begin(1, "example", activity.WakeWord)
	if err := c.claw.Start(""); err != nil {
		t.Fatal(err)
	}
	if err := c.claw.Audio([]byte{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := c.claw.End(); err != nil {
		t.Fatal(err)
	}
	id := waitFollowupID(t, f.ends)
	// The local player is represented by a reply id. Its explicit evPlayed
	// below is the completed audible drain, not the received turn.ended event.
	c.enter(phaseReplying)
	c.reply = reply{id: 41, stream: newStreamRate(0, 0, 0, clawOutputRate)}
	c.claw.receive(json.RawMessage(fmt.Sprintf(`{"sessionId":%q,"type":"event","talkEvent":{"type":"output.audio.started"}}`, id)))
	c.claw.receive(json.RawMessage(fmt.Sprintf(`{"sessionId":%q,"type":"audio","audioBase64":"AQACAA=="}`, id)))
	c.claw.receive(json.RawMessage(fmt.Sprintf(`{"sessionId":%q,"type":"event","talkEvent":{"type":"turn.ended"}}`, id)))
	for !c.nativeFinished {
		handleNext(t, c)
	}
	t.Cleanup(func() { c.handle(event{kind: evCancel}); cancel(); streams.Wait() })
	return c, f, id
}

func flushClawSends(t *testing.T, c *conversation) {
	t.Helper()
	for len(c.out) > 0 {
		if err := (<-c.out)(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTalkConnectedFollowupKeepsLogicalIDAndSpokenAnswer(t *testing.T) {
	c, f, id := connectedReply(t)
	oldScope := c.clawSends
	gen := c.claw.generation()
	oldEnd := event{kind: evRunEnd, clawGeneration: gen, clawPlayback: 1}
	// Operations still in the shared sender queue must be retired before the
	// logical session opens its next input, including duplicate final End.
	c.sendAudio(c.claw, []byte{9, 0}, oldScope)
	c.sendClaw(oldScope, "late end", c.claw.End)
	c.handle(event{kind: evPlayed, reply: 41, at: time.Now()})
	if c.phase != phaseListening || !c.followUp || c.clawSends == oldScope || oldScope.current() {
		t.Fatal("reply did not admit a fresh follow-up capture")
	}
	if c.claw.turn == nil || c.claw.turn.id != id || c.claw.generation() == gen {
		t.Fatal("follow-up lost its logical session or capture fence")
	}
	c.handle(oldEnd)
	c.handle(event{kind: evPlayed, reply: 41})
	if c.phase != phaseListening {
		t.Fatal("old reply completion ended the new capture")
	}
	flushClawSends(t, c)
	c.sendAudio(c.claw, []byte{2, 0}, c.clawSends)
	c.sendClaw(c.clawSends, "yes boundary", c.claw.End)
	flushClawSends(t, c)
	c.stopStreaming()
	streams.Wait()
	if got := waitFollowupID(t, f.ends); got != id {
		t.Fatal("Yes was submitted to another logical Talk")
	}
	f.mu.Lock()
	confirmed, creates := f.confirmed, f.serial
	var closed int
	for _, call := range f.calls {
		if call.method == "talk.session.close" {
			closed++
		}
	}
	f.mu.Unlock()
	if !confirmed || creates != 1 || closed != 0 {
		t.Fatalf("pending answer lost: confirmed=%v creates=%d closes=%d", confirmed, creates, closed)
	}
	c.handle(event{kind: evCancel})
	if got := waitFollowupID(t, f.closes); got != id {
		t.Fatal("logical end did not close the retained Talk")
	}
}

func TestTalkConnectedFollowupEndsOnPrivacyErrorOrSilence(t *testing.T) {
	for _, reason := range []string{"silence", "mute", "source", "error", "transport", "disconnect", "manual"} {
		t.Run(reason, func(t *testing.T) {
			c, f, id := connectedReply(t)
			c.handle(event{kind: evPlayed, reply: 41, at: time.Now()})
			flushClawSends(t, c)
			switch reason {
			case "silence":
				until := c.noSpeechUntil
				c.now = func() time.Time { return until.Add(time.Second) }
				c.handle(event{kind: evNoSpeechTimeout})
			case "mute":
				c.busy = func() bool { return true }
				c.handle(event{kind: evWatch})
			case "source":
				c.clawConfig.SessionKey = "agent:example:voice:other"
				c.handle(event{kind: evWatch})
			case "error":
				c.handle(event{kind: evError, code: "openclaw-session"})
			case "transport":
				f.mu.Lock()
				f.failAppend = true
				f.mu.Unlock()
				if err := c.claw.Audio([]byte{2, 0}); err != nil {
					t.Fatal(err)
				}
				for c.phase != phaseIdle {
					handleNext(t, c)
				}
			case "disconnect":
				f.mu.Lock()
				f.connection++
				f.mu.Unlock()
				if err := c.claw.Audio([]byte{2, 0}); err == nil {
					t.Fatal("old logical Talk accepted input after reconnect")
				}
				// Run the production poll's check at this controlled boundary;
				// its one-second ticker is not part of the revocation contract.
				c.claw.checkConnection()
				for c.phase != phaseIdle {
					handleNext(t, c)
				}
			case "manual":
				c.handle(event{kind: evStart, slot: 0})
			}
			if got := waitFollowupID(t, f.closes); got != id {
				t.Fatal("logical Talk did not close")
			}
			if c.claw.turn != nil {
				t.Fatal("ended conversation retained native authority")
			}
			if reason == "manual" {
				c.startPending()
				flushClawSends(t, c)
				if err := c.claw.Audio([]byte{2, 0}); err != nil {
					t.Fatal(err)
				}
				if err := c.claw.End(); err != nil {
					t.Fatal(err)
				}
				if next := waitFollowupID(t, f.ends); next == id {
					t.Fatal("manual conversation reused prior logical Talk")
				}
				f.mu.Lock()
				confirmed := f.confirmed
				f.mu.Unlock()
				if confirmed {
					t.Fatal("unrelated conversation inherited the spoken challenge")
				}
			}
		})
	}
}

func TestTalkFollowupAdmissionFailureClosesRetainedSession(t *testing.T) {
	c, f, id := connectedReply(t)
	f.mu.Lock()
	f.ready = false
	f.mu.Unlock()
	c.handle(event{kind: evPlayed, reply: 41, at: time.Now()})
	if got := waitFollowupID(t, f.closes); got != id || c.claw.turn != nil {
		t.Fatal("failed admission leaked retained Talk")
	}
}

func TestTalkChangedSourceAtPlaybackBoundaryDoesNotReopenCapture(t *testing.T) {
	c, f, id := connectedReply(t)
	c.clawConfig.SessionKey = "agent:example:voice:other"
	c.handle(event{kind: evPlayed, reply: 41, at: time.Now()})
	if got := waitFollowupID(t, f.closes); got != id || c.claw.turn != nil || c.phase != phaseIdle || c.pending != nil {
		t.Fatal("changed source reused or reopened the previous Talk")
	}
}

func TestTalkConnectedFollowupKeepsTimingTimelineWithFreshVisibleText(t *testing.T) {
	c, _, id := connectedReply(t)
	generation := int64(0)
	contract := gw.OutputTimingsContract{Accepted: true, Version: 1, OutputGeneration: &generation, SampleRate: clawOutputRate, Encoding: "pcm16", Channels: 1, SampleUnit: "pcm-frame", Mapping: "piecewise-segment-v1", TextDelivery: "outputTextBlock-v1"}
	target := c.claw.turn
	target.timings = gw.NewWordTimings(id, "relay-example", contract)
	if target.timings == nil {
		t.Fatal("invalid test timing contract")
	}
	block := func(turn, block, text string) captionPresentation {
		raw, _ := json.Marshal(map[string]any{"sessionId": id, "relaySessionId": "relay-example", "outputGeneration": 0, "type": "outputTextBlock", "turnId": turn, "textBlockId": block, "textRevision": 1, "purpose": "final", "text": text})
		return c.claw.presentation(target, raw)
	}
	if got := block("turn-one", "block-one", "First answer."); got.text != "First answer." {
		t.Fatal(got)
	}
	c.handle(event{kind: evPlayed, reply: 41, at: time.Now()})
	if got := block("turn-two", "block-two", "Second answer."); got.text != "Second answer." {
		t.Fatal("follow-up replayed prior caption", got.text)
	}
	if target.timings.Generation() != generation || target.timings.Text() != "First answer.\n\nSecond answer." {
		t.Fatal("local capture reset the native timing timeline")
	}
}

func TestTalkTurnEndedProducerCannotRetagAfterPlaybackOpensFollowup(t *testing.T) {
	c, _, id := connectedReply(t)
	b := c.claw
	b.mu.Lock()
	target := b.turn
	fence := clawFence{target.generation, target.outputEpoch}
	b.mu.Unlock()
	advanced := make(chan struct{})
	var once sync.Once
	b.mu.Lock()
	b.post = func(e event) {
		c.handle(e)
		if e.kind == evStreamEnd {
			once.Do(func() {
				// Pause the native playback producer at the exact completed
				// stream boundary while the local owner opens its next capture.
				c.handle(event{kind: evPlayed, reply: 41, at: time.Now()})
				close(advanced)
			})
		}
	}
	b.mu.Unlock()
	b.receive(json.RawMessage(fmt.Sprintf(`{"sessionId":%q,"type":"event","talkEvent":{"type":"turn.ended"}}`, id)))
	waitClaw(t, advanced)
	// Resume a producer that decoded against the old capture before that
	// barrier. Its run-end, PCM, and direct transcript cannot acquire new tags.
	b.queueOutputFrom(target, fence, event{kind: evRunEnd}, event{kind: evStreamAudio, audio: []byte{9, 0}})
	b.emitFrom(target, fence, event{kind: evHeard, text: "old input"})
	b.mu.Lock()
	queued := len(target.output)
	b.mu.Unlock()
	if c.phase != phaseListening || queued != 0 || c.shown.Heard == "old input" {
		t.Fatal("old native producer contaminated the next capture")
	}
}

func TestTalkFollowupRechecksSelectedRouteAndSinkAtQueuedAdmission(t *testing.T) {
	for _, reason := range []string{"source", "sink"} {
		t.Run(reason, func(t *testing.T) {
			c, f, id := connectedReply(t)
			c.handle(event{kind: evPlayed, reply: 41, at: time.Now()})
			if c.phase != phaseListening {
				t.Fatal("follow-up offer was not admitted")
			}
			c.stopStreaming()
			streams.Wait()
			// Start remains in the real shared send queue. Revoke its prepared
			// source/sink after the offer, before that operation executes.
			if reason == "sink" {
				c.claw.playbackReady = func() bool { return false }
			} else {
				cfg := config.Get()
				cfg.OpenClaw.SessionKey = "agent:example:voice:other"
				path := filepath.Join(t.TempDir(), "changed-state.json")
				data, _ := json.Marshal(cfg)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				config.Use(path)
			}
			failed := false
			for len(c.out) > 0 {
				if err := (<-c.out)(); err != nil {
					failed = true
					c.handle(event{kind: evError, code: "send"})
					break
				}
			}
			if !failed {
				t.Fatal("queued admission used a revoked source or playback sink")
			}
			if got := waitFollowupID(t, f.closes); got != id || c.claw.turn != nil {
				t.Fatal("failed queued admission retained Talk")
			}
		})
	}
}
