package mic

import (
	"fmt"
	"runtime"
	"slices"
	"testing"
	"time"
)

func historyFrameNumbers(pre []int16) []int16 {
	var out []int16
	for i := 0; i < len(pre); i += FrameSamples {
		out = append(out, pre[i])
	}
	return out
}

func TestListenHistoryJoinsOnce(t *testing.T) {
	for _, d := range []time.Duration{0, History, Confirming} {
		t.Run(d.String(), func(t *testing.T) {
			s := testSource()
			s.mixer = &reusedBuffer{}
			s.broadcast(rawFrame(0, false))
			frames, pre, stop := s.ListenHistory("test", d)
			defer stop()
			s.broadcast(rawFrame(1, false))
			got := append(historyFrameNumbers(pre), (<-frames)[0])
			want := []int16{1, 2}
			if d == 0 {
				want = []int16{2}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("joined frames %v, want %v", got, want)
			}
			stop()
			stop()
			if _, ok := <-frames; ok {
				t.Fatal("unsubscribe did not close channel")
			}
			if len(s.listeners) != 0 {
				t.Fatal("subscription leaked")
			}
		})
	}
}

func TestListenSinceJoinsAndRejectsUnavailableBoundary(t *testing.T) {
	s := testSource()
	s.mixer = &reusedBuffer{}
	at := time.Now().Add(-200 * time.Millisecond)
	s.broadcastAt(rawFrame(0, false), at.Add(20*time.Millisecond))
	frames, pre, stop, err := s.ListenSince("absolute", at)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	s.broadcastAt(rawFrame(1, false), at.Add(40*time.Millisecond))
	if got := append(historyFrameNumbers(pre), (<-frames)[0]); !slices.Equal(got, []int16{1, 2}) {
		t.Fatal("absolute snapshot lost or repeated its boundary", got)
	}
	stop()
	for _, after := range []time.Time{time.Now().Add(-Confirming - time.Second), time.Now().Add(time.Minute)} {
		if _, _, _, err := s.ListenSince("unavailable", after); err == nil {
			t.Fatal("unretained boundary was silently truncated")
		}
	}
	if len(s.listeners) != 0 {
		t.Fatal("rejected boundary left a listener")
	}
}

