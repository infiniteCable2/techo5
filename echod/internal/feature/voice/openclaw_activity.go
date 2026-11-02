package voice

import (
	"encoding/json"
	"log/slog"

	"github.com/HuskerMinion/techo5/echod/internal/feature/activity"
)

// Activity is an observation of native owners, never a command or proof that
// transport audio has sounded. All mutable projection state belongs to b.mu.
type clawActivitySnapshot struct {
	Version    int    `json:"version"`
	Connection string `json:"connection"`
	Input      struct {
		Phase     string `json:"phase"`
		CaptureID string `json:"captureId"`
		Pending   int    `json:"pending"`
	} `json:"input"`
	Work struct {
		Pending int `json:"pending"`
		Queued  int `json:"queued"`
		Running int `json:"running"`
	} `json:"work"`
	Output struct {
		Generation int    `json:"generation"`
		State      string `json:"state"`
		Purpose    string `json:"purpose"`
	} `json:"output"`
}

type clawActivityView struct {
	known    bool
	snapshot clawActivitySnapshot
	terminal string
	settled  bool
}

type clawActivity struct {
	supported                    bool
	settlementQueued             bool
	seq, snapshotSeq, outcomeSeq uint64
	snapshot                     clawActivitySnapshot
	terminal                     string
}

func (a *clawActivity) nextCapture() {
	a.terminal, a.outcomeSeq = "", 0
	a.settlementQueued = false
}

func (a *clawActivity) view() clawActivityView {
	s := a.snapshot
	quiet := s.Connection == "ready" && s.Input.Phase == "listening" && s.Input.Pending == 0 && s.Work.Pending == 0 && s.Work.Queued == 0 && s.Work.Running == 0 && (s.Output.State == "idle" || s.Output.State == "submitted")
	terminal := a.terminal == "no-input" || a.terminal == "rejected" || a.terminal == "transcribed" || a.terminal == "failed" || a.terminal == "cancelled"
	return clawActivityView{known: a.supported && a.snapshotSeq != 0, snapshot: s, terminal: a.terminal,
		settled: terminal && a.snapshotSeq >= a.outcomeSeq && quiet}
}

func (s clawActivitySnapshot) valid() bool {
	if s.Version != 1 || s.Input.Pending < 0 || s.Work.Pending < 0 || s.Work.Queued < 0 || s.Work.Running < 0 || s.Output.Generation < 0 {
		return false
	}
	switch s.Connection {
	case "preparing", "ready", "closed":
	default:
		return false
	}
	switch s.Input.Phase {
	case "listening", "receiving", "candidate", "speech", "transcribing", "unknown":
	default:
		return false
	}
	switch s.Output.State {
	case "idle", "preparing", "streaming", "submitted":
	default:
		return false
	}
	switch s.Output.Purpose {
	case "", "speech", "waiting", "greeting":
	default:
		return false
	}
	return len(s.Input.CaptureID) <= 128
}

func completeActivitySnapshot(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	for _, name := range []string{"version", "connection"} {
		if len(fields[name]) == 0 || string(fields[name]) == "null" {
			return false
		}
	}
	for owner, required := range map[string][]string{
		"input": {"phase", "pending"}, "work": {"pending", "queued", "running"}, "output": {"generation", "state"},
	} {
		var values map[string]json.RawMessage
		if json.Unmarshal(fields[owner], &values) != nil {
			return false
		}
		for _, name := range required {
			if len(values[name]) == 0 || string(values[name]) == "null" {
				return false
			}
		}
	}
	return true
}

