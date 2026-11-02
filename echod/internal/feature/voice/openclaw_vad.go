package voice

import (
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/lib/speechvad"
)

const (
	levelCandidateConfirm = 40 * time.Millisecond
	levelCandidateLife    = 400 * time.Millisecond
	pairedSpeechConfirm   = 60 * time.Millisecond
)

// nativeInput combines a quick configured room-level candidate with neural
// speech confirmation. Only the VAD holds speech or decides its quiet period.
type nativeInput struct {
	model                                          *speechvad.Detector
	activity                                       nativeActivity
	silence                                        time.Duration
	ended                                          bool
	peakProbability                                float64
	work                                           time.Duration
	levelElapsed, levelAt, levelActive, levelTotal time.Duration
	levelSeen, levelReported                       bool
}

// level observes fresh post-chime audio only. It never disarms the opening
// guard or submits a turn. The short-lived candidate speeds VAD confirmation;
// standalone VAD confirmation remains available for very quiet speech.
func (v *nativeInput) level(active bool, duration time.Duration) bool {
	v.levelElapsed += duration
	if !active {
		return false
	}
	if !v.levelSeen || v.levelElapsed-v.levelAt >= nativeActivityGap {
		v.levelActive = 0
	}
	v.levelTotal += duration
	v.levelActive += duration
	v.levelAt = v.levelElapsed
	v.levelSeen = true
	if v.levelActive < levelCandidateConfirm || v.levelReported {
		return false
	}
	v.levelReported = true
	return true
}

func (v *nativeInput) recentLevel() bool {
	return v.levelSeen && v.levelActive >= levelCandidateConfirm && v.levelElapsed-v.levelAt <= levelCandidateLife
}

func newNativeInput(silenceMS int) (*nativeInput, error) {
	model, err := speechvad.New()
	if err != nil {
		return nil, err
	}
	return &nativeInput{model: model, silence: time.Duration(nativeEndpointSettings(silenceMS).Quiet) * 100 * time.Millisecond}, nil
}

func (v *nativeInput) observe(r speechvad.Result) (onset, ended bool) {
	v.peakProbability = max(v.peakProbability, r.Probability)
	onset = v.activity.feed(r.Active, r.Duration)
	if !v.activity.sent && r.Active && v.recentLevel() && v.activity.active >= pairedSpeechConfirm {
		v.activity.sent = true
		onset = true
	}
	if !v.ended && v.activity.ended(true, v.silence) {
		v.ended = true
		ended = true
	}
	return
}

func (v *nativeInput) feed(frame []int16) (onset, ended bool, err error) {
	start := time.Now()
	err = v.model.Feed(frame, func(r speechvad.Result) {
		on, end := v.observe(r)
		onset = onset || on
		ended = ended || end
	})
	v.work += time.Since(start)
	return
}
