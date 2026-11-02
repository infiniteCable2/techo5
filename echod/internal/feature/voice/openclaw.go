package voice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/speaker"
	"github.com/HuskerMinion/techo5/echod/internal/layout"
	gw "github.com/HuskerMinion/techo5/echod/internal/lib/openclaw"
	"github.com/HuskerMinion/techo5/echod/internal/lib/safe"
)

type talkGateway interface {
	Ready() bool
	Call(context.Context, string, any, any) error
	Events() <-chan gw.Event
}

type clawInput struct {
	audio []byte
	end   bool
}
type clawOutput struct {
	event event
	epoch int
}
type clawFence struct{ generation, epoch int }
type clawTurn struct {
	captions      clawCaptions
	timings       *gw.WordTimings
	earlyReady    []json.RawMessage
	ctx           context.Context
	cancel        context.CancelFunc
	generation    int
	id            string
	input         chan clawInput
	inputEnded    bool
	outputEnded   bool
	followUpReady bool
	connection    uint64
	captionOffset int
	selected      *config.OpenClaw
	output        []clawOutput // bounded transport backlog; never claimed as played audio
	outputEpoch   int
	outputSpeech  bool
	outputWake    chan struct{}
	outputAt      time.Time // last accepted PCM, not a speech/status marker
}

// A bounded backlog, not a total reply limit. This also admits a two-minute
// TTS burst before the paced playback worker has consumed any of it.
const clawOutputLimit = 8192 // about 164 seconds of the Gateway's 20 ms frames

const clawOutputRate = 24000

// Keep ordinary scheduler lateness on the original audio timeline. Rebasing on
// every late wake accumulates that lateness and eventually drains the player.
// A long stall may catch up by at most ten 20 ms frames, below its 64-frame queue.
const clawCatchUp = 200 * time.Millisecond

type clawPlaybackClock struct{ next time.Time }

func (c *clawPlaybackClock) delay(now time.Time) time.Duration {
	if c.next.IsZero() || now.Sub(c.next) > clawCatchUp {
		c.next = now
	}
	return max(c.next.Sub(now), 0)
}

func (c *clawPlaybackClock) advance(bytes int) {
	c.next = c.next.Add(time.Duration(bytes) * time.Second / (clawOutputRate * 2))
}

// clawBackend translates transport events only. The existing microphone, endpoint,
// wake words, reply player, volume and speaker ownership remain the device owners.
type clawBackend struct {
	mu            sync.Mutex
	client        talkGateway
	ctx           context.Context
	key           string
	route         config.OpenClaw
	gen           int
	turn          *clawTurn
	post          func(event)
	playbackReady func() bool
	wantTimings   bool
}

func newClaw(post func(event)) *clawBackend {
	return &clawBackend{post: post, playbackReady: func() bool { return speaker.Get().Ready() }}
}
func (b *clawBackend) Name() string { return "OpenClaw Talk" }
func (b *clawBackend) Ready() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.client != nil && b.client.Ready()
}
func (b *clawBackend) generation() int { b.mu.Lock(); defer b.mu.Unlock(); return b.gen }
func (b *clawBackend) current(gen int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.turn != nil && b.turn.generation == gen && b.turn.ctx.Err() == nil
}
func (b *clawBackend) currentPlayback(gen, epoch int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.turn != nil && b.turn.generation == gen && b.turn.outputEpoch+1 == epoch && b.turn.ctx.Err() == nil && (b.client == nil || b.sameConnection(b.turn)) && b.sameSource(b.turn)
}

func (b *clawBackend) run(ctx context.Context) error {
	cfg := config.Get()
	if !cfg.OpenClaw.Enabled() {
		return nil
	}
	if cfg.OpenClaw.URL == "" || cfg.OpenClaw.SessionKey == "" {
		return errors.New("OpenClaw requires gateway URL and an explicitly selected device session key")
	}
	if cfg.Wake.OnHA.Stream || cfg.Wake.OnHA.Confirm {
		return errors.New("OpenClaw Talk requires the existing device-local wake engine; Home Assistant wake streaming is configured")
	}
	state := cfg.OpenClaw.StateDir
	if state == "" {
		state = filepath.Join(layout.StateDir, "openclaw")
	}
	client, err := gw.New(gw.Config{URL: cfg.OpenClaw.URL, StateDir: state,
		DisplayName: "OpenClaw Echo", BootstrapTokenFile: cfg.OpenClaw.BootstrapTokenFile, BootstrapRole: cfg.OpenClaw.BootstrapRole,
		PersonalAgentAccess: cfg.OpenClaw.PersonalAgentAccess})
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.client, b.ctx, b.key, b.route = client, ctx, cfg.OpenClaw.SessionKey, cfg.OpenClaw
	b.wantTimings = cfg.OpenClaw.OutputWordTimings
	b.mu.Unlock()
	safe.Go("OpenClaw Gateway", func() { _ = client.Run(ctx) })
	safe.Go("OpenClaw events", func() { b.events(ctx) })
	return nil
}