func TestListenSinceDoesNotReclockPausedOrDelayedCapture(t *testing.T) {
	s := testSource()
	s.mixer = &reusedBuffer{}
	at := time.Now().Add(-200 * time.Millisecond)
	s.broadcastAt(rawFrame(0, false), at.Add(20*time.Millisecond))
	cutoff := at.Add(100 * time.Millisecond)
	frames, pre, stop, err := s.ListenSince("paused", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if len(pre) != 0 {
		t.Fatal("pre-cue frame was relabeled after the cutoff", historyFrameNumbers(pre))
	}
	// This read completed before the cutoff but reached the broadcaster late.
	s.broadcastAt(rawFrame(1, false), cutoff.Add(-10*time.Millisecond))
	select {
	case <-frames:
		t.Fatal("delayed pre-cue frame reached live capture")
	default:
	}
	s.broadcastAt(rawFrame(2, false), cutoff.Add(20*time.Millisecond))
	select {
	case frame := <-frames:
		if frame[0] != 3 {
			t.Fatal("first post-cue frame was lost", frame[0])
		}
	default:
		t.Fatal("post-cue frame did not reach live capture")
	}
}

func TestListenSinceRequiresActualRetainedCoverage(t *testing.T) {
	for _, mode := range []string{"missing-prefix", "capture-gap", "restart", "ring-overwrite"} {
		t.Run(mode, func(t *testing.T) {
			s := testSource()
			s.mixer = &reusedBuffer{}
			at := time.Now().Add(-time.Second)
			cutoff := at
			switch mode {
			case "missing-prefix":
				s.broadcastAt(rawFrame(0, false), at.Add(40*time.Millisecond))
			case "capture-gap":
				s.broadcastAt(rawFrame(0, false), at.Add(20*time.Millisecond))
				s.broadcastAt(rawFrame(1, false), at.Add(100*time.Millisecond))
			case "restart":
				s.broadcastAt(rawFrame(0, false), at.Add(20*time.Millisecond))
				s.history.discontinue(at.Add(40 * time.Millisecond))
			case "ring-overwrite":
				at = time.Now().Add(-Confirming)
				cutoff = at.Add(10 * time.Millisecond)
				for i := range 101 {
					s.broadcastAt(rawFrame(i, false), at.Add(time.Duration(i+1)*20*time.Millisecond))
				}
			}
			if _, _, _, err := s.ListenSince("unavailable", cutoff); err == nil {
				t.Fatal("unproven prefix was silently accepted")
			}
			if len(s.listeners) != 0 {
				t.Fatal("rejected boundary registered a listener")
			}
		})
	}
}

func TestListenSinceKeepsFrameGranularJitterAndOmitsStraddlingFrame(t *testing.T) {
	s := testSource()
	s.mixer = &reusedBuffer{}
	at := time.Now().Add(-200 * time.Millisecond)
	s.broadcastAt(rawFrame(0, false), at.Add(20*time.Millisecond))
	s.broadcastAt(rawFrame(1, false), at.Add(42*time.Millisecond))
	_, pre, stop, err := s.ListenSince("jitter", at)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if got := historyFrameNumbers(pre); !slices.Equal(got, []int16{1, 2}) {
		t.Fatal("healthy frame jitter lost audio", got)
	}
	_, pre, stop, err = s.ListenSince("straddling", at.Add(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if got := historyFrameNumbers(pre); !slices.Equal(got, []int16{2}) {
		t.Fatal("cue-straddling frame was retained", got)
	}
}

// Pause the real snapshot on history.mu. Capture must wait for the whole
// subscription/snapshot transaction, so the next frame appears only as live.
func TestListenHistorySerializesCapture(t *testing.T) {
	s := testSource()
	s.mixer = &reusedBuffer{}
	s.broadcast(rawFrame(0, false))
	s.history.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.history.mu.Unlock()
		}
	}()
	type joined struct {
		pre    []int16
		frames <-chan []int16
		stop   func()
	}
	acquired := make(chan joined, 1)
	go func() {
		frames, pre, stop := s.ListenHistory("paused", Confirming)
		acquired <- joined{pre, frames, stop}
	}()
	until := time.Now().Add(time.Second)
	for s.mu.TryLock() {
		s.mu.Unlock()
		if time.Now().After(until) {
			t.Fatal("snapshot did not acquire Source.mu")
		}
		runtime.Gosched()
	}
	captured := make(chan struct{})
	go func() { s.broadcast(rawFrame(1, false)); close(captured) }()
	s.history.mu.Unlock()
	locked = false
	var got joined
	select {
	case got = <-acquired:
	case <-time.After(time.Second):
		t.Fatal("snapshot blocked")
	}
	defer got.stop()
	select {
	case <-captured:
	case <-time.After(time.Second):
		t.Fatal("capture blocked")
	}
	numbers := append(historyFrameNumbers(got.pre), (<-got.frames)[0])
	if !slices.Equal(numbers, []int16{1, 2}) {
		t.Fatalf("joined frames %v, want [1 2]", numbers)
	}
}

func TestListenHistoryConcurrentCapture(t *testing.T) {
	s := testSource()
	s.mixer = &reusedBuffer{}
	for range 100 {
		// Both possible lock orders are valid: the boundary frame is in history
		// or live. It must occur exactly once, and capture's final frame follows it.
		start := make(chan struct{})
		done := make(chan struct{})
		go func() { <-start; s.broadcast(rawFrame(0, false)); close(done) }()
		close(start)
		frames, pre, stop := s.ListenHistory("concurrent", 20*time.Millisecond)
		<-done
		s.broadcast(rawFrame(0, false))
		got := historyFrameNumbers(pre)
		for len(frames) > 0 {
			got = append(got, (<-frames)[0])
		}
		stop()
		for i := 1; i < len(got); i++ {
			if got[i] != got[i-1]+1 {
				t.Fatalf("gap or overlap: %v", got)
			}
		}
	}
	if s.Dropped() != 0 {
		t.Fatal("fixture dropped frames")
	}
}

func BenchmarkHistorySubscription(b *testing.B) {
	for _, d := range []time.Duration{0, History, Confirming} {
		b.Run(fmt.Sprint(d), func(b *testing.B) {
			s := testSource()
			s.history.add(make([]int16, historySamples))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_, _, stop := s.ListenHistory("bench", d)
				stop()
			}
		})
	}
}

func BenchmarkHistoryBroadcast(b *testing.B) {
	s := testSource()
	s.mixer = Center{}
	raw := rawFrame(0, false)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.broadcast(raw)
	}
}
