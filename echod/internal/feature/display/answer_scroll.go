//go:build !dot && !spot

package display

import (
	"image"
	"image/draw"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/HuskerMinion/techo5/echod/internal/hardware/touch"
)

// The provisional reading clock estimates source characters, never pixels.
// Actual wrapping projects that position into the current font and viewport.
const answerReadDelay = 5 * time.Second
const answerLineHeight = 73
const answerQuestionHeight = 59

type answerLine struct {
	text                 string
	top                  int
	question             bool
	replyStart, replyEnd int
}

type answerLayout struct {
	heard, reply  string
	width, height int
	lines         []answerLine
}

type answerScroll struct {
	position         float64
	limit            int
	readFor          time.Duration
	last             time.Time
	running, manual  bool
	anchorKnown      bool
	anchor, runeRate float64
	anchorRead       time.Duration
	cursor           int
}

func (s *answerScroll) advance(now time.Time, running bool, limit, lineHeight int, focus func(float64) float64) {
	s.limit = max(0, limit)
	s.position = min(max(0, s.position), float64(s.limit))
	if running && s.running && !s.manual && !s.last.IsZero() {
		dt := min(max(now.Sub(s.last), 0), time.Second)
		s.readFor += dt
		if s.anchorKnown || s.readFor > answerReadDelay {
			rate := s.runeRate
			if rate == 0 {
				rate = 13
			}
			cursor := s.readFor.Seconds() * rate
			if s.anchorKnown {
				cursor = float64(s.cursor) + min(max((s.readFor-s.anchorRead).Seconds(), 0), .75)*rate
			}
			target := focus(cursor)
			if s.anchorKnown {
				target = max(s.anchor, min(target, s.anchor+float64(lineHeight)*.35))
			}
			target = min(float64(s.limit), max(s.position, target))
			step := min((target-s.position)*(1-math.Exp(-3*dt.Seconds())), dt.Seconds()*float64(lineHeight)*1.5)
			s.position = min(target, s.position+step)
		}
	}
	s.last, s.running = now, running
}

func (s *answerScroll) follow(cursor int, target float64) {
	if s.manual || (s.anchorKnown && cursor <= s.cursor) {
		return
	}
	if s.anchorKnown {
		if elapsed := (s.readFor - s.anchorRead).Seconds(); elapsed >= .2 {
			observed := min(max(float64(cursor-s.cursor)/elapsed, 5), 30)
			if s.runeRate == 0 {
				s.runeRate = 13
			}
			s.runeRate = .8*s.runeRate + .2*observed
		}
	}
	if !s.anchorKnown || target > s.anchor {
		s.anchor = max(s.anchor, target)
	}
	s.anchorRead = s.readFor
	s.anchorKnown = true
	s.cursor = cursor
}

func (l *answerLayout) focus(cursor float64, viewportHeight, lineHeight int) float64 {
	target := 0
	answerTop := -1
	for _, line := range l.lines {
		if line.question {
			continue
		}
		if answerTop < 0 {
			answerTop = line.top
		}
		center := line.top + lineHeight/2
		target = center - viewportHeight/2
		// While the question is still visible, only the space below it belongs
		// to the answer. Solve for the scroll offset using that moving boundary.
		if target < answerTop {
			target = 2*center - answerTop - viewportHeight
		}
		target = max(0, target)
		if cursor < float64(line.replyEnd) {
			break
		}
	}
	return float64(target)
}

func (s *answerScroll) moving() bool {
	return s.running && !s.manual && s.position < float64(s.limit)
}

type answerPage struct {
	layout              answerLayout // renderer goroutine only; cleared when font faces change
	scroll              answerScroll // scroll and visibility are protected by the renderer's zmu
	visible, wasVisible bool
	phase, reply        string
}

func (r *renderer) beginAnswerFrame(s scene) {
	r.zmu.Lock()
	defer r.zmu.Unlock()
	a := &r.answer
	if (s.phase == "listening" && a.phase != "listening") ||
		(s.reply != "" && a.reply != "" && !strings.HasPrefix(s.reply, a.reply)) {
		a.scroll = answerScroll{}
	}
	a.wasVisible, a.visible, a.phase = a.visible, false, s.phase
	if s.reply != "" {
		a.reply = s.reply
	}
}