func (b *clawBackend) Start(string) error { return b.startFrom(-1) }
func (b *clawBackend) startFrom(gen int) error {
	return b.startInputFrom(gen, false)
}
func (b *clawBackend) startInputFrom(gen int, followUp bool, selected ...config.OpenClaw) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if gen >= 0 && gen != b.gen {
		return nil
	}
	if len(selected) == 1 && (config.Get().OpenClaw != selected[0] || selected[0].SessionKey != b.key || b.route.Enabled() && selected[0].URL != b.route.URL) {
		return errors.New("OpenClaw selected source changed")
	}
	if b.client == nil || !b.client.Ready() || b.ctx == nil || b.ctx.Err() != nil {
		return errors.New("OpenClaw node and Talk pairing are not ready")
	}
	if followUp && b.turn != nil && b.turn.followUpReady {
		if b.turn.ctx.Err() != nil || !b.sameConnection(b.turn) {
			return errors.New("OpenClaw follow-up connection changed")
		}
		if !b.playbackReady() {
			return errors.New("OpenClaw follow-up playback is unavailable")
		}
		b.turn.followUpReady = false
		if len(selected) == 1 {
			admitted := selected[0]
			b.turn.selected = &admitted
		}
		return nil
	}
	if b.turn != nil {
		b.turn.cancel()
	}
	b.gen++
	// Capture and processing have their own inactivity limits. A wall-clock
	// deadline here would cut off a healthy long reply, including buffered audio.
	ctx, cancel := context.WithCancel(b.ctx)
	t := &clawTurn{ctx: ctx, cancel: cancel, generation: b.gen, input: make(chan clawInput, 750), outputWake: make(chan struct{}, 1)}
	t.connection = b.connectionGeneration()
	if len(selected) == 1 {
		admitted := selected[0]
		t.selected = &admitted
	}
	b.turn = t
	safe.Go("OpenClaw turn", func() { b.transmit(t) })
	safe.Go("OpenClaw playback transport", func() { b.playback(t) })
	return nil
}

func (b *clawBackend) connectionGeneration() uint64 {
	if client, ok := b.client.(interface{ ConnectionGeneration() uint64 }); ok {
		return client.ConnectionGeneration()
	}
	return 0
}
func (b *clawBackend) sameConnection(t *clawTurn) bool {
	return b.client != nil && b.client.Ready() && t.connection == b.connectionGeneration()
}
func (b *clawBackend) sameSource(t *clawTurn) bool {
	return t.selected == nil || config.Get().OpenClaw == *t.selected
}

// Only the local player's completed drain can offer a connected follow-up.
// Retiring the old send scope before this call keeps its final Audio/End from
// crossing the new input boundary. The native Talk session and readiness stay.
func (b *clawBackend) keepForFollowUp() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.turn
	if t == nil || t.ctx.Err() != nil || t.id == "" || !t.inputEnded || !t.outputEnded || len(t.input) != 0 || !b.sameConnection(t) || !b.sameSource(t) || !b.playbackReady() {
		return false
	}
	t.inputEnded, t.outputEnded = false, false
	b.gen++
	t.generation = b.gen
	t.followUpReady = true
	t.output, t.outputAt, t.outputSpeech = nil, time.Time{}, false
	t.outputEpoch++ // revoke posted audio/end events from the completed reply
	t.captions = clawCaptions{}
	if t.timings != nil {
		t.captionOffset = len([]rune(t.timings.Text()))
		if t.captionOffset > 0 {
			t.captionOffset += 2 // the native registry separates visible blocks by two newlines
		}
	}
	return true
}

// queueOutput absorbs native TTS bursts without changing the existing player or
// its acoustic settings. A clear revokes both queued and already posted chunks.
func (b *clawBackend) queueOutput(t *clawTurn, e event) {
	b.mu.Lock()
	fence := clawFence{t.generation, t.outputEpoch}
	b.mu.Unlock()
	b.queueOutputFrom(t, fence, e)
}

func (b *clawBackend) matchesFence(t *clawTurn, fence clawFence) bool {
	return b.turn == t && t.ctx.Err() == nil && t.generation == fence.generation && t.outputEpoch == fence.epoch
}

