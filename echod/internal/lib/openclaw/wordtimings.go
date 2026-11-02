package openclaw

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const maxTimingInteger = int64(1<<53 - 1)

// OutputTimingsContract is accepted explicitly by talk.session.create. Seeing
// presentation events alone never enables the receiver on a legacy session.
type OutputTimingsContract struct {
	Accepted         bool   `json:"accepted"`
	Version          int    `json:"version"`
	OutputGeneration *int64 `json:"outputGeneration"`
	SampleRate       int    `json:"sampleRate"`
	Encoding         string `json:"encoding"`
	Channels         int    `json:"channels"`
	SampleUnit       string `json:"sampleUnit"`
	Mapping          string `json:"mapping"`
	TextDelivery     string `json:"textDelivery"`
}

func (c OutputTimingsContract) Valid() bool {
	return c.Accepted && c.Version == 1 && timingInteger(c.OutputGeneration) &&
		c.SampleRate == 24000 && c.Encoding == "pcm16" && c.Channels == 1 &&
		c.SampleUnit == "pcm-frame" && c.Mapping == "piecewise-segment-v1" && c.TextDelivery == "outputTextBlock-v1"
}

type OutputWord struct {
	RecognizedWord string   `json:"recognizedWord"`
	StartSample    int64    `json:"startSample"`
	EndSample      int64    `json:"endSample"`
	Probability    *float64 `json:"probability,omitempty"`
	DisplaySpan    []int    `json:"displaySpan,omitempty"`
}

// OutputPresentationEvent mirrors the opt-in native v1 envelope. Optional
// counters use pointers so an absent field cannot masquerade as sample zero.
type OutputPresentationEvent struct {
	Type               string                 `json:"type"`
	SessionID          string                 `json:"sessionId"`
	RelaySessionID     string                 `json:"relaySessionId"`
	TurnID             string                 `json:"turnId"`
	OutputGeneration   *int64                 `json:"outputGeneration"`
	AudioSampleRate    int                    `json:"audioSampleRate"`
	OutputWordTimings  *OutputTimingsContract `json:"outputWordTimings,omitempty"`
	TextBlockID        string                 `json:"textBlockId"`
	TextRevision       *int64                 `json:"textRevision"`
	Purpose            string                 `json:"purpose"`
	Text               string                 `json:"text"`
	SegmentID          string                 `json:"segmentId"`
	SegmentStartSample *int64                 `json:"segmentStartSample"`
	AudioStartSample   *int64                 `json:"audioStartSample"`
	AudioSampleCount   int64                  `json:"audioSampleCount"`
	AudioPurpose       string                 `json:"audioPurpose"`
	AudioBase64        string                 `json:"audioBase64"`
	SegmentSampleCount *int64                 `json:"segmentSampleCount"`
	SourceSampleCount  *int64                 `json:"sourceSampleCount"`
	SampleRate         int                    `json:"sampleRate"`
	Revision           int64                  `json:"revision"`
	Final              bool                   `json:"final"`
	Words              []OutputWord           `json:"words"`
	Reason             string                 `json:"reason"`
}

type OutputTextBlock struct {
	ID, TurnID, Text string
	Revision         int64
	RuneOffset       int
}

type OutputCursor struct {
	BlockID    string
	Start, End int // Unicode codepoints of this immutable block
}

type outputRange struct{ sourceStart, sourceEnd, audioStart int64 }
type outputSegment struct {
	block                 OutputTextBlock
	ranges                []outputRange
	emitted, sourceCount  int64
	done, disabled, final bool
	revision              int64
	digest                [32]byte
	words                 []OutputWord
}

// WordTimings maps presentation facts, never audio ownership or agent policy.
// A caller serializes access, retains visible text separately, and supplies a
// locally audible generation/sample cursor. Network receipt is not that cursor.
type WordTimings struct {
	session, relay        string
	generation, nextAudio int64
	blocks                map[string]OutputTextBlock
	blockOrder            []string
	segments              map[string]*outputSegment
	segmentOrder          []string
	rangeCount            int
	visible               string
	visibleRunes          int
}

func NewWordTimings(session, relay string, contract OutputTimingsContract) *WordTimings {
	if !validTimingID(session) || (relay != "" && !validTimingID(relay)) || !contract.Valid() {
		return nil
	}
	w := &WordTimings{session: session, relay: relay}
	w.reset(*contract.OutputGeneration)
	return w
}

func timingInteger(n *int64) bool { return n != nil && *n >= 0 && *n <= maxTimingInteger }
func validTimingID(s string) bool { return s != "" && len(s) <= 256 && utf8.ValidString(s) }