func (r *renderer) layoutAnswer(heard, reply string) *answerLayout {
	l := &r.answer.layout
	w := r.w - 2*r.margin
	if l.heard == heard && l.reply == reply && l.width == w {
		return l
	}
	*l = answerLayout{heard: heard, reply: reply, width: w}
	if heard != "" {
		lines := r.wrap(r.answerQuestion, "“"+heard+"”", w)
		// Keep the question as context without making a long question hide the answer.
		if len(lines) > 2 {
			lines = lines[:2]
			lines[1] = r.clipTo(r.answerQuestion, lines[1]+"…", w)
		}
		for _, text := range lines {
			l.lines = append(l.lines, answerLine{text: text, top: l.height, question: true})
			l.height += r.s(answerQuestionHeight)
		}
		l.height += r.s(18)
	}
	base := 0
	for _, paragraph := range strings.Split(reply, "\n") {
		if strings.TrimSpace(paragraph) == "" {
			l.height += r.s(answerLineHeight)
			base += len([]rune(paragraph)) + 1
			continue
		}
		rs := []rune(paragraph)
		line := ""
		start, end := 0, 0
		flush := func() {
			if line != "" {
				l.lines = append(l.lines, answerLine{text: line, top: l.height, replyStart: base + start, replyEnd: base + end})
				l.height += r.s(answerLineHeight)
				line = ""
			}
		}
		for i := 0; i < len(rs); {
			if unicode.IsSpace(rs[i]) {
				i++
				continue
			}
			first := i
			for i < len(rs) && !unicode.IsSpace(rs[i]) {
				i++
			}
			word := string(rs[first:i])
			trial := word
			if line != "" {
				trial = line + " " + word
			}
			if line != "" && r.width(r.answerBody, trial) > w {
				flush()
				trial = word
			}
			if line == "" {
				start = first
			}
			end = i
			line = trial
		}
		flush()
		base += len(rs) + 1
	}
	return l
}

// words clips a viewport, not the answer: every reply line keeps the same face.
func (r *renderer) words(s scene, top int) {
	l := r.layoutAnswer(s.heard, s.reply)
	viewport := image.Rect(r.margin, top, r.w-r.s(12), r.h-r.s(70))
	r.zmu.Lock()
	a := &r.answer
	if s.reply != "" {
		if s.replyCursorKnown {
			for _, line := range l.lines {
				if !line.question && s.replyCursor >= line.replyStart && s.replyCursor < line.replyEnd {
					a.scroll.follow(s.replyCursor, l.focus(float64(s.replyCursor), viewport.Dy(), r.s(answerLineHeight)))
					break
				}
			}
		}
		running := s.phase == "replying" || s.phase == "lingering"
		if s.playbackKnown {
			running = s.phase == "replying" && s.speaking
		}
		a.scroll.advance(s.now, a.wasVisible && running, l.height-viewport.Dy(), r.s(answerLineHeight), func(cursor float64) float64 { return l.focus(cursor, viewport.Dy(), r.s(answerLineHeight)) })
		// The first frame establishes the timer; hidden/processing frames pause it.
		a.scroll.running = running
		a.visible = true
	}
	offset, limit := int(a.scroll.position), a.scroll.limit
	if s.reply == "" {
		offset, limit = 0, 0
	}
	r.zmu.Unlock()
	canvas := r.dst
	r.dst = canvas.SubImage(viewport).(*image.RGBA)
	for _, line := range l.lines {
		face, height, ink := r.answerBody, r.s(answerLineHeight), cream
		if line.question {
			face, height, ink = r.answerQuestion, r.s(answerQuestionHeight), dim
		}
		y := top + line.top - offset
		if y+height < viewport.Min.Y || y >= viewport.Max.Y {
			continue
		}
		r.text(face, line.text, r.margin, y+height-r.s(12), ink)
	}
	r.dst = canvas
	if limit > 0 {
		h := max(r.s(18), viewport.Dy()*viewport.Dy()/l.height)
		y := top + (viewport.Dy()-h)*offset/limit
		draw.Draw(canvas, image.Rect(r.w-r.s(18), y, r.w-r.s(15), y+h), image.NewUniform(dim), image.Point{}, draw.Src)
	}
}

func (r *renderer) answerAnimating() bool {
	r.zmu.Lock()
	defer r.zmu.Unlock()
	return r.answer.visible && r.answer.scroll.moving()
}

func (r *renderer) scrollAnswer(g touch.Gesture) bool {
	if g.Kind != touch.SwipeUp && g.Kind != touch.SwipeDown {
		return false
	}
	// Preserve the top-edge gesture for Settings.
	if g.Kind == touch.SwipeDown && g.Y < topEdge {
		return false
	}
	r.zmu.Lock()
	defer r.zmu.Unlock()
	a := &r.answer
	if !a.visible || a.scroll.limit == 0 {
		return false
	}
	delta := r.s(answerLineHeight)
	if g.Kind == touch.SwipeDown {
		delta = -delta
	}
	a.scroll.position = min(max(a.scroll.position+float64(delta), 0), float64(a.scroll.limit))
	a.scroll.manual = true
	return true
}
