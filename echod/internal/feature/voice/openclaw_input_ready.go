package voice

import "errors"

// Input readiness is media backpressure, not permission to invoke an agent.
// The existing copied-PCM FIFO owns buffering; its sender waits only before the
// first PCM of a capture. Empty end markers and started captures keep their order.
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
		blocked := t.activity.supported && available != nil && !*available
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
