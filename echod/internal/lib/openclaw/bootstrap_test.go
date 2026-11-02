package openclaw

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gorilla/websocket"
)

func TestBootstrapCompanionGrant(t *testing.T) {
	node := func(scopes []string, token string) map[string]any {
		return map[string]any{"role": "node", "scopes": scopes, "deviceToken": token}
	}
	for _, tc := range []struct {
		name      string
		grants    []map[string]any
		cached    string
		bootstrap bool
		wantOK    bool
	}{
		{"new node", []map[string]any{node(nil, "synthetic-node")}, "", true, true},
		{"same cached node", []map[string]any{node(nil, "synthetic-node")}, "synthetic-node", true, true},
		{"different cached node", []map[string]any{node(nil, "synthetic-node")}, "synthetic-old-node", true, false},
		{"scoped node", []map[string]any{node([]string{"operator.admin"}, "synthetic-node")}, "", true, false},
		{"duplicate node", []map[string]any{node(nil, "synthetic-node"), node(nil, "synthetic-node")}, "", true, false},
		{"other role", []map[string]any{{"role": "operator", "scopes": []string{}, "deviceToken": "synthetic-node"}}, "", true, false},
		{"empty receipt", []map[string]any{node(nil, "")}, "", true, false},
		{"invalid receipt", []map[string]any{node(nil, "synthetic|invalid")}, "", true, false},
		{"same as operator", []map[string]any{node(nil, "synthetic-operator")}, "", true, false},
		{"not bootstrap", []map[string]any{node(nil, "synthetic-node")}, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, offers := mockGateway(t)
			c := newTestClient(t, url)
			if tc.bootstrap {
				c.config.BootstrapTokenFile = filepath.Join(c.config.StateDir, "bootstrap")
				if err := os.WriteFile(c.config.BootstrapTokenFile, []byte("synthetic-setup"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			nodePath := filepath.Join(c.config.StateDir, "node.token")
			var before os.FileInfo
			if tc.cached != "" {
				if err := os.WriteFile(nodePath, []byte(tc.cached), 0600); err != nil {
					t.Fatal(err)
				}
				before, _ = os.Stat(nodePath)
			}
			socket, _, err := websocket.DefaultDialer.Dial(url, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer socket.Close()
			done := make(chan error, 1)
			go func() { done <- c.handshake(&connection{socket: socket}, "operator") }()
			o := receiveOffer(t, offers)
			if err := o.socket.WriteJSON(map[string]any{"type": "res", "id": o.request.ID, "ok": true,
				"payload": map[string]any{"type": "hello-ok", "protocol": 4, "auth": map[string]any{
					"role": "operator", "scopes": []string{"operator.read", "operator.talk"},
					"deviceToken": "synthetic-operator", "deviceTokens": tc.grants}}}); err != nil {
				t.Fatal(err)
			}
			if err := <-done; (err == nil) != tc.wantOK {
				t.Fatalf("handshake accepted=%v, want %v", err == nil, tc.wantOK)
			}
			got, err := privateFile(nodePath)
			want := tc.cached
			if tc.wantOK {
				want = "synthetic-node"
			}
			if want == "" {
				if !os.IsNotExist(err) {
					t.Fatal("rejected handoff created node state")
				}
			} else if err != nil || string(got) != want {
				t.Fatal("node state differs from accepted grant or prior credential")
			}
			if before != nil {
				after, err := os.Stat(nodePath)
				if err != nil || !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
					t.Fatal("cached node credential was replaced")
				}
			}
			if !tc.wantOK {
				if _, err := os.Lstat(filepath.Join(c.config.StateDir, "operator.token")); !os.IsNotExist(err) {
					t.Fatal("rejected handoff persisted primary token")
				}
			}
		})
	}
}

// The native voice-node setup authenticates the Node first, then hands it the
// bounded operator grant. A mocked operator-first hello misses this contract.
func TestNodeFirstBootstrap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scopes []string
		cached string
		wantOK bool
	}{
		{"new operator", []string{"operator.read", "operator.talk"}, "", true},
		{"existing operator", []string{"operator.talk", "operator.read"}, "synthetic-operator", true},
		{"changed operator", []string{"operator.read", "operator.talk"}, "synthetic-old", false},
		{"admin scope", []string{"operator.read", "operator.admin"}, "", false},
		{"duplicate scope", []string{"operator.talk", "operator.talk"}, "", false},
		{"missing talk", []string{"operator.read"}, "", false},
		{"extra scope", []string{"operator.read", "operator.talk", "operator.write"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, offers := mockGateway(t)
			c := newTestClient(t, url)
			c.config.BootstrapRole = "node"
			c.config.BootstrapTokenFile = filepath.Join(c.config.StateDir, "bootstrap")
			if err := os.WriteFile(c.config.BootstrapTokenFile, []byte("synthetic-setup"), 0600); err != nil {
				t.Fatal(err)
			}
			opPath := filepath.Join(c.config.StateDir, "operator.token")
			var before os.FileInfo
			if tc.cached != "" {
				if err := os.WriteFile(opPath, []byte(tc.cached), 0600); err != nil {
					t.Fatal(err)
				}
				before, _ = os.Stat(opPath)
			}
			socket, _, err := websocket.DefaultDialer.Dial(url, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer socket.Close()
			done := make(chan error, 1)
			go func() { done <- c.handshake(&connection{socket: socket}, "node") }()
			o := receiveOffer(t, offers)
			auth := jsonValue[map[string]string](t, o.request.Params["auth"])
			if auth["bootstrapToken"] != "synthetic-setup" || auth["deviceToken"] != "" || jsonValue[string](t, o.request.Params["role"]) != "node" {
				t.Fatal("setup did not authenticate as node")
			}
			if err := o.socket.WriteJSON(map[string]any{"type": "event", "event": "device.pair.resolved", "payload": map[string]any{"decision": "approved"}}); err != nil {
				t.Fatal(err)
			}
			if err := o.socket.WriteJSON(map[string]any{"type": "res", "id": o.request.ID, "ok": true, "payload": map[string]any{"type": "hello-ok", "protocol": 4, "auth": map[string]any{
				"role": "node", "scopes": []string{}, "deviceToken": "synthetic-node", "deviceTokens": []map[string]any{{"role": "operator", "scopes": tc.scopes, "deviceToken": "synthetic-operator"}},
			}}}); err != nil {
				t.Fatal(err)
			}
			if err := <-done; (err == nil) != tc.wantOK {
				t.Fatalf("accepted=%v, want %v", err == nil, tc.wantOK)
			}
			got, err := privateFile(opPath)
			want := tc.cached
			if tc.wantOK {
				want = "synthetic-operator"
			}
			if want == "" {
				if !os.IsNotExist(err) {
					t.Fatal("rejected receipt created operator state")
				}
			} else if err != nil || string(got) != want {
				t.Fatal("operator receipt changed")
			}
			if before != nil {
				after, err := os.Stat(opPath)
				if err != nil || !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
					t.Fatal("existing operator file replaced")
				}
			}
			node, err := privateFile(filepath.Join(c.config.StateDir, "node.token"))
			if tc.wantOK {
				if err != nil || string(node) != "synthetic-node" {
					t.Fatal("node receipt missing")
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("rejected receipt persisted primary node")
			}
		})
	}
}
