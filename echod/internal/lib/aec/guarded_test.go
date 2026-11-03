package aec

import (
	"math"
	"slices"
	"testing"
)

func guardedRun(t *testing.T, c *Guarded, mic, ref []int16) []int16 {
	t.Helper()
	var out []int16
	for i := 0; i < len(mic); i += frame {
		end := min(i+frame, len(mic))
		block, err := c.Process(mic[i:end], ref[i:end])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, block...)
	}
	return out
}

func guardedFixture(t *testing.T) (*Guarded, []int16, []int16) {
	t.Helper()
	c, err := NewGuarded(Config{Taps: 1024, Mu: .1})
	if err != nil {
		t.Fatal(err)
	}
	ref := noiseAt(128000, 8000, 91)
	h := make([]float64, 65)
	h[30], h[64] = .5, .1
	return c, ref, echoed(ref, h)
}

func TestGuardedLearnsAColdEchoWithoutManualWarmup(t *testing.T) {
	c, ref, echo := guardedFixture(t)
	out := guardedRun(t, c, echo, ref)
	if got := erle(echo[96000:], out[96000:]); got < 20 {
		t.Fatalf("cold guarded path failed to converge: %.2f dB", got)
	}
}

func TestGuardedDoesNotLearnUncorrelatedNearSpeechOrSilentReference(t *testing.T) {
	for _, silentRef := range []bool{false, true} {
		c, ref, _ := guardedFixture(t)
		if silentRef {
			clear(ref)
		}
		voice := noiseAt(len(ref), 9000, 99)
		out := guardedRun(t, c, voice, ref)
		if got := erle(voice, out); math.Abs(got) > .01 {
			t.Fatalf("silent reference %v changed independent voice: %.2f dB", silentRef, got)
		}
		if slices.ContainsFunc(c.stable.w, func(v float32) bool { return v != 0 }) {
			t.Fatal("unrelated voice promoted coefficients")
		}
	}
}

func TestGuardedKeepsAcceptedPathThroughLoudDoubleTalk(t *testing.T) {
	c, ref, echo := guardedFixture(t)
	guardedRun(t, c, echo[:64000], ref[:64000])
	before := slices.Clone(c.stable.w)
	voice := noiseAt(len(ref)-64000, 9000, 97)
	both := make([]int16, len(voice))
	for i := range both {
		both[i] = echo[64000+i] + voice[i]
	}
	out := guardedRun(t, c, both, ref[64000:])
	if !slices.Equal(before, c.stable.w) {
		t.Fatal("loud independent speech changed accepted coefficients")
	}
	var input, output float64
	for i, v := range voice {
		input += float64(v) * float64(v)
		output += float64(out[i]) * float64(v)
	}
	if gain := output / input; gain < .97 || gain > 1.03 {
		t.Fatalf("near voice gain %.4f", gain)
	}
}

func TestGuardedExplicitListeningFreezePreventsLearningAndPromotion(t *testing.T) {
	c, ref, echo := guardedFixture(t)
	c.SetAdapting(false)
	out := guardedRun(t, c, echo, ref)
	if got := erle(echo, out); math.Abs(got) > .01 {
		t.Fatalf("frozen cold filter removed %.2f dB", got)
	}
	c.SetAdapting(true)
	out = guardedRun(t, c, echo, ref)
	if got := erle(echo[96000:], out[96000:]); got < 20 {
		t.Fatalf("resume failed: %.2f dB", got)
	}
}

func TestGuardedFollowsAnEchoPathChange(t *testing.T) {
	c, ref, echo := guardedFixture(t)
	guardedRun(t, c, echo, ref)
	h := make([]float64, 101)
	h[50], h[100] = -.4, .15
	changed := echoed(ref, h)
	out := guardedRun(t, c, changed, ref)
	if got := erle(changed[96000:], out[96000:]); got < 20 {
		t.Fatalf("new path failed to converge: %.2f dB", got)
	}
}

func TestGuardedRejectsClippedTraining(t *testing.T) {
	c, ref, _ := guardedFixture(t)
	clipped := make([]int16, len(ref))
	for i := range clipped {
		clipped[i] = 32767
	}
	guardedRun(t, c, clipped, ref)
	if slices.ContainsFunc(c.stable.w, func(v float32) bool { return v != 0 }) || slices.ContainsFunc(c.candidate.w, func(v float32) bool { return v != 0 }) {
		t.Fatal("clipped input trained filter")
	}
}

func TestGuardedColdDoubleTalkKeepsIndependentNearSpeechAtDifferentLevels(t *testing.T) {
	for _, amplitude := range []float64{300, 1000, 3000, 9000} {
		c, ref, echo := guardedFixture(t)
		voice := noiseAt(len(ref), amplitude, 94)
		both := make([]int16, len(ref))
		for i := range both {
			both[i] = echo[i] + voice[i]
		}
		out := guardedRun(t, c, both, ref)
		var input, output float64
		for i := 96000; i < len(out); i++ {
			input += float64(voice[i]) * float64(voice[i])
			output += float64(out[i]) * float64(voice[i])
		}
		gain := output / input
		t.Logf("near amplitude %.0f: gain %.4f", amplitude, gain)
		if gain < .94 || gain > 1.06 {
			t.Fatalf("cold double talk damaged voice gain: %.4f", gain)
		}
	}
}

