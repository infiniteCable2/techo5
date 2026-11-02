package voice

import (
	"context"
	"encoding/json"
	"testing"
)

func TestClawCaptionsPreserveCommentaryAndParagraphs(t *testing.T) {
	c := clawCaptions{}
	c.update("Ich schaue nach.", "commentary-1", true, false)
	c.update("Ich prüfe noch etwas.", "commentary-2", true, false)
	c.update("Die Antwort", "", false, false)
	got := c.update("Die Antwort ist da.", "answer-1", true, false)
	want := "Ich schaue nach.\n\nIch prüfe noch etwas.\n\nDie Antwort ist da."
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := c.update("Die Antwort ist da.", "answer-1", true, false); got != want {
		t.Fatal("duplicate delivery added a second paragraph")
	}
	if got := c.update("Die Antwort ist da.", "", true, true); got != want {
		t.Fatal("final summary replaced commentary or duplicated the answer")
	}
	c.update("Weiterer sichtbarer Fortschritt", "", false, false)
	if got := c.update("Die Antwort ist da.", "answer-1", true, false); got != want+"\n\nWeiterer sichtbarer Fortschritt" {
		t.Fatal("retransmitted completed block erased a newer partial block")
	}
}

func TestClawCaptionDoesNotChangeVoicePhase(t *testing.T) {
	c := thinkingTurn(t)
	c.be = c.claw
	c.handle(event{kind: evClawCaption, text: "Visible progress."})
	if c.phase != phaseThinking || c.shown.Reply != "Visible progress." {
		t.Fatal("display text changed the processing state or was not displayed")
	}
	c.enter(phaseIdle)
	c.handle(event{kind: evClawCaption, text: "Late progress."})
	if c.shown.Reply == "Late progress." {
		t.Fatal("ended turn admitted late progress")
	}
}

func TestClawReceivesVisibleTextWithoutStartingPlayback(t *testing.T) {
	f := &fakeClaw{ready: true}
	b, posted := newTestClaw(t, f)
	b.gen = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.turn = &clawTurn{ctx: ctx, cancel: cancel, generation: 1, id: "current"}
	b.receive(json.RawMessage(`{"sessionId":"old","type":"transcript","role":"assistant","final":false,"text":"Old progress"}`))
	select {
	case <-posted:
		t.Fatal("old session reached display")
	default:
	}
	b.receive(json.RawMessage(`{"sessionId":"current","type":"transcript","role":"assistant","final":false,"text":"Checking"}`))
	if e := <-posted; e.kind != evClawCaption || e.text != "Checking" || e.clawGeneration != 1 {
		t.Fatal("native partial text did not remain a fenced display-only event")
	}
	select {
	case <-posted:
		t.Fatal("partial text produced an audio/turn event")
	default:
	}
}

func TestClawCommentaryBlocksFromTalkRelayRemainBeforeFinalAnswer(t *testing.T) {
	f := &fakeClaw{ready: true}
	b, posted := newTestClaw(t, f)
	b.gen = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.turn = &clawTurn{ctx: ctx, cancel: cancel, generation: 1, id: "current"}
	raw := json.RawMessage(`{"sessionId":"current","type":"event","talkEvent":{"type":"output.text.delta","payload":{"text":"Checking.","purpose":"commentary","textBlockId":"block-1","blockFinal":true}}}`)
	b.receive(raw)
	if e := <-posted; e.kind != evClawCaption || e.text != "Checking." {
		t.Fatal("commentary did not reach display", e)
	}
	b.receive(raw)
	select {
	case <-posted:
		t.Fatal("duplicate commentary delivered twice")
	default:
	}
	b.receive(json.RawMessage(`{"sessionId":"current","type":"transcript","role":"assistant","final":true,"text":"Finished.","talkEvent":{"type":"output.text.done"}}`))
	if e := <-posted; e.kind != evClawCaption || e.text != "Checking.\n\nFinished." {
		t.Fatal("final answer erased commentary", e)
	}
	if e := <-posted; e.kind != evReplyText {
		t.Fatal("final answer lost normal turn effects")
	}
}
