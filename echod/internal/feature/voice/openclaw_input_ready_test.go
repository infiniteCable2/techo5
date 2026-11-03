package voice

import (
	"context"
	"encoding/base64"
	"reflect"
	"sync"
	"testing"

	"github.com/HuskerMinion/techo5/echod/internal/config"
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

func TestTalkInputReadyBuffersCopiedPCMAndBoundaryUntilCurrentAvailability(t *testing.T) {
	no, yes := false, true
	b, f, waiting, _ := inputReadyFixture(t, &no)
	pcm := []byte{1, 0, 2, 0}
	if err := b.Audio(pcm); err != nil {
		t.Fatal(err)
	}
	pcm[0] = 99
	if err := b.End(); err != nil {
		t.Fatal(err)
	}
	waitClaw(t, waiting)
	requireNoInputAppend(t, f)
	b.mu.Lock()
	id, capture := b.turn.id, b.turn.captureID
	b.mu.Unlock()
	ready := activitySnapshot("listening", "idle", 0, 0, 0)
	ready.Input.Available = &yes
	b.receive(activityPacket("another-session", 9, "session.activity", "", ready))
	b.receive(activityPacket(id, 1, "session.activity", "", ready))
	inputReadySnapshot(b, 2, nil) // Missing telemetry cannot erase known backpressure.
	b.mu.Lock()
	blocked := b.turn.activity.inputAvailable != nil && !*b.turn.activity.inputAvailable
	seq := b.turn.activity.snapshotSeq
	b.mu.Unlock()
	if !blocked || seq != 2 {
		t.Fatal("stale, wrong-session or omitted availability released input")
	}
	requireNoInputAppend(t, f)
	inputReadySnapshot(b, 3, &yes)
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

func TestTalkInputReadyCancellationClosesWithoutSendingBufferedInput(t *testing.T) {
	no := false
	b, f, waiting, stop := inputReadyFixture(t, &no)
	if err := b.Audio([]byte{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := b.End(); err != nil {
		t.Fatal(err)
	}
	waitClaw(t, waiting)
	stop()
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
			failed := make(chan event, 4)
			b.mu.Lock()
			b.post = func(e event) { failed <- e }
			b.mu.Unlock()
			if err := b.Audio([]byte{1, 0}); err != nil {
				t.Fatal(err)
			}
			if err := b.End(); err != nil {
				t.Fatal(err)
			}
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
			waitClaw(t, f.closed)
			requireNoInputAppend(t, f)
			select {
			case e := <-failed:
				if e.kind != evError || e.code == "" {
					t.Fatal("readiness failure was not visible", e)
				}
			default:
				t.Fatal("readiness failure was silent")
			}
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
