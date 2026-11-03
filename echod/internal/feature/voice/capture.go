package voice

import "time"

// The microphone remains the capture, privacy and echo-cancellation owner.
// Voice consumes its existing subscription and presentation facts only.
type captureSource interface {
	ListenHistory(string, time.Duration) (<-chan []int16, []int16, func())
	Gain() float64
	Clipped() uint64
	Dropped() uint64
	SpeechActive() bool
	SpeechActivityFrames() uint64
	Level() float64
}
