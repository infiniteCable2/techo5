package voice

import (
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/feature/wakeword"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/led"
	"github.com/HuskerMinion/techo5/echod/internal/lib/endpoint"
)

const (
	nativeActivityConfirm = 200 * time.Millisecond
	nativeActivityGap     = 160 * time.Millisecond
)

// Confirm fresh classifier activity across short syllable gaps, not history
// containing the wake word or the visual meter's lingering release.
type nativeActivity struct {
	active   time.Duration
	gap      time.Duration
	elapsed  time.Duration
	overGate time.Duration
	sent     bool
}

func (a *nativeActivity) feed(active bool, duration time.Duration) bool {
	a.elapsed += duration
	if active {
		a.overGate += duration
		a.gap = 0
	} else {
		a.gap += duration
	}
	if a.sent {
		return false
	}
	if !active {
		if a.gap >= nativeActivityGap {
			a.active = 0
		}
		return false
	}
	a.active += duration
	if a.active < nativeActivityConfirm {
		return false
	}
	a.sent = true
	return true
}

func (a *nativeActivity) ended(relativeEnded bool, silence time.Duration) bool {
	return a.sent && relativeEnded && a.gap >= silence
}

func (c *conversation) nativeTalk() bool { _, ok := c.be.(*clawBackend); return ok }

func (c *conversation) armNoSpeech() {
	c.disarmNoSpeech()
	if !c.nativeTalk() {
		return
	}
	d := wakeword.NoSpeech(c.slot)
	if c.followUp && wakeword.FollowUp(c.slot) > 0 {
		d = wakeword.FollowUp(c.slot)
	}
	c.noSpeechUntil = c.now().Add(d)
	c.noSpeech = time.NewTimer(d)
}

func (c *conversation) disarmNoSpeech() {
	if c.noSpeech != nil {
		c.noSpeech.Stop()
		c.noSpeech = nil
	}
	c.noSpeechUntil = time.Time{}
}

func (c *conversation) showThinking() {
	if c.nativeTalk() && c.noSpeech != nil {
		c.showListening()
		return
	}
	if c.nativeTalk() && wakeword.ThinkingEffect(c.slot) == "" {
		c.claim.Play(led.EffectComet, c.ring.Base())
		return
	}
	c.showPhase(wakeword.ThinkingEffect(c.slot))
}

func nativeEndpointSettings(milliseconds int) endpoint.Settings {
	s := endpoint.Default
	if milliseconds > 0 {
		s.Quiet = max(2, min(milliseconds, 3000)/100)
	}
	return s
}

func (c *conversation) showReplying() {
	if c.nativeTalk() && (c.shown.Speaking != c.nativeAudible || !c.shown.PlaybackKnown) {
		c.shown.PlaybackKnown = true
		c.shown.Speaking = c.nativeAudible
		Changed.Emit(c.shown)
	}
	if c.nativeTalk() && wakeword.ReplyingEffect(c.slot) == "" {
		if c.nativeAudible {
			c.claim.Paint(led.Solid(c.ring.Base()))
		} else {
			c.showThinking()
		}
		return
	}
	c.showPhase(wakeword.ReplyingEffect(c.slot))
}
