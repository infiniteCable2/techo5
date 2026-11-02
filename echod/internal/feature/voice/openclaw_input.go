package voice

import (
	"errors"
	"log/slog"
	"time"
)

const nativeInputFrameSamples = 320 // 20 ms PCM16 at the negotiated 16 kHz
const nativeInputTailWait = 200 * time.Millisecond

var errNativeInputRetired = errors.New("native input retired")

// nativeInputFrames owns capture framing through its final boundary. Capture
// and history chunks need not be 20 ms; no captured sample is padded or cut.
type nativeInputFrames struct {
	pcm                 [nativeInputFrameSamples * 2]byte
	used                int // samples in the unfinished frame
	frames, tailSamples int
	emit                func([]byte)
}

func (f *nativeInputFrames) feed(samples []int16) {
	for _, sample := range samples {
		i := f.used * 2
		f.pcm[i], f.pcm[i+1] = byte(sample), byte(sample>>8)
		f.used++
		if f.used == nativeInputFrameSamples {
			f.emit(f.pcm[:]) // consumer copies before this buffer is reused
			f.frames++
			f.used = 0
		}
	}
}

// complete keeps the original subscription until the last partial frame is
// filled with the next real samples. Retirement aborts instead of feeding the
// next turn; a stopped microphone fails closed rather than fabricating silence.
func (f *nativeInputFrames) complete(scope *clawSendScope, frames <-chan []int16, observe func([]int16)) error {
	retired := scope.retiredSignal()
	select {
	case <-retired:
		return errNativeInputRetired
	default:
	}
	if f.used == 0 {
		return nil
	}
	timer := time.NewTimer(nativeInputTailWait)
	defer timer.Stop()
	for f.used != 0 {
		select {
		case <-retired:
			return errNativeInputRetired
		case <-timer.C:
			return errors.New("native input real tail unavailable")
		case frame, ok := <-frames:
			if !ok {
				return errors.New("native input ended with a partial real frame")
			}
			scope.mu.Lock()
			if !scope.active {
				scope.mu.Unlock()
				return errNativeInputRetired
			}
			part := frame[:min(len(frame), nativeInputFrameSamples-f.used)]
			observe(part)
			f.tailSamples += len(part)
			f.feed(part)
			scope.mu.Unlock()
		}
	}
	return nil
}

func (c *conversation) finishNativeInput(framed *nativeInputFrames, frames <-chan []int16, be backend, scope *clawSendScope, observe func([]int16)) {
	if err := framed.complete(scope, frames, observe); err != nil {
		if !errors.Is(err, errNativeInputRetired) {
			c.post(event{kind: evError, code: "openclaw-input-frame", clawSend: scope})
		}
		return
	}
	c.sendClaw(scope, "end of audio", be.End)
	slog.Info("native input framed end", "frames", framed.frames, "tail_samples", framed.tailSamples)
}