func TestGuardedFilterPromotionCrossfadesInsteadOfStepping(t *testing.T) {
	c, err := NewGuarded(Config{Taps: 64, Mu: .1})
	if err != nil {
		t.Fatal(err)
	}
	// Construct two already-validated, constant predictions. The next block
	// completes held-out validation and must ramp between them, not step once.
	c.wasOn = true
	c.training = guardWindow
	c.checking = guardWindow - frame
	c.input, c.old, c.next = 1e12, 1e12, 1
	c.candidate.w[63] = .5
	mic, ref := make([]int16, frame), make([]int16, frame)
	for i := range mic {
		mic[i], ref[i] = 5000, 10000
	}
	out, e := c.Process(mic, ref)
	if e != nil {
		t.Fatal(e)
	}
	if out[0] < 4900 || out[len(out)-1] > 10 {
		t.Fatalf("promotion did not ramp: first %d last %d", out[0], out[len(out)-1])
	}
	for i := 1; i < len(out); i++ {
		if math.Abs(float64(out[i]-out[i-1])) > 20 {
			t.Fatal("filter swap introduced a step")
		}
	}
	var inputPower, outputPower float64
	for i, v := range out {
		inputPower = inputPower*(1-1.0/erleTau) + float64(mic[i])*float64(mic[i])
		outputPower = outputPower*(1-1.0/erleTau) + float64(v)*float64(v)
	}
	if math.Abs(c.ERLE()-10*math.Log10(inputPower/outputPower)) > 1e-9 {
		t.Fatal("ERLE did not measure the returned crossfade")
	}
}

func TestGuardedAvoidsBlindAdaptationOfUnrelatedUserSpeech(t *testing.T) {
	guard, ref, _ := guardedFixture(t)
	plain, err := New(Config{Taps: 1024, Mu: .1})
	if err != nil {
		t.Fatal(err)
	}
	voice := noiseAt(len(ref), 9000, 98)
	guarded := guardedRun(t, guard, voice, ref)
	var blindError, guardedError, energy float64
	for i := 0; i < len(voice); i += frame {
		out, e := plain.Process(voice[i:i+frame], ref[i:i+frame])
		if e != nil {
			t.Fatal(e)
		}
		for j, v := range out {
			original := float64(voice[i+j])
			d := float64(v) - original
			g := float64(guarded[i+j]) - original
			blindError += d * d
			guardedError += g * g
			energy += original * original
		}
	}
	t.Logf("relative voice waveform error: blind %.4f, guarded %.4f", math.Sqrt(blindError/energy), math.Sqrt(guardedError/energy))
	if blindError < energy*.01 {
		t.Fatal("fixture failed to expose blind adaptation")
	}
	if guardedError > energy*1e-6 {
		t.Fatal("guard changed independent user speech")
	}
}

func TestGuardedDiscardsAnUnacceptedCandidateAfterQuietOrClipping(t *testing.T) {
	for _, clipping := range []bool{false, true} {
		c, ref, echo := guardedFixture(t)
		guardedRun(t, c, echo, ref)
		for i := range c.candidate.w {
			c.candidate.w[i] = .01
		}
		c.training = guardWindow
		quiet := make([]int16, 320)
		mic := make([]int16, 320)
		if clipping {
			for i := range mic {
				mic[i] = 32767
			}
		}
		for range 25 {
			if _, err := c.Process(mic, quiet); err != nil {
				t.Fatal(err)
			}
		}
		if !slices.Equal(c.candidate.w, c.stable.w) {
			t.Fatalf("clipping=%v retained unaccepted coefficients", clipping)
		}
		voice := noiseAt(len(ref), 300, 93)
		both := make([]int16, len(ref))
		for i := range both {
			both[i] = echo[i] + voice[i]
		}
		out := guardedRun(t, c, both, ref)
		var a, b float64
		for i := 96000; i < len(out); i++ {
			a += float64(voice[i]) * float64(voice[i])
			b += float64(out[i]) * float64(voice[i])
		}
		if gain := b / a; gain < .94 || gain > 1.06 {
			t.Fatalf("quiet/clipping recovery damaged near voice: %.4f", gain)
		}
	}
}

func TestGuardedKeepsVoicedNearSpeechWithNarrowbandPlayback(t *testing.T) {
	const n = 16000 * 12
	ref, near := make([]int16, n), make([]int16, n)
	for i := range ref {
		x := float64(i) / 16000
		ref[i] = int16(6000 * (math.Sin(2*math.Pi*173*x) + .3*math.Sin(2*math.Pi*346*x)))
		if i >= 16000*4 {
			env := .3 + .7*math.Pow(math.Sin(2*math.Pi*3*x), 2)
			near[i] = int16(1800 * env * (math.Sin(2*math.Pi*233*x) + .35*math.Sin(2*math.Pi*466*x)))
		}
	}
	h := make([]float64, 65)
	h[30], h[64] = .5, .1
	echo := echoed(ref, h)
	both := make([]int16, n)
	for i := range both {
		both[i] = echo[i] + near[i]
	}
	c, err := NewGuarded(Config{Taps: 1024, Mu: .1})
	if err != nil {
		t.Fatal(err)
	}
	out := guardedRun(t, c, both, ref)
	var energy, projection, errorEnergy float64
	for i := 16000 * 5; i < n; i++ {
		v := float64(near[i])
		energy += v * v
		projection += float64(out[i]) * v
		d := float64(out[i]) - v
		errorEnergy += d * d
	}
	gain := projection / energy
	distortion := math.Sqrt(errorEnergy / energy)
	t.Logf("voiced near gain %.4f, relative error %.4f", gain, distortion)
	if gain < .97 || gain > 1.03 || distortion > .15 {
		t.Fatal("narrowband playback damaged independent voiced near speech")
	}
}
