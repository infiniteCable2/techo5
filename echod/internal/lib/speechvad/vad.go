// Package speechvad runs the small ESPHome microWakeWord VAD v2 model on
// 16 kHz mono PCM. It observes audio without changing the samples.
package speechvad

import (
	_ "embed"
	"fmt"
	"math"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/lib/mwwfront"
	"github.com/zserge/microwakeword/interpreter"
	"github.com/zserge/microwakeword/model"
)

// Unmodified Apache-2.0 model; see MODEL.md, LICENSE and NOTICE.
//
//go:embed vad.tflite
var weights []byte

const (
	ModelBytes = 34328
	Onset      = .5
	Release    = .35
	window     = 480 // 30 ms
	step       = 160 // 10 ms
	average    = 5
)

// Result covers one inference stride. Active has a short probability average
// and hysteresis; the caller owns onset confirmation and end-of-turn silence.
type Result struct {
	Probability float64
	Active      bool
	Duration    time.Duration
}

// Detector belongs to one stream. Each turn gets independent frontend and
// neural state; no background inference or shared microphone-reader work.
type Detector struct {
	front          *mwwfront.Frontend
	net            *interpreter.Interpreter
	stride, filled int
	input          []int8
	pcm            []int16
	prob           [average]float64
	index, count   int
	active         bool
}

func New() (*Detector, error) {
	m, err := model.Parse(weights)
	if err != nil {
		return nil, fmt.Errorf("VAD model: %w", err)
	}
	if len(m.Subgraphs) == 0 || len(m.Subgraphs[0].Inputs) != 1 {
		return nil, fmt.Errorf("VAD input missing")
	}
	g := m.Subgraphs[0]
	shape := g.Tensors[g.Inputs[0]].Shape
	if len(shape) != 3 || shape[0] != 1 || shape[1] < 1 || shape[1] > 10 || shape[2] != 40 {
		return nil, fmt.Errorf("VAD input shape %v", shape)
	}
	net, err := interpreter.New(m)
	if err != nil {
		return nil, fmt.Errorf("VAD interpreter: %w", err)
	}
	return &Detector{front: mwwfront.New(10), net: net, stride: shape[1], input: make([]int8, shape[1]*40), pcm: make([]int16, 0, window+320)}, nil
}

// Feed accepts arbitrary chunk sizes and emits every inference in order. It
// never returns early on speech and never drops the rest of an input chunk.
func (d *Detector) Feed(samples []int16, observe func(Result)) error {
	d.pcm = append(d.pcm, samples...)
	used := 0
	for len(d.pcm)-used >= window {
		mwwfront.Quantize(d.front.ProcessFrame(d.pcm[used:used+window]), d.input[d.filled*40:(d.filled+1)*40])
		used += step
		d.filled++
		if d.filled != d.stride {
			continue
		}
		d.filled = 0
		if err := d.net.SetInput(d.input); err != nil {
			return err
		}
		if err := d.net.Invoke(); err != nil {
			return err
		}
		out := d.net.OutputFloat32()
		if len(out) != 1 || math.IsNaN(float64(out[0])) || math.IsInf(float64(out[0]), 0) || out[0] < 0 || out[0] > 1 {
			return fmt.Errorf("invalid VAD probability")
		}
		d.prob[d.index] = float64(out[0])
		d.index = (d.index + 1) % average
		d.count = min(d.count+1, average)
		var probability float64
		for _, p := range d.prob {
			probability += p
		}
		probability /= float64(d.count)
		if d.count == average {
			cutoff := Onset
			if d.active {
				cutoff = Release
			}
			d.active = probability >= cutoff
		}
		if observe != nil {
			observe(Result{Probability: probability, Active: d.active, Duration: time.Duration(d.stride) * 10 * time.Millisecond})
		}
	}
	copy(d.pcm, d.pcm[used:])
	d.pcm = d.pcm[:len(d.pcm)-used]
	return nil
}
