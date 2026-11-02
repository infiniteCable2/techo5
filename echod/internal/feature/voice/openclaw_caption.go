package voice

import "strings"

// clawCaptions is display-only. Transcript blocks remain in arrival order,
// separated by an empty line; provisional text never changes playback policy.
type clawCaptions struct {
	blocks  []string
	pending string
	seen    map[string]bool
}

func (c *clawCaptions) text() string {
	if c.pending == "" {
		return strings.Join(c.blocks, "\n\n")
	}
	if len(c.blocks) == 0 {
		return c.pending
	}
	return strings.Join(c.blocks, "\n\n") + "\n\n" + c.pending
}

func (c *clawCaptions) update(text, id string, final, summary bool) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return c.text()
	}
	if !final {
		// The native transcript callback supplies the current text snapshot.
		c.pending = text
		return c.text()
	}
	if id != "" {
		if c.seen[id] {
			return c.text()
		}
		if c.seen == nil {
			c.seen = make(map[string]bool)
		}
		c.seen[id] = true
	}
	c.pending = ""
	if n := len(c.blocks); n > 0 && c.blocks[n-1] == text {
		return c.text()
	}
	if summary {
		// output.text.done may repeat the final answer already delivered in
		// transcript blocks. Preserve the earlier commentary and paragraph breaks.
		normalize := func(s string) string { return strings.Join(strings.Fields(s), " ") }
		for i := range c.blocks {
			if normalize(strings.Join(c.blocks[i:], " ")) == normalize(text) {
				return c.text()
			}
		}
	}
	c.blocks = append(c.blocks, text)
	return c.text()
}

func (b *clawBackend) caption(t *clawTurn, text, id string, final, summary bool, expected ...clawFence) {
	b.mu.Lock()
	fence := clawFence{t.generation, t.outputEpoch}
	if len(expected) == 1 {
		fence = expected[0]
	}
	if !b.matchesFence(t, fence) {
		b.mu.Unlock()
		return
	}
	before := t.captions.text()
	after := t.captions.update(text, id, final, summary)
	b.mu.Unlock()
	if before != after {
		b.emitFrom(t, fence, event{kind: evClawCaption, text: after})
	}
}
