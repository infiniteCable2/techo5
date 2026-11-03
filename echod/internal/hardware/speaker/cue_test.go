package speaker

import (
	"context"
	"reflect"
	"testing"
	"time"
)

type cuePlayback struct{}

func (*cuePlayback) Write(b []byte) (int, error) { return len(b), nil }
func (*cuePlayback) Close() error                { return nil }
func (*cuePlayback) Delay() (int, error)         { return 0, nil }

func TestCueBoundaryUsesSuccessfulHandoffNotRenderOrConsumerStart(t *testing.T) {
	p := newTestPlayer()
	p.pb = &cuePlayback{}
	other := newTestPlayer()
	other.pb = &cuePlayback{}
	notes := []Note{{Freq: 500, Ms: 20}}
	other.Chime(.3, notes...)
	cue := p.ChimeCue(.3, 10*time.Millisecond, notes...)
	if !reflect.DeepEqual(p.pending, other.pending) || p.overlay != other.overlay {
		t.Fatal("observation changed cue waveform/overlay")
	}
	buf := make([]byte, period*Channels*2)
	var mark *cueMark
	for mark == nil {
		mark = p.fill(buf)
	}
	select {
	case <-cue.done:
		t.Fatal("rendering falsely completed handoff")
	default:
	}
	at := time.Unix(100, 0)
	if err := p.send(context.Background(), p.pb, buf); err != nil {
		t.Fatal(err)
	}
	p.handedCue(mark, at)
	end, err := cue.Wait(context.Background())
	want := at.Add(HardwareTail - time.Duration(mark.after)*time.Second/Rate)
	if err != nil || !end.Equal(want) {
		t.Fatal("handoff boundary lost", end, err)
	}
	late, err := cue.Wait(context.Background())
	if err != nil || !late.Equal(end) {
		t.Fatal("late startup restarted suppression")
	}
}

func TestCueRetirementAndWaitCancellationDoNotFabricatePlayback(t *testing.T) {
	for _, action := range []string{"drain", "take", "replace", "close"} {
		t.Run(action, func(t *testing.T) {
			p := newTestPlayer()
			p.pb = &cuePlayback{}
			cue := p.ChimeCue(.3, 10*time.Millisecond, Note{Freq: 500, Ms: 20})
			buf := make([]byte, period*Channels*2)
			var mark *cueMark
			for mark == nil {
				mark = p.fill(buf)
			}
			switch action {
			case "drain":
				p.Drain()
			case "take":
				p.Take()
			case "replace":
				p.ChimeCue(.3, 10*time.Millisecond, Note{Freq: 600, Ms: 20})
			case "close":
				_ = p.Close()
			}
			p.handedCue(mark, time.Now())
			if _, err := cue.Wait(context.Background()); err == nil {
				t.Fatal("retired cue claimed a boundary")
			}
		})
	}
	p := newTestPlayer()
	p.pb = &cuePlayback{}
	cue := p.ChimeCue(.3, 10*time.Millisecond, Note{Freq: 500, Ms: 20})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cue.Wait(ctx); err != context.Canceled {
		t.Fatal("cancel did not unblock cue", err)
	}
	p.Drain()
}

func TestCueExternalSinkRemainsExplicitlyUnobserved(t *testing.T) {
	p := newTestPlayer()
	p.pb = &cuePlayback{}
	p.SetSink(&fakeSink{}, "synthetic", Rate, Channels)
	defer p.SetSink(nil, "", 0, 0)
	cue := p.ChimeCue(.3, 10*time.Millisecond, Note{Freq: 500, Ms: 20})
	if _, err := cue.Wait(context.Background()); err == nil {
		t.Fatal("local silence claimed remote playback")
	}
	if len(p.pending) == 0 {
		t.Fatal("unsupported observation suppressed configured sound")
	}
}
