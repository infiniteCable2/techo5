package voice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/hardware/mic"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/speaker"
)

// The existing acoustic leaf supplies numbered PCM. Capture, framing, sender,
// Gateway adapter, reply stream and speaker claim all remain the real owners.
// Draining the in-memory player below represents hardware consumption; no device
// is opened. Cue timing has its separate existing speaker/stream regressions.
func TestNativeTranscriptKeepsRepeatedCaptureTailBeforeLocalEnd(t *testing.T) {
	c, f, id := connectedReply(t)
	c.speaker = speaker.New()
	c.sound = speaker.NewDriver(c.speaker)
	at := time.Unix(100, 0)
	c.now = func() time.Time { return at }
	ctx, cancel := context.WithCancel(context.Background())
	sent := make(chan struct{})
	go func() { defer close(sent); c.sends(ctx) }()
	t.Cleanup(func() {
		c.handle(event{kind: evCancel})
		streams.Wait()
		cancel()
		<-sent
	})
	drained := event{kind: evPlayed, reply: 41, at: at}
	var captures []string
	for round := range 2 {
		capture := &numberedCapture{Source: mic.New(), frames: make(chan []int16), subscribed: make(chan struct{}), at: at}
		c.source = capture
		c.handle(drained)
		if c.phase != phaseThinking || !c.followUp {
			t.Fatal("local playback drain did not prepare follow-up")
		}
		c.handle(next(t, c, evClawCaptureReady))
		waitClaw(t, capture.subscribed)
		c.claw.mu.Lock()
		captureID := c.claw.turn.captureID
		c.claw.mu.Unlock()
		for _, previous := range captures {
			if captureID == previous { t.Fatal("follow-up reused its capture identity") }
		}
		captures = append(captures, captureID)
		prefix, tail := int16(101+round*10), int16(102+round*10)
		feed := func(value int16, count int) {
			t.Helper()
			select {
			case capture.frames <- numberedPCM(value):
			case <-time.After(3*time.Second): t.Fatal("native capture stopped consuming microphone frames")
			}
			waitUntilT(t, func() bool {
				f.mu.Lock(); defer f.mu.Unlock()
				frames := 0
				for _, call := range f.calls {
					if call.method == "talk.session.appendAudio" && call.params["captureId"] == captureID && call.params["endOfInput"] != true { frames++ }
				}
				return frames == count
			})
		}
		feed(prefix, 1)
		c.claw.receive(json.RawMessage(fmt.Sprintf(`{"sessionId":%q,"type":"transcript","role":"user","final":true,"text":"Synthetic first clause."}`, id)))
		c.handle(next(t, c, evHeard))
		if c.phase != phaseListening || c.noSpeech != nil || c.deadline == nil {
			t.Fatal("a completed utterance transcript cut off the open native capture")
		}
		select { case <-f.ends: t.Fatal("transcript submitted an early endOfInput"); default: }
		feed(tail, 2)
		if round == 0 {
			// The actual local endpoint event owns submission, not transcription.
			c.handle(event{kind: evSpokeEnd, text: c.turn.ID()})
			streams.Wait()
			if c.phase != phaseThinking || waitFollowupID(t, f.ends) != id {
				t.Fatal("local endpoint failed to submit the same logical Talk")
			}
		} else {
			c.handle(event{kind: evCancel})
			streams.Wait()
			if c.phase != phaseIdle || waitFollowupID(t, f.closes) != id {
				t.Fatal("cancellation failed to retire ongoing native capture")
			}
		}
		f.mu.Lock()
		calls := append([]clawCall(nil), f.calls...)
		creates := f.serial
		f.mu.Unlock()
		var pcm []int16
		ends := 0
		for _, call := range calls {
			if call.method != "talk.session.appendAudio" || call.params["captureId"] != captureID { continue }
			if call.params["sessionId"] != id { t.Fatal("capture tail moved to another Talk") }
			if call.params["endOfInput"] == true { ends++; continue }
			if ends != 0 { t.Fatal("capture PCM followed its own end") }
			body, err := base64.StdEncoding.DecodeString(call.params["audioBase64"].(string))
			if err != nil || len(body) != nativeInputFrameSamples*2 { t.Fatal("invalid native capture frame", err) }
			for i:=0; i<len(body); i+=2 { pcm=append(pcm,int16(uint16(body[i])|uint16(body[i+1])<<8)) }
		}
		wantEnds := 1-round
		if creates != 1 || ends != wantEnds || !reflect.DeepEqual(pcm, numberedPCM(prefix,tail)) {
			t.Fatal("transcript split, lost, repeated or reordered ongoing capture", creates, ends, len(pcm))
		}
		if round == 1 { break }
		// Exercise the real reply/player claim before opening another capture.
		for _, packet := range []string{
			`{"sessionId":%q,"type":"event","talkEvent":{"type":"output.audio.started"}}`,
			`{"sessionId":%q,"type":"audio","audioBase64":"AQACAA=="}`,
			`{"sessionId":%q,"type":"event","talkEvent":{"type":"turn.ended"}}`,
		} { c.claw.receive(json.RawMessage(fmt.Sprintf(packet,id))) }
		for !c.nativeFinished { handleNext(t,c) }
		waitUntilT(t,func()bool{return c.speaker.Queued()>0})
		if c.phase != phaseReplying || c.pending != nil { t.Fatal("reply opened capture before local drain") }
		c.speaker.Drain()
		drained=next(t,c,evPlayed)
		if c.phase != phaseReplying { t.Fatal("local drain event was bypassed") }
	}
}
