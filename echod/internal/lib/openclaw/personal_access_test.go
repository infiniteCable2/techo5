package openclaw

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Exercise the wire request, signed scopes and primary/companion receipt paths.
func TestPersonalAccessScopeContract(t *testing.T) {
	readTalk := []string{"operator.read", "operator.talk"}
	personal := []string{"operator.read", "operator.talk", "operator.write"}
	for _, mode := range []bool{false, true} {
		for _, companion := range []bool{false, true} {
			for _, tc := range []struct {
				name                  string
				scopes                []string
				defaultOK, personalOK bool
			}{
				{"read-talk", readTalk, true, false},
				{"personal", personal, false, true},
				{"reordered", []string{"operator.write", "operator.talk", "operator.read"}, false, true},
				{"missing-read", []string{"operator.talk", "operator.write"}, false, false},
				{"missing-talk", []string{"operator.read", "operator.write"}, false, false},
				{"duplicate", []string{"operator.read", "operator.talk", "operator.write", "operator.write"}, false, false},
				{"admin", []string{"operator.read", "operator.talk", "operator.write", "operator.admin"}, false, false},
				{"other", []string{"operator.read", "operator.talk", "operator.write", "operator.approvals"}, false, false},
			} {
				t.Run(strconv.FormatBool(mode)+"/companion-"+strconv.FormatBool(companion)+"/"+tc.name, func(t *testing.T) {
					url, offers := mockGateway(t)
					c := newTestClient(t, url)
					c.config.PersonalAgentAccess = mode
					role, signedToken := "operator", ""
					wantRequest := readTalk
					if mode {
						wantRequest = personal
					}
					if companion {
						role, signedToken, wantRequest = "node", "synthetic-setup", []string{}
						c.config.BootstrapRole = "node"
						c.config.BootstrapTokenFile = filepath.Join(c.config.StateDir, "bootstrap")
						if err := os.WriteFile(c.config.BootstrapTokenFile, []byte(signedToken), 0600); err != nil {
							t.Fatal(err)
						}
					}
					socket, _, err := websocket.DefaultDialer.Dial(url, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer socket.Close()
					done := make(chan error, 1)
					go func() { done <- c.handshake(&connection{socket: socket}, role) }()
					o := receiveOffer(t, offers)
					scopes := jsonValue[[]string](t, o.request.Params["scopes"])
					if !reflect.DeepEqual(scopes, wantRequest) {
						t.Fatalf("requested scopes %v; want %v", scopes, wantRequest)
					}
					device := jsonValue[map[string]any](t, o.request.Params["device"])
					payload := strings.Join([]string{"v3", c.identity.id, "node-host", "node", role, strings.Join(scopes, ","), strconv.FormatInt(o.ts, 10), signedToken, "synthetic-nonce", "linux", "echo"}, "|")
					sig, err := base64.RawURLEncoding.DecodeString(device["signature"].(string))
					if err != nil || !ed25519.Verify(c.identity.key.Public().(ed25519.PublicKey), []byte(payload), sig) {
						t.Fatal("scope request not covered by device signature")
					}
					auth := map[string]any{"role": role, "scopes": tc.scopes, "deviceToken": "synthetic-operator"}
					if companion {
						auth = map[string]any{"role": "node", "scopes": []string{}, "deviceToken": "synthetic-node", "deviceTokens": []map[string]any{{"role": "operator", "scopes": tc.scopes, "deviceToken": "synthetic-operator"}}}
					}
					if err := o.socket.WriteJSON(map[string]any{"type": "res", "id": o.request.ID, "ok": true, "payload": map[string]any{"type": "hello-ok", "protocol": 4, "auth": auth}}); err != nil {
						t.Fatal(err)
					}
					wantOK := tc.defaultOK
					if mode {
						wantOK = tc.personalOK
					}
					select {
					case err := <-done:
						if (err == nil) != wantOK {
							t.Fatalf("accepted=%v; want %v (%v)", err == nil, wantOK, err)
						}
					case <-time.After(2 * time.Second):
						t.Fatal("handshake stalled")
					}
					for _, storedRole := range []string{"operator", "node"} {
						_, err := privateFile(filepath.Join(c.config.StateDir, storedRole+".token"))
						wantStored := wantOK && (storedRole == "operator" || companion)
						if wantStored && err != nil {
							t.Fatal("accepted credential missing", err)
						}
						if !wantStored && !os.IsNotExist(err) {
							t.Fatal("rejected or unsolicited credential persisted")
						}
					}
				})
			}
		}
	}
}
