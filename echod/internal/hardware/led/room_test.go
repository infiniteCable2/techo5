package led

import (
	"bytes"
	"math"
	"testing"
)

func TestRoomBrightnessRepaintsSolidWithoutCompoundingOrLightingBlack(t *testing.T) {
	r := &Ring{Path: t.TempDir()}
	frame := bytes.Repeat([]byte{200, 100, 50}, Segments)
	if err := r.SetFrame(frame); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRoomScale(.5); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Frame(); !bytes.Equal(got, bytes.Repeat([]byte{100, 50, 25}, Segments)) {
		t.Fatal("solid frame did not dim")
	}
	if err := r.SetFrame(frame); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRoomScale(1); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Frame(); !bytes.Equal(got, frame) {
		t.Fatal("brightness compounded or color changed")
	}
	if err := r.Off(); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRoomScale(.2); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Frame(); !bytes.Equal(got, make([]byte, Channels)) {
		t.Fatal("room scale lit a dark idle ring")
	}
	if err := r.SetRoomScale(math.NaN()); err == nil {
		t.Fatal("invalid scale accepted")
	}
}
