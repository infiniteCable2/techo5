package voice

import (
	"context"
	"time"
)

// The speaker owns this boundary; capture only waits and selects later samples.
type captureCue interface {
	Wait(context.Context) (time.Time, error)
}

// The microphone remains the capture, privacy and echo-cancellation owner.
// Voice consumes its existing subscription and presentation facts only.
type captureSource interface {
	ListenHistory(string, time.Duration) (<-chan []int16, []int16, func())
	ListenSince(string, time.Time) (<-chan []int16, []int16, func(), error)
	Gain() float64
	Clipped() uint64
	Dropped() uint64
	SpeechActive() bool
	SpeechActivityFrames() uint64
	Level() float64
}