func (w *WordTimings) reset(generation int64) {
	w.generation, w.nextAudio, w.rangeCount = generation, 0, 0
	w.blocks = make(map[string]OutputTextBlock)
	w.segments = make(map[string]*outputSegment)
	w.blockOrder, w.segmentOrder = nil, nil
}

// Apply returns a newly admitted immutable text block for the display owner.
// Invalid/obsolete optional metadata is ignored; callers keep audio and text
// independent. Only an authoritative clear can change this receiver's generation.
func (w *WordTimings) Apply(raw json.RawMessage) (block *OutputTextBlock, changed bool) {
	if len(raw) > 65536 {
		return nil, false
	}
	var e OutputPresentationEvent
	if json.Unmarshal(raw, &e) != nil || e.SessionID != w.session || !timingInteger(e.OutputGeneration) {
		return nil, false
	}
	if w.relay == "" {
		if e.Type != "ready" || !validTimingID(e.RelaySessionID) || *e.OutputGeneration != w.generation || e.AudioSampleRate != 24000 || e.OutputWordTimings == nil || !e.OutputWordTimings.Valid() || *e.OutputWordTimings.OutputGeneration != w.generation {
			return nil, false
		}
		w.relay = e.RelaySessionID
	}
	if e.RelaySessionID != w.relay {
		return nil, false
	}
	if e.Type == "clear" {
		if *e.OutputGeneration <= w.generation {
			return nil, false
		}
		w.reset(*e.OutputGeneration)
		return nil, true
	}
	if *e.OutputGeneration != w.generation {
		return nil, false
	}
	s := w.segments[e.SegmentID]
	switch e.Type {
	case "ready":
		return nil, e.AudioSampleRate == 24000 && e.OutputWordTimings != nil && e.OutputWordTimings.Valid() && *e.OutputWordTimings.OutputGeneration == w.generation
	case "outputTextBlock":
		if !validTimingID(e.TextBlockID) || !validTimingID(e.TurnID) || !timingInteger(e.TextRevision) || utf8.RuneCountInString(e.Text) > 16384 ||
			(e.Purpose != "commentary" && e.Purpose != "final" && e.Purpose != "greeting" && e.Purpose != "speech") {
			return nil, false
		}
		b := OutputTextBlock{ID: e.TextBlockID, TurnID: e.TurnID, Text: e.Text, Revision: *e.TextRevision}
		if _, exists := w.blocks[b.ID]; exists {
			return nil, false
		}
		if w.visible != "" {
			w.visible += "\n\n"
			w.visibleRunes += 2
		}
		b.RuneOffset = w.visibleRunes
		w.visible += b.Text
		w.visibleRunes += utf8.RuneCountInString(b.Text)
		// Visible text is returned independently of this bounded mapping registry.
		if len(w.blockOrder) == 64 {
			delete(w.blocks, w.blockOrder[0])
			w.blockOrder = w.blockOrder[1:]
		}
		w.blocks[b.ID] = b
		w.blockOrder = append(w.blockOrder, b.ID)
		return &b, true
	case "outputSegmentPrepared":
		b, ok := w.blocks[e.TextBlockID]
		if !ok || !validTimingID(e.SegmentID) || !timingInteger(e.TextRevision) || b.Revision != *e.TextRevision || b.TurnID != e.TurnID || s != nil {
			return nil, false
		}
		if len(w.segmentOrder) == 16 {
			old := w.segmentOrder[0]
			w.rangeCount -= len(w.segments[old].ranges)
			delete(w.segments, old)
			w.segmentOrder = w.segmentOrder[1:]
		}
		w.segments[e.SegmentID] = &outputSegment{block: b}
		w.segmentOrder = append(w.segmentOrder, e.SegmentID)
		return nil, true
	case "audio":
		if !timingInteger(e.AudioStartSample) || e.AudioSampleCount <= 0 || e.AudioSampleCount > 480 || *e.AudioStartSample != w.nextAudio || len(e.AudioBase64) > 1280 {
			return nil, false
		}
		pcm, err := base64.StdEncoding.DecodeString(e.AudioBase64)
		if err != nil || int64(len(pcm)) != 2*e.AudioSampleCount || w.nextAudio > maxTimingInteger-e.AudioSampleCount {
			return nil, false
		}
		w.nextAudio += e.AudioSampleCount
		if e.AudioPurpose != "speech" {
			return nil, e.AudioPurpose == "waiting" || e.AudioPurpose == "silence" || e.AudioPurpose == "other"
		}
		if s == nil || s.disabled || s.done || s.block.TurnID != e.TurnID || !timingInteger(e.SegmentStartSample) || *e.SegmentStartSample != s.emitted || s.emitted > maxTimingInteger-e.AudioSampleCount {
			return nil, false
		}
		r := outputRange{s.emitted, s.emitted + e.AudioSampleCount, *e.AudioStartSample}
		if s.final && r.sourceEnd > s.sourceCount {
			s.disabled = true
			return nil, false
		}
		s.emitted = r.sourceEnd
		if n := len(s.ranges); n > 0 && s.ranges[n-1].sourceEnd == r.sourceStart && s.ranges[n-1].audioStart+s.ranges[n-1].sourceEnd-s.ranges[n-1].sourceStart == r.audioStart {
			s.ranges[n-1].sourceEnd = r.sourceEnd
		} else {
			if w.rangeCount >= 4096 {
				s.disabled = true
				return nil, false
			}
			s.ranges = append(s.ranges, r)
			w.rangeCount++
		}
		return nil, true
	case "outputSegmentCancelled", "outputWordTimingsUnavailable":
		if s == nil || e.TurnID != s.block.TurnID {
			return nil, false
		}
		s.disabled = true
		s.words = nil
		return nil, true
	case "outputSegmentDone":
		if !w.bound(s, e) || !timingInteger(e.SegmentSampleCount) || *e.SegmentSampleCount != s.emitted {
			return nil, false
		}
		s.done = true
		return nil, true
	case "outputWordTimings":
		if !w.bound(s, e) || s.disabled || e.SampleRate != 24000 || !timingInteger(e.SourceSampleCount) || *e.SourceSampleCount < s.emitted || e.Revision <= 0 || e.Revision > maxTimingInteger || len(e.Words) > 512 {
			return nil, false
		}
		body, _ := json.Marshal(struct {
			Count int64
			Final bool
			Words []OutputWord
		}{*e.SourceSampleCount, e.Final, e.Words})
		digest := sha256.Sum256(body)
		if e.Revision <= s.revision || s.final {
			return nil, false
		} // identical retransmission is intentionally idempotent
		lastStart := int64(0)
		for _, word := range e.Words {
			if strings.TrimSpace(word.RecognizedWord) == "" || utf8.RuneCountInString(word.RecognizedWord) > 256 || word.StartSample < lastStart || word.EndSample < word.StartSample || word.EndSample > *e.SourceSampleCount ||
				(word.Probability != nil && (*word.Probability < 0 || *word.Probability > 1)) {
				return nil, false
			}
			if len(word.DisplaySpan) != 0 && (len(word.DisplaySpan) != 2 || word.DisplaySpan[0] < 0 || word.DisplaySpan[1] < word.DisplaySpan[0] || word.DisplaySpan[1] > utf8.RuneCountInString(s.block.Text)) {
				return nil, false
			}
			lastStart = word.StartSample
		}
		s.words = e.Words
		s.sourceCount = *e.SourceSampleCount
		s.revision = e.Revision
		s.final = e.Final
		s.digest = digest
		return nil, true
	}
	return nil, false
}