// A transport event owns its capture fence from decode through publication.
// In particular the two turn.ended markers enter the backlog atomically.
func (b *clawBackend) queueOutputFrom(t *clawTurn, fence clawFence, events ...event) {
	b.mu.Lock()
	if !b.matchesFence(t, fence) {
		b.mu.Unlock()
		return
	}
	if len(events) == 1 && events[0].kind == evClawSpeaking && t.outputSpeech {
		b.mu.Unlock()
		return
	}
	if len(t.output)+len(events) > clawOutputLimit && (len(events) == 0 || events[0].kind != evClawClear) {
		b.mu.Unlock()
		b.emitFrom(t, fence, event{kind: evError, code: "openclaw-output-overflow"})
		return
	}
	for _, e := range events {
		if e.kind == evClawClear {
			t.output = nil
			t.outputEnded = false
			t.outputAt = time.Time{}
			t.outputEpoch++
			t.outputSpeech = false
		}
		if e.kind == evClawSpeaking {
			if t.outputSpeech {
				continue
			}
			t.outputSpeech = true
		}
		if e.kind == evClawSilent {
			t.outputSpeech = false
		}
		if e.kind == evStreamEnd {
			t.outputEnded = true
		}
		t.output = append(t.output, clawOutput{e, t.outputEpoch + 1})
		if e.kind == evStreamAudio && len(e.audio) > 0 {
			t.outputAt = time.Now()
		}
	}
	select {
	case t.outputWake <- struct{}{}:
	default:
	}
	b.mu.Unlock()
}

// outputRemaining protects against a timer selected just before queued PCM is
// handled. Control events and a live WebSocket alone are not audio progress.
func (b *clawBackend) outputRemaining(now time.Time, quiet time.Duration) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.turn
	if t == nil || t.ctx.Err() != nil || t.outputAt.IsZero() {
		return 0
	}
	for _, pending := range t.output {
		if pending.event.kind == evStreamAudio && len(pending.event.audio) > 0 {
			return quiet
		}
	}
	return max(0, quiet-max(0, now.Sub(t.outputAt)))
}

func (b *clawBackend) playback(t *clawTurn) {
	var clock clawPlaybackClock
	lastEpoch := 0
	for {
		b.mu.Lock()
		if b.turn != t || t.ctx.Err() != nil {
			b.mu.Unlock()
			return
		}
		if len(t.output) == 0 {
			b.mu.Unlock()
			select {
			case <-t.ctx.Done():
				return
			case <-t.outputWake:
				continue
			}
		}
		out := t.output[0]
		gen := t.generation
		t.output[0] = clawOutput{}
		t.output = t.output[1:]
		b.mu.Unlock()
		if out.epoch != lastEpoch {
			clock = clawPlaybackClock{}
			lastEpoch = out.epoch
		}
		if delay := clock.delay(time.Now()); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-t.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		if !b.currentPlayback(gen, out.epoch) {
			continue
		}
		out.event.clawPlayback = out.epoch
		b.emitFrom(t, clawFence{gen, out.epoch - 1}, out.event)
		if out.event.kind == evStreamAudio {
			clock.advance(len(out.event.audio))
		}
	}
}

func (b *clawBackend) Audio(frame []byte) error {
	if len(frame)%2 != 0 {
		return errors.New("OpenClaw input requires complete PCM16 samples")
	}
	return b.enqueue(clawInput{audio: append([]byte(nil), frame...)})
}
func (b *clawBackend) End() error { return b.enqueue(clawInput{end: true}) }
func (b *clawBackend) enqueue(input clawInput) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.turn == nil || b.turn.ctx.Err() != nil {
		return errors.New("OpenClaw turn is closed")
	}
	if b.client != nil && !b.sameConnection(b.turn) {
		return errors.New("OpenClaw Talk connection changed")
	}
	if !b.sameSource(b.turn) {
		return errors.New("OpenClaw selected source changed")
	}
	if b.turn.followUpReady {
		return errors.New("OpenClaw follow-up input has not started")
	}
	// Capture may already have posted its last frame when the conversation queues
	// End. Never reopen a committed input with that late frame or duplicate End.
	if b.turn.inputEnded {
		return nil
	}
	select {
	case b.turn.input <- input:
		if input.end {
			b.turn.inputEnded = true
		}
		return nil
	default:
		return errors.New("OpenClaw input queue is full")
	}
}
func (b *clawBackend) Stop() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.turn != nil {
		b.turn.cancel()
		b.turn = nil
	}
	b.gen++
	return nil
}
func (b *clawBackend) emit(t *clawTurn, e event) {
	b.mu.Lock()
	fence := clawFence{t.generation, t.outputEpoch}
	b.mu.Unlock()
	b.emitFrom(t, fence, e)
}
func (b *clawBackend) emitFrom(t *clawTurn, fence clawFence, e event) {
	b.mu.Lock()
	current := b.matchesFence(t, fence)
	e.clawGeneration = fence.generation
	post := b.post
	b.mu.Unlock()
	if current {
		post(e)
	}
}