func (b *clawBackend) receiveActivity(t *clawTurn, fence clawFence, raw json.RawMessage) {
	var envelope struct {
		TalkEvent struct {
			SessionID string          `json:"sessionId"`
			Seq       uint64          `json:"seq"`
			CaptureID string          `json:"captureId"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		} `json:"talkEvent"`
	}
	if len(raw) > 4096 || json.Unmarshal(raw, &envelope) != nil {
		return
	}
	e := envelope.TalkEvent
	b.mu.Lock()
	if !b.matchesFence(t, fence) || !t.activity.supported || e.SessionID != t.id || e.Seq == 0 || e.Seq <= t.activity.seq {
		b.mu.Unlock()
		return
	}
	a := &t.activity
	switch e.Type {
	case "session.activity":
		var snapshot clawActivitySnapshot
		if json.Unmarshal(e.Payload, &snapshot) != nil || !snapshot.valid() || !completeActivitySnapshot(e.Payload) {
			b.mu.Unlock()
			return
		}
		a.snapshot, a.snapshotSeq = snapshot, e.Seq
	case "input.outcome":
		var outcome struct {
			Version int    `json:"version"`
			Scope   string `json:"scope"`
			Outcome string `json:"outcome"`
		}
		if json.Unmarshal(e.Payload, &outcome) != nil || outcome.Version != 1 {
			b.mu.Unlock()
			return
		}
		if outcome.Scope != "capture" && outcome.Scope != "utterance" {
			b.mu.Unlock()
			return
		}
		switch outcome.Outcome {
		case "no-input", "rejected", "transcribed", "failed", "cancelled", "unknown":
		default:
			b.mu.Unlock()
			return
		}
		if outcome.Scope == "capture" && e.CaptureID != "" && len(e.CaptureID) <= 128 && e.CaptureID == t.captureID && a.outcomeSeq == 0 {
			a.terminal, a.outcomeSeq = outcome.Outcome, e.Seq
		}
	default:
		b.mu.Unlock()
		return
	}
	a.seq = e.Seq
	settle := a.view().settled
	b.mu.Unlock()
	b.emitFrom(t, fence, event{kind: evClawActivity})
	if settle {
		b.queueOutputFrom(t, fence, event{kind: evClawActivityEnd})
	}
}

func (b *clawBackend) activityView() (clawActivityView, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.turn == nil {
		return clawActivityView{}, false
	}
	return b.turn.activity.view(), b.turn.activity.supported
}

func (c *conversation) nativeProgressStatus() string {
	if !c.nativeTalk() || c.phase == phaseIdle || c.phase == phaseListening {
		return ""
	}
	view, supported := c.claw.activityView()
	status := ""
	if supported {
		status = "Processing…"
		if view.known {
			s := view.snapshot
			switch {
			case s.Work.Running > 0:
				status = "Thinking…"
			case s.Work.Pending > 0 || s.Work.Queued > 0:
				status = "Waiting…"
			case s.Input.Pending > 0 || s.Input.Phase == "transcribing":
				status = "Transcribing…"
			}
		}
	}
	return status
}

func (c *conversation) showNativeActivity() {
	status := c.nativeProgressStatus()
	if c.shown.NativeStatus != status {
		c.shown.NativeStatus = status
		Changed.Emit(c.shown)
	}
}

func (b *clawBackend) consumeActivityEnd() (clawActivityView, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.turn == nil || !b.turn.activity.supported {
		return clawActivityView{}, false
	}
	b.turn.activity.settlementQueued = false
	view := b.turn.activity.view()
	if view.settled {
		b.turn.outputEnded = true
	}
	return view, view.settled
}

func (c *conversation) nativeActivityEnded() {
	view, settled := c.claw.consumeActivityEnd()
	if !settled || c.phase == phaseIdle || c.phase == phaseListening {
		return
	}
	// A capture outcome cannot revoke PCM or reopen capture over the player's
	// buffer. This marker follows queued PCM; normal follow-up admission remains
	// the local evPlayed path, including a last waiting-music tail.
	if c.phase == phaseReplying {
		c.turnEvent(event{kind: evStreamEnd})
		return
	}
	slog.Info("native input settled without reply", "outcome", view.terminal)
	how := activity.NoSpeech
	if view.terminal == "transcribed" {
		how = activity.Answered
	}
	if view.terminal == "failed" {
		how = activity.Failed
	}
	if view.terminal == "cancelled" {
		how = activity.Canceled
	}
	c.clearPending()
	c.idle("native input settled", how)
}
