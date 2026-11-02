//go:build !dot && !spot

package display

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/hardware/touch"
	"github.com/HuskerMinion/techo5/echod/internal/lib/fonts"
)

func TestAnswerPreview(t *testing.T) {
	dir := os.Getenv("TECHO5_ANSWER_PREVIEW_DIR")
	if dir == "" {
		t.Skip("set a private directory to write synthetic previews")
	}
	for name, text := range map[string]string{
		"short": "Ein kurzer Text bleibt gut lesbar.",
		"long":  strings.Repeat("Das ist ein langer Antworttext. Die Schrift bleibt gut lesbar, und wir können in Ruhe weiterlesen. ", 8) + "Hier endet die Antwort.",
	} {
		img := image.NewRGBA(image.Rect(0, 0, showWide, showHigh))
		r := newRenderer(img)
		r.draw(scene{now: time.Date(2026, 11, 1, 16, 24, 0, 0, time.UTC), phase: "replying", heard: "Erkläre mir bitte ein interessantes Thema.", reply: text})
		f, err := os.Create(filepath.Join(dir, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if name == "long" {
			r.answer.scroll.manual = true
			r.answer.scroll.position = 1e6
			r.draw(scene{now: time.Date(2026, 11, 1, 16, 24, 5, 0, time.UTC), phase: "replying", heard: "Erkläre mir bitte ein interessantes Thema.", reply: text})
			f, err := os.Create(filepath.Join(dir, "long-end.png"))
			if err != nil {
				t.Fatal(err)
			}
			if err := png.Encode(f, img); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestAnswerEmojiFallbackPreview(t *testing.T) {
	dir := os.Getenv("TECHO5_EMOJI_FONT_DIR")
	if dir == "" {
		t.Skip("set a private font directory to qualify an optional emoji fallback")
	}
	old := fonts.Dir
	fonts.Dir = dir
	fonts.Rescan()
	t.Cleanup(func() { fonts.Dir = old; fonts.Rescan() })
	for _, c := range "😊🙂😀👍❤" {
		if !fonts.OwnDraws(c, "") {
			t.Fatalf("fallback does not draw U+%04X", c)
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, showWide, showHigh))
	newRenderer(img).draw(scene{now: time.Date(2026, 11, 1, 16, 24, 0, 0, time.UTC), phase: "replying", reply: "Gut lesbar mit Smileys: 😊 🙂 😀 👍 ❤"})
	if out := os.Getenv("TECHO5_ANSWER_PREVIEW_DIR"); out != "" {
		f, err := os.Create(filepath.Join(out, "emoji.png"))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAnswerLayoutKeepsAllReplyLines(t *testing.T) {
	for _, size := range []image.Point{{960, 480}, {1280, 800}} {
		r := newRenderer(image.NewRGBA(image.Rectangle{Max: size}))
		text := strings.Repeat("Eine gut lesbare Antwort mit gleichbleibender Schrift. ", 30) + "Das Ende ist erreichbar."
		l := r.layoutAnswer(strings.Repeat("Eine lange Frage. ", 20), text)
		questions, replies := 0, 0
		for _, line := range l.lines {
			if line.question {
				questions++
			} else {
				replies++
			}
		}
		if questions > 2 || replies != len(r.wrap(r.answerBody, text, r.w-2*r.margin)) {
			t.Fatal("question hid the answer or reply lines were shrunk/cut")
		}
		if l.height <= r.h || !strings.HasSuffix(l.lines[len(l.lines)-1].text, "erreichbar.") {
			t.Fatal("long answer is incomplete")
		}
		first := &l.lines[0]
		again := r.layoutAnswer(l.heard, text)
		if first != &again.lines[0] {
			t.Fatal("unchanged text was wrapped again")
		}
	}
}

func TestAnswerScrollPacePauseAndLimits(t *testing.T) {
	at := time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC)
	s := answerScroll{}
	focus := func(cursor float64) float64 { return cursor }
	for i := 0; i <= 5; i++ {
		s.advance(at.Add(time.Duration(i)*time.Second), true, 100, 73, focus)
	}
	if s.position != 0 {
		t.Fatal("did not wait before scrolling")
	}
	s.advance(at.Add(6*time.Second), true, 100, 73, focus)
	before := s.position
	if before <= 0 || before >= 100 {
		t.Fatal("reader did not follow its estimated source position")
	}
	s.advance(at.Add(20*time.Second), false, 100, 73, focus)
	s.advance(at.Add(21*time.Second), true, 100, 73, focus)
	if s.position != before {
		t.Fatal("processing pause advanced the answer")
	}
	s.advance(at.Add(22*time.Second), true, 100, 73, focus)
	if s.position <= before {
		t.Fatal("reading did not resume")
	}
	s.manual = true
	before = s.position
	s.advance(at.Add(23*time.Second), true, 100, 73, focus)
	if s.position != before || s.moving() {
		t.Fatal("automatic scroll fought manual reading")
	}
	s.advance(at.Add(24*time.Second), true, 10, 73, focus)
	if s.position != 10 {
		t.Fatal("scroll escaped content bounds")
	}
}

func TestAnswerParagraphsKeepAnEmptyLine(t *testing.T) {
	r := newRenderer(image.NewRGBA(image.Rect(0, 0, 960, 480)))
	l := r.layoutAnswer("", "Commentary.\n\nThe answer.")
	if len(l.lines) != 2 || l.lines[1].top-l.lines[0].top != r.s(2*answerLineHeight) {
		t.Fatal("paragraph separation collapsed")
	}
	r.draw(scene{phase: "thinking", reply: "Visible commentary."})
	if !r.answer.visible {
		t.Fatal("processing phase hid commentary")
	}
}

func TestAnswerPausesWithLocalPlayback(t *testing.T) {
	r := newRenderer(image.NewRGBA(image.Rect(0, 0, 960, 480)))
	at := time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC)
	s := scene{phase: "replying", reply: strings.Repeat("Eine lange Antwort. ", 50), playbackKnown: true, speaking: true}
	for i := 0; i <= 6; i++ {
		s.now = at.Add(time.Duration(i) * time.Second)
		r.draw(s)
	}
	before := r.answer.scroll.position
	if before <= 0 {
		t.Fatal("audible speech did not advance the provisional reader")
	}
	s.speaking = false
	s.now = at.Add(7 * time.Second)
	r.draw(s)
	s.now = at.Add(15 * time.Second)
	r.draw(s)
	if r.answer.scroll.position != before || r.answerAnimating() {
		t.Fatal("waiting music advanced the text")
	}
	s.speaking = true
	s.now = at.Add(16 * time.Second)
	r.draw(s)
	if r.answer.scroll.position != before {
		t.Fatal("resume counted the silent pause")
	}
	s.now = at.Add(17 * time.Second)
	r.draw(s)
	if r.answer.scroll.position <= before {
		t.Fatal("reader did not resume with speech")
	}
}

func TestAnswerTouchAndStreamedText(t *testing.T) {
	r := newRenderer(image.NewRGBA(image.Rect(0, 0, 960, 480)))
	s := scene{now: time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC), phase: "replying", reply: strings.Repeat("Ein langer Antworttext. ", 50)}
	r.draw(s)
	if r.scrollAnswer(touch.Gesture{Kind: touch.SwipeDown, Y: 0}) {
		t.Fatal("answer stole the Settings gesture")
	}
	if !r.scrollAnswer(touch.Gesture{Kind: touch.SwipeUp, Y: 200}) {
		t.Fatal("long answer did not consume its swipe")
	}
	pos := r.answer.scroll.position
	if pos <= 0 || !r.answer.scroll.manual {
		t.Fatal("swipe did not pause automatic reading")
	}
	s.reply += " Noch ein Stück der Antwort."
	s.now = s.now.Add(time.Second)
	r.draw(s)
	if r.answer.scroll.position != pos || !r.answer.scroll.manual {
		t.Fatal("streamed text reset the reader's position")
	}
	s.phase = "idle"
	r.draw(s)
	if r.scrollAnswer(touch.Gesture{Kind: touch.SwipeUp, Y: 200}) {
		t.Fatal("hidden answer stole volume gesture")
	}
	s.phase = "listening"
	r.beginAnswerFrame(s)
	if r.answer.scroll.position != 0 || r.answer.scroll.manual {
		t.Fatal("new turn retained old reading position")
	}
}

func TestAnswerAnchorsStayForwardAndBoundPrediction(t *testing.T) {
	var s answerScroll
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s.follow(10, 100)
	focus := func(cursor float64) float64 { return cursor * 100 }
	for i := 0; i < 30; i++ {
		s.advance(at.Add(time.Duration(i)*time.Second), true, 1000, 73, focus)
	}
	if s.position < 100 || s.position > 126 {
		t.Fatal("unbounded word-anchor prediction", s.position)
	}
	before := s.position
	s.follow(20, 20)
	s.advance(at.Add(30*time.Second), true, 1000, 73, focus)
	if s.position < before {
		t.Fatal("late timing rewound the reader")
	}
	s.manual = true
	s.follow(200, 500)
	if s.anchor != 100 {
		t.Fatal("anchor overrode manual reading")
	}
}

func TestAnswerProvisionalFocusUsesWrappedLinesAtEachResolution(t *testing.T) {
	text := strings.Repeat("Ein gut lesbarer langer Antworttext. ", 30)
	for _, size := range []image.Point{{960, 480}, {1280, 800}} {
		r := newRenderer(image.NewRGBA(image.Rectangle{Max: size}))
		l := r.layoutAnswer("", text)
		line := l.lines[6]
		height := r.h - r.s(140)
		got := l.focus(float64(line.replyStart), height, r.s(answerLineHeight))
		if got != float64(max(0, line.top+r.s(answerLineHeight)/2-height/2)) {
			t.Fatal("focus ignores actual layout")
		}
		// A second font size wraps the same source position differently; the
		// provisional reader must use those new lines, not a fixed px/s offset.
		r.answerBody = r.body
		r.answer.layout = answerLayout{}
		smaller := r.layoutAnswer("", text)
		if smaller.focus(float64(line.replyStart), height, r.s(52)) >= got {
			t.Fatal("font reflow did not change focus projection")
		}
	}
}

func TestAnswerLargerTextAndMiddleReadingFocus(t *testing.T) {
	r := newRenderer(image.NewRGBA(image.Rect(0, 0, 960, 480)))
	if ratio := float64(r.answerBody.Metrics().Height) / float64(r.body.Metrics().Height); ratio < 1.35 || ratio > 1.45 {
		t.Fatal("answer size is not about 40 percent larger", ratio)
	}
	text := strings.Repeat("Readable words in a long reply. ", 40)
	l := r.layoutAnswer("", text)
	line := l.lines[6]
	r.draw(scene{phase: "replying", reply: text, replyCursorKnown: true, replyCursor: line.replyStart})
	viewportHeight := r.h - r.s(70) - r.s(70)
	want := float64(line.top + r.s(answerLineHeight)/2 - viewportHeight/2)
	if r.answer.scroll.anchor != want {
		t.Fatal("reading anchor is not centered", r.answer.scroll.anchor, want)
	}
}

func TestAnswerOffsetsUseOriginalUnicodeCodepoints(t *testing.T) {
	r := newRenderer(image.NewRGBA(image.Rect(0, 0, 960, 480)))
	original := "Hi 🙂\r\n\r\nCafe\u0301 next"
	l := r.layoutAnswer("", original)
	if len(l.lines) != 2 {
		t.Fatal("unexpected lines", l.lines)
	}
	if l.lines[0].replyStart != 0 || l.lines[0].replyEnd != 4 || l.lines[1].replyStart != 8 {
		t.Fatal("indices drifted from source runes", l.lines)
	}
}

func TestAnswerGeometryFocusPreview(t *testing.T) {
	dir := os.Getenv("TECHO5_ANSWER_PREVIEW_DIR")
	if dir == "" {
		t.Skip("private renderer preview")
	}
	for _, size := range []image.Point{{960, 480}, {1280, 800}} {
		img := image.NewRGBA(image.Rectangle{Max: size})
		r := newRenderer(img)
		at := time.Date(2026, 11, 1, 16, 24, 0, 0, time.UTC)
		s := scene{phase: "replying", heard: "Erkläre mir bitte ein interessantes Thema.", reply: strings.Repeat("Die Schrift bleibt gut lesbar. Der gesprochene Text soll ungefähr in der Mitte bleiben. ", 12), playbackKnown: true, speaking: true}
		for i := 0; i <= 12; i++ {
			s.now = at.Add(time.Duration(i) * time.Second)
			r.draw(s)
		}
		name := "focus-show5.png"
		if size.X == 1280 {
			name = "focus-show8.png"
		}
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAnswerFocusUsesOnlyVisibleReplySpaceBelowQuestion(t *testing.T) {
	for _, size := range []image.Point{{960, 480}, {1280, 800}} {
		r := newRenderer(image.NewRGBA(image.Rectangle{Max: size}))
		l := r.layoutAnswer(strings.Repeat("A longer question. ", 10), strings.Repeat("A readable answer continues. ", 100))
		height := r.h - r.s(140)
		lineHeight := r.s(answerLineHeight)
		answerTop := -1
		headerCase, fullCase := false, false
		for _, line := range l.lines {
			if line.question {
				continue
			}
			if answerTop < 0 {
				answerTop = line.top
			}
			offset := l.focus(float64(line.replyStart), height, lineHeight)
			if offset <= 0 || offset >= float64(l.height-height) {
				continue
			}
			visibleTop := max(0, float64(answerTop)-offset)
			actualCenter := float64(line.top+lineHeight/2) - offset
			want := (visibleTop + float64(height)) / 2
			if actualCenter < want-1 || actualCenter > want+1 {
				t.Fatalf("%v: focus %v, actual text area center %v", size, actualCenter, want)
			}
			if visibleTop > 0 {
				headerCase = true
			} else {
				fullCase = true
			}
		}
		if !headerCase || !fullCase {
			t.Fatal("did not exercise both visible and scrolled-away question", size)
		}
	}
}