func (b *clawBackend) transmit(t *clawTurn) {
	var created struct {
		OutputWordTimings *gw.OutputTimingsContract `json:"outputWordTimings"`
		ID                string                    `json:"sessionId"`
		ReadyMark         string                    `json:"readyMark"`
		Audio             struct {
			InputEncoding    string `json:"inputEncoding"`
			InputRate        int    `json:"inputSampleRateHz"`
			OutputEncoding   string `json:"outputEncoding"`
			OutputRate       int    `json:"outputSampleRateHz"`
			SupportsInputEnd bool   `json:"supportsInputEnd"`
		} `json:"audio"`
	}
	fail := func(method string) { b.emit(t, event{kind: evError, code: "openclaw-transport", msg: method}) }
	// Even a cancelled create can have committed. Keep the connection's normal
	// cleanup as the final owner of a response lost during disconnection.
	params := map[string]any{
		"mode": "stt-tts", "transport": "gateway-relay", "brain": "agent-consult", "sessionKey": b.key,
	}
	if b.wantTimings {
		params["capabilities"] = []string{"output-word-timings"}
	}
	err := b.client.Call(t.ctx, "talk.session.create", params, &created)
	if err != nil {
		fail("talk.session.create")
		return
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = b.client.Call(closeCtx, "talk.session.close", map[string]any{"sessionId": created.ID}, nil)
	}()
	if created.ID == "" || created.ReadyMark == "" || !created.Audio.SupportsInputEnd ||
		created.Audio.InputEncoding != "pcm16" || created.Audio.InputRate != 16000 ||
		created.Audio.OutputEncoding != "pcm16" || created.Audio.OutputRate != clawOutputRate {
		b.emit(t, event{kind: evError, code: "openclaw-speech-contract"})
		return
	}
	b.mu.Lock()
	if b.turn != t || t.ctx.Err() != nil {
		b.mu.Unlock()
		return
	}
	t.id = created.ID
	if b.wantTimings && created.OutputWordTimings != nil && created.OutputWordTimings.Accepted {
		t.timings = gw.NewWordTimings(created.ID, "", *created.OutputWordTimings)
		if t.timings == nil {
			b.mu.Unlock()
			b.emit(t, event{kind: evError, code: "openclaw-timing-contract"})
			return
		}
		for _, raw := range t.earlyReady {
			t.timings.Apply(raw)
		}
	}
	t.earlyReady = nil
	b.mu.Unlock()
	// Hardware services can fail independently; acknowledge only an acquired sink.
	// This is readiness, never physical delivery of future audio.
	if !b.playbackReady() {
		b.emit(t, event{kind: evError, code: "openclaw-playback-unavailable"})
		return
	}
	if err := b.client.Call(t.ctx, "talk.session.acknowledgeMark", map[string]any{
		"sessionId": created.ID, "markName": created.ReadyMark,
	}, nil); err != nil {
		fail("talk.session.acknowledgeMark")
		return
	}
	for {
		select {
		case <-t.ctx.Done():
			return
		case input := <-t.input:
			b.mu.Lock()
			currentSource := b.sameSource(t)
			b.mu.Unlock()
			if !currentSource {
				fail("OpenClaw selected source changed")
				return
			}
			params := map[string]any{"sessionId": created.ID, "audioBase64": base64.StdEncoding.EncodeToString(input.audio)}
			if input.end {
				params["endOfInput"] = true
			}
			if err := b.client.Call(t.ctx, "talk.session.appendAudio", params, nil); err != nil {
				fail("talk.session.appendAudio")
				return
			}
		}
	}
}

func (b *clawBackend) events(ctx context.Context) {
	network, _ := os.ReadFile("/run/techo5/network-dhcp-ssid")
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = b.Stop()
			return
		case <-tick.C:
			// The address owner publishes this fact; Talk must not manage DHCP.
			if next, err := os.ReadFile("/run/techo5/network-dhcp-ssid"); err == nil && len(next) <= 128 && len(next) > 0 && string(next) != string(network) {
				network = next
				if client, ok := b.client.(interface{ NetworkChanged() }); ok {
					client.NetworkChanged()
				}
			}
			b.mu.Lock()
			t := b.turn
			disconnected := t != nil && !b.sameConnection(t)
			b.mu.Unlock()
			if disconnected {
				if t != nil {
					b.emit(t, event{kind: evError, code: "openclaw-disconnected"})
				}
			}
		case e := <-b.client.Events():
			b.receive(e.Payload)
		}
	}
}

