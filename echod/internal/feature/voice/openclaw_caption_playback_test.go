package voice

import (
	"testing"
	"time"
)

func TestCaptionClockUsesAudibleSamplesAndSkipsUntaggedGaps(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var p captionPlaybackClock
	p.add(2400, captionMark{true, 4, 0})
	p.add(2400, captionMark{})
	p.add(2400, captionMark{true, 4, 4800})
	p.submitted = 7200
	var marks []captionMark
	emit := func(m captionMark) { marks = append(marks, m) }
	p.poll(at, 9600, 50*time.Millisecond, emit)
	if len(marks) != 1 || marks[0].sample != 1200 {
		t.Fatalf("not locally audible: %+v", marks)
	}
	p.poll(at.Add(100*time.Millisecond), 4800, 50*time.Millisecond, emit)
	if len(marks) != 1 {
		t.Fatal("invented a position in an untagged gap")
	}
	p.poll(at.Add(200*time.Millisecond), 0, 50*time.Millisecond, emit)
	if len(marks) != 2 || marks[1].sample != 6000 {
		t.Fatalf("lost later mapping: %+v", marks)
	}
	p.poll(at.Add(300*time.Millisecond), 0, 200*time.Millisecond, emit)
	if p.heard != 7200 {
		t.Fatal("silence kept charging hardware latency")
	}
}

func TestCaptionClockMusicDoesNotAcquireSpeechPosition(t *testing.T) {
	var p captionPlaybackClock
	p.add(4800, captionMark{true, 2, 0})
	p.submitted = 4800
	var emitted int
	// The mapped global point may be waiting music. The word registry, rather
	// than the clock, owns that distinction and must decline a text cursor.
	p.poll(time.Now(), 4800, 0, func(m captionMark) {
		emitted++
		if m.sample != 2400 {
			t.Fatal(m)
		}
	})
	if emitted != 1 {
		t.Fatal("clock lost a valid wire position")
	}
	p.add(1, captionMark{true, 2, 4800})
	if len(p.ranges) != 1 {
		t.Fatal("contiguous ranges did not coalesce")
	}
}

func TestCaptionClockBoundsOptionalFragments(t *testing.T) {
	var p captionPlaybackClock
	for i := 0; i < 2000; i++ {
		p.add(1, captionMark{true, 4, int64(i * 2)})
	}
	if len(p.ranges) != 512 || p.received != 2000 {
		t.Fatal("optional fragments changed audio or exceeded cap")
	}
}
