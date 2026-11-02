package speechvad

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func results(t *testing.T, pcm []int16, chunk int) []Result {
	t.Helper()
	d, err := New()
	if err != nil {
		t.Fatal(err)
	}
	var out []Result
	for start := 0; start < len(pcm); start += chunk {
		if err := d.Feed(pcm[start:min(start+chunk, len(pcm))], func(r Result) { out = append(out, r) }); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestSilence(t *testing.T) {
	for _, r := range results(t, make([]int16, 8*16000), 320) {
		if r.Active {
			t.Fatal("silence classified as speech")
		}
	}
}

func fixture(t *testing.T, name string) []int16 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "third_party", "microwakeword", "testdata", name+".wav"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		t.Fatal("not WAV")
	}
	for i := 12; i+8 <= len(b); {
		n := int(binary.LittleEndian.Uint32(b[i+4:]))
		id := string(b[i : i+4])
		i += 8
		if n > len(b)-i {
			t.Fatal("short WAV")
		}
		if id == "fmt " && (binary.LittleEndian.Uint16(b[i:]) != 1 || binary.LittleEndian.Uint16(b[i+2:]) != 1 || binary.LittleEndian.Uint32(b[i+4:]) != 16000 || binary.LittleEndian.Uint16(b[i+14:]) != 16) {
			t.Fatal("expected mono PCM16 16 kHz")
		}
		if id == "data" {
			pcm := make([]int16, n/2)
			for j := range pcm {
				pcm[j] = int16(binary.LittleEndian.Uint16(b[i+j*2:]))
			}
			return pcm
		}
		i += n + n%2
	}
	t.Fatal("no WAV data")
	return nil
}

func TestRealSpeechAndChunkBoundaries(t *testing.T) {
	pcm := fixture(t, "hey_jarvis")
	unchanged := append([]int16(nil), pcm...)
	want := results(t, pcm, 320)
	if !reflect.DeepEqual(pcm, unchanged) {
		t.Fatal("classifier changed microphone PCM")
	}
	for _, chunk := range []int{1, 160, 511, 16000} {
		if got := results(t, pcm, chunk); !reflect.DeepEqual(got, want) {
			t.Fatalf("chunk size %d changed decisions", chunk)
		}
	}
	for _, gain := range []float64{1, .25, .1} {
		t.Run(fmt.Sprintf("gain_%.2f", gain), func(t *testing.T) {
			copyPCM := make([]int16, len(pcm)+3*16000)
			for i, v := range pcm {
				copyPCM[i] = int16(float64(v) * gain)
			}
			active, tail := 0, 0
			for _, r := range results(t, copyPCM, 320) {
				if r.Active {
					active++
					tail = 0
				} else {
					tail++
				}
			}
			if active == 0 {
				t.Fatal("real speech was never detected")
			}
			if tail < 20 {
				t.Fatal("VAD did not release after speech")
			}
		})
	}
}

func TestFootprint(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	d, err := New()
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(d)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("model_bytes=%d initialization_allocated_bytes=%d", len(weights), allocated)
	if allocated > 2*1024*1024 {
		t.Fatal("tiny VAD exceeded initialization allocation budget")
	}
}

func TestLongStreamRetainsSpeechAcrossQuietSegments(t *testing.T) {
	word := fixture(t, "hey_jarvis")
	// Keep the original spoken segment; remove file padding for a sequence
	// with known short breaks. This is test annotation, never runtime gating.
	var peak int
	for _, s := range word {
		peak = max(peak, int(s), -int(s))
	}
	first, last := 0, len(word)-1
	for first < last && int(math.Abs(float64(word[first]))) < peak/20 {
		first++
	}
	for last > first && int(math.Abs(float64(word[last]))) < peak/20 {
		last--
	}
	word = word[max(0, first-320):min(len(word), last+321)]
	d, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for repetition := range 24 {
		pcm := make([]int16, len(word)+4000)
		gain := []float64{1, .25, .1}[repetition%3]
		for i, s := range word {
			pcm[i] = int16(float64(s) * gain)
		}
		active := 0
		for i := 0; i < len(pcm); i += 320 {
			if err := d.Feed(pcm[i:min(i+320, len(pcm))], func(r Result) {
				if r.Active {
					active++
				}
			}); err != nil {
				t.Fatal(err)
			}
		}
		if active == 0 {
			t.Fatalf("stream lost speech on repetition %d", repetition)
		}
	}
	quiet := 0
	if err := d.Feed(make([]int16, 3*16000), func(r Result) {
		if r.Active {
			quiet = 0
		} else {
			quiet++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if quiet < 20 {
		t.Fatal("long stream did not release into silence")
	}
}

func TestNoiseAndToneAreNotSustainedSpeech(t *testing.T) {
	for _, tone := range []bool{false, true} {
		pcm := make([]int16, 5*16000)
		var seed uint32 = 17
		for i := range pcm {
			seed = 1664525*seed + 1013904223
			v := float64(int32(seed>>16)-32768) * .03
			if tone {
				v += 1000 * math.Sin(2*math.Pi*500*float64(i)/16000)
			}
			pcm[i] = int16(v)
		}
		active := 0
		for _, r := range results(t, pcm, 320) {
			if r.Active {
				active++
			}
		}
		if active > 10 {
			t.Fatalf("sustained noise/tone classified as speech: %d decisions", active)
		}
	}
}

func BenchmarkSilence(b *testing.B) {
	d, err := New()
	if err != nil {
		b.Fatal(err)
	}
	pcm := make([]int16, 320)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		for range 50 {
			if err := d.Feed(pcm, nil); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(1, "audio_s/op")
}