func (w *WordTimings) bound(s *outputSegment, e OutputPresentationEvent) bool {
	return s != nil && e.TextBlockID == s.block.ID && timingInteger(e.TextRevision) && *e.TextRevision == s.block.Revision && e.TurnID == s.block.TurnID
}

// At finds only a word intersecting ACTUALLY emitted speech. Waiting music and
// prepared-but-unsent suffixes never acquire a text position by interpolation.
func (w *WordTimings) At(generation, sample int64) (OutputCursor, bool) {
	if generation != w.generation || sample < 0 {
		return OutputCursor{}, false
	}
	for _, id := range w.segmentOrder {
		s := w.segments[id]
		if s.disabled {
			continue
		}
		for _, r := range s.ranges {
			if sample < r.audioStart || sample >= r.audioStart+r.sourceEnd-r.sourceStart {
				continue
			}
			source := r.sourceStart + sample - r.audioStart
			for _, word := range s.words {
				if len(word.DisplaySpan) == 2 && source >= word.StartSample && source < word.EndSample {
					return OutputCursor{s.block.ID, word.DisplaySpan[0], word.DisplaySpan[1]}, true
				}
			}
		}
	}
	return OutputCursor{}, false
}

func (w *WordTimings) Text() string      { return w.visible }
func (w *WordTimings) Generation() int64 { return w.generation }
func (w *WordTimings) AbsoluteAt(generation, sample int64) (int, bool) {
	c, ok := w.At(generation, sample)
	if !ok {
		return 0, false
	}
	b, ok := w.blocks[c.BlockID]
	if !ok {
		return 0, false
	}
	return b.RuneOffset + c.Start, true
}
