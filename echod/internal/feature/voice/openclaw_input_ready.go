package voice

import (
	"errors"

	"github.com/HuskerMinion/techo5/echod/internal/lib/safe"
)

// Input readiness is media backpressure, not permission to invoke an agent.
// Wait before the device cue and capture, never by holding protected-window PCM.
// The waiter owns no sender/scope lock, so cancellation can retire it immediately.
func (b *clawBackend) inputCurrentLocked(t *clawTurn, capture string) error {
	if b.turn != t || t.ctx.Err() != nil {
		return errors.New("OpenClaw input turn is closed")
	}
	if !b.sameConnection(t) {
		return errors.New("OpenClaw input connection changed")
	}
	if !b.sameSource(t) {
		return errors.New("OpenClaw selected source changed")
	}
	if capture != t.captureID || t.followUpReady {
		return errors.New("OpenClaw input capture changed")
	}
	return nil
}

func (b *clawBackend) inputCurrent(t *clawTurn, capture string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inputCurrentLocked(t, capture)
}

// The conversation rechecks the observed readiness at its actual cue/capture
// boundary: an event queued while ready is not a lease on future readiness.
func (b *clawBackend) captureReady(gen int) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.turn
	if t == nil || t.generation != gen {
		return false, errors.New("OpenClaw input turn changed")
	}
	if err := b.inputCurrentLocked(t, t.captureID); err != nil {
		return false, err
	}
	return t.inputReady && (t.activity.inputAvailable == nil || *t.activity.inputAvailable), nil
}

func (b *clawBackend) waitInputReady(t *clawTurn, capture string) error {
	for {
		b.mu.Lock()
		if err := b.inputCurrentLocked(t, capture); err != nil {
			b.mu.Unlock()
			return err
		}
		available := t.activity.inputAvailable
		// Older activity-v1 implementations omit the additive observation. Keep
		// their existing behavior; omission cannot erase a known false observation.
		blocked := !t.inputReady || t.activity.supported && available != nil && !*available
		wake, waiting := t.inputWake, b.inputWaiting
		b.mu.Unlock()
		if !blocked {
			return nil
		}
		if waiting != nil {
			waiting()
		}
		select {
		case <-t.ctx.Done():
			return t.ctx.Err()
		case <-wake:
		}
	}
}

func (b *clawBackend) prepareCapture(notify func(int, error)) {
	b.mu.Lock()
	t := b.turn
	if t == nil {
		b.mu.Unlock()
		notify(0, errors.New("OpenClaw input turn is closed"))
		return
	}
	capture, gen := t.captureID, t.generation
	b.mu.Unlock()
	safe.Go("OpenClaw capture readiness", func() {
		notify(gen, b.waitInputReady(t, capture))
	})
}