func (b *clawBackend) receive(raw json.RawMessage) {
	var e struct {
		SessionID    string `json:"sessionId"`
		Type         string `json:"type"`
		Audio        string `json:"audioBase64"`
		Role         string `json:"role"`
		Text         string `json:"text"`
		Final        bool   `json:"final"`
		TranscriptID string `json:"transcriptId"`
		TalkEvent    struct {
			Type    string `json:"type"`
			Payload struct {
				Text        string `json:"text"`
				Purpose     string `json:"purpose"`
				TextBlockID string `json:"textBlockId"`
				BlockFinal  bool   `json:"blockFinal"`
			} `json:"payload"`
		} `json:"talkEvent"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	b.mu.Lock()
	t := b.turn
	matches := t != nil && t.id != "" && t.id == e.SessionID && (b.client == nil || b.sameConnection(t)) && b.sameSource(t)
	var fence clawFence
	if matches {
		fence = clawFence{t.generation, t.outputEpoch}
	}
	if !matches && b.wantTimings && t != nil && t.id == "" && e.Type == "ready" && len(raw) <= 4096 && len(t.earlyReady) < 2 {
		t.earlyReady = append(t.earlyReady, append(json.RawMessage(nil), raw...))
	}
	b.mu.Unlock()
	if !matches {
		return
	}
	emit := func(e event) { b.emitFrom(t, fence, e) }
	queue := func(events ...event) { b.queueOutputFrom(t, fence, events...) }
	presentation := b.presentation(t, raw, fence)
	if presentation.drop {
		return
	}
	if presentation.textChanged {
		emit(event{kind: evClawCaption, text: presentation.text, canonical: true})
	}
	switch e.Type {
	case "audio":
		// The native relay emits at most 20 ms: 960 PCM16 bytes at 24 kHz.
		// Bound memory by bytes as well as queue entries, even for a faulty peer.
		if len(e.Audio) == 0 || len(e.Audio) > 1280 {
			emit(event{kind: evError, code: "openclaw-audio"})
			return
		}
		pcm, err := base64.StdEncoding.DecodeString(e.Audio)
		if err != nil || len(pcm) == 0 || len(pcm) > 960 || len(pcm)%2 != 0 {
			emit(event{kind: evError, code: "openclaw-audio"})
			return
		}
		// Preserve the native 24 kHz samples. The reply's stateful converter takes
		// them straight to the speaker rate, without the legacy 16 kHz voice path.
		queue(event{kind: evStreamAudio, audio: pcm, captionMark: presentation.mark})
	case "transcript":
		if e.Final && e.Role == "user" {
			b.mu.Lock()
			if b.matchesFence(t, fence) {
				t.captions = clawCaptions{}
			}
			b.mu.Unlock()
			emit(event{kind: evHeard, text: e.Text})
		}
		if e.Role == "assistant" && !presentation.canonical {
			b.caption(t, e.Text, e.TranscriptID, e.Final, e.TalkEvent.Type == "output.text.done", fence)
		}
		if e.Final && e.Role == "assistant" {
			emit(event{kind: evReplyText, text: e.Text, canonical: presentation.canonical})
		}
	case "clear":
		queue(event{kind: evClawClear})
	case "close":
		emit(event{kind: evError, code: "openclaw-closed"})
	case "event":
		switch e.TalkEvent.Type {
		case "output.text.delta", "output.text.done":
			if !presentation.canonical {
				p := e.TalkEvent.Payload
				if p.Purpose == "commentary" {
					if p.BlockFinal && p.TextBlockID != "" {
						b.caption(t, p.Text, p.TextBlockID, true, false, fence)
					}
				} else {
					b.caption(t, p.Text, "", e.TalkEvent.Type == "output.text.done", e.TalkEvent.Type == "output.text.done", fence)
				}
			}
		case "transcript.delta":
			if strings.TrimSpace(e.TalkEvent.Payload.Text) != "" {
				emit(event{kind: evClawInputSpeech})
			}
		case "output.audio.started", "output.audio.delta":
			queue(event{kind: evClawSpeaking})
		case "output.audio.done":
			queue(event{kind: evClawSilent})
		case "turn.ended":
			queue(event{kind: evStreamEnd}, event{kind: evRunEnd})
		case "session.error":
			emit(event{kind: evError, code: "openclaw-session"})
		}
	}
}
