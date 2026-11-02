package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestOpenClawBindingStaysOnItsOwnDevice(t *testing.T) {
	c := Defaults()
	c.OpenClaw = OpenClaw{URL: "wss://gateway.example", SessionKey: "device-private-conversation", StateDir: "/private/device-state", BootstrapTokenFile: "/private/bootstrap"}
	data, err := Export(c, "test", "local")
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"gateway.example", "device-private-conversation", "/private/"} {
		if strings.Contains(string(data), private) {
			t.Fatal("paired OpenClaw binding was exported")
		}
	}
	other := Defaults()
	other.OpenClaw = OpenClaw{URL: "wss://other.example", SessionKey: "other"}
	file, err := Export(other, "test", "local")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Restored(c, file, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Config.OpenClaw, c.OpenClaw) {
		t.Fatal("settings import changed paired Gateway binding")
	}
}
