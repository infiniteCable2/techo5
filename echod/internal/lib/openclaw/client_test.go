package openclaw

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type offer struct {
	socket  *websocket.Conn
	request struct {
		ID     string                     `json:"id"`
		Method string                     `json:"method"`
		Params map[string]json.RawMessage `json:"params"`
	}
	ts int64
}

func mockGateway(t *testing.T) (string, <-chan offer) {
	t.Helper()
	offers := make(chan offer, 8)
	stop := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer socket.Close()
		ts := time.Now().UnixMilli()
		if socket.WriteJSON(map[string]any{"type": "event", "event": "connect.challenge", "payload": map[string]any{"nonce": "synthetic-nonce", "ts": ts}}) != nil {
			return
		}
		o := offer{socket: socket, ts: ts}
		if socket.ReadJSON(&o.request) != nil {
			return
		}
		select {
		case offers <- o:
		case <-stop:
			return
		}
		<-stop
	}))
	t.Cleanup(func() { close(stop); server.Close() })
	return "ws" + strings.TrimPrefix(server.URL, "http"), offers
}

func receiveOffer(t *testing.T, offers <-chan offer) offer {
	t.Helper()
	select {
	case o := <-offers:
		o.socket.SetReadDeadline(time.Now().Add(5 * time.Second))
		return o
	case <-time.After(2 * time.Second):
		t.Fatal("Gateway connect timed out")
		return offer{}
	}
}

func jsonValue[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func hello(t *testing.T, o offer, token string) {
	t.Helper()
	role := jsonValue[string](t, o.request.Params["role"])
	scopes := []string{}
	if role == "operator" {
		scopes = []string{"operator.read", "operator.talk"}
	}
	if err := o.socket.WriteJSON(map[string]any{"type": "res", "id": o.request.ID, "ok": true, "payload": map[string]any{"type": "hello-ok", "protocol": 4, "auth": map[string]any{"role": role, "scopes": scopes, "deviceToken": token}}}); err != nil {
		t.Fatal(err)
	}
}

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{URL: url, StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func waitReady(t *testing.T, c *Client, want bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for c.Ready() != want {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("Ready() did not become %v", want)
		}
	}
}

func TestDualRoleSigningAndTokenSeparation(t *testing.T) {
	url, offers := mockGateway(t)
	c := newTestClient(t, url)
	bootstrap := filepath.Join(c.config.StateDir, "bootstrap")
	if err := os.WriteFile(bootstrap, []byte("synthetic-bootstrap"), 0600); err != nil {
		t.Fatal(err)
	}
	c.config.BootstrapTokenFile = bootstrap
	if err := os.WriteFile(filepath.Join(c.config.StateDir, "node.token"), []byte("synthetic-node"), 0600); err != nil {
		t.Fatal(err)
	}
	nodeBefore, err := os.Stat(filepath.Join(c.config.StateDir, "node.token"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	seen := map[string]bool{}
	for range 2 {
		o := receiveOffer(t, offers)
		role := jsonValue[string](t, o.request.Params["role"])
		seen[role] = true
		if o.request.Method != "connect" {
			t.Fatal("not a native connect request")
		}
		scopes := jsonValue[[]string](t, o.request.Params["scopes"])
		if (role == "node" && len(scopes) != 0) || (role == "operator" && (len(scopes) != 2 || scopes[0] != "operator.read" || scopes[1] != "operator.talk")) {
			t.Fatal("unexpected grants")
		}
		metadata := jsonValue[map[string]any](t, o.request.Params["client"])
		if metadata["id"] != "node-host" || metadata["mode"] != "node" || metadata["platform"] != "linux" || metadata["deviceFamily"] != "echo" {
			t.Fatal("incorrect client metadata")
		}
		if role == "node" {
			if caps := jsonValue[[]string](t, o.request.Params["caps"]); len(caps) != 1 || caps[0] != "talk" {
				t.Fatal("incorrect node capability")
			}
			if commands := jsonValue[[]string](t, o.request.Params["commands"]); len(commands) != 0 {
				t.Fatal("unimplemented commands advertised")
			}
		}
		auth := jsonValue[map[string]string](t, o.request.Params["auth"])
		signedToken := "synthetic-bootstrap"
		if role == "node" {
			signedToken = "synthetic-node"
			if auth["deviceToken"] != signedToken || auth["bootstrapToken"] != "" || auth["token"] != "" {
				t.Fatal("operator setup credential reached node auth")
			}
		} else if auth["bootstrapToken"] != signedToken || auth["deviceToken"] != "" || auth["token"] != "" {
			t.Fatal("incorrect native operator bootstrap credential")
		}
		device := jsonValue[map[string]any](t, o.request.Params["device"])
		if device["id"] != c.identity.id || device["publicKey"] != c.identity.public {
			t.Fatal("roles do not share identity")
		}
		public, _ := base64.RawURLEncoding.DecodeString(device["publicKey"].(string))
		signature, _ := base64.RawURLEncoding.DecodeString(device["signature"].(string))
		payload := strings.Join([]string{"v3", c.identity.id, "node-host", "node", role, strings.Join(scopes, ","), strconv.FormatInt(o.ts, 10), signedToken, "synthetic-nonce", "linux", "echo"}, "|")
		if !ed25519.Verify(public, []byte(payload), signature) {
			t.Fatal("v3 signature differs from native byte contract")
		}
		hello(t, o, "synthetic-"+role)
	}
	if !seen["node"] || !seen["operator"] {
		t.Fatal("missing role")
	}
	waitReady(t, c, true)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run cancellation: %v", err)
	}
	if c.Ready() {
		t.Fatal("client remains ready after cancellation")
	}
	nodeAfter, err := os.Stat(filepath.Join(c.config.StateDir, "node.token"))
	if err != nil || !os.SameFile(nodeBefore, nodeAfter) || !nodeBefore.ModTime().Equal(nodeAfter.ModTime()) || nodeBefore.Mode() != nodeAfter.Mode() {
		t.Fatal("operator renewal rewrote existing node credential metadata")
	}
	if err := os.Remove(bootstrap); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"node", "operator"} {
		raw, err := privateFile(filepath.Join(c.config.StateDir, role+".token"))
		if err != nil || string(raw) != "synthetic-"+role {
			t.Fatal("role token not separately persisted")
		}
	}
	loaded, err := loadIdentity(c.config.StateDir)
	if err != nil || loaded.id != c.identity.id {
		t.Fatal("identity did not persist")
	}
	// Restart authenticates with each role's own token even when bootstrap is gone.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() { done <- c.Run(ctx2) }()
	for range 2 {
		o := receiveOffer(t, offers)
		role := jsonValue[string](t, o.request.Params["role"])
		auth := jsonValue[map[string]string](t, o.request.Params["auth"])
		if auth["deviceToken"] != "synthetic-"+role || auth["token"] != "" || auth["bootstrapToken"] != "" {
			t.Fatal("role credential crossed or bootstrap reused")
		}
		hello(t, o, "synthetic-"+role)
	}
	waitReady(t, c, true)
	cancel2()
	<-done
}

func TestDisconnectFailsPendingCallAndReadiness(t *testing.T) {
	url, offers := mockGateway(t)
	c := newTestClient(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	var operator, node offer
	for range 2 {
		o := receiveOffer(t, offers)
		if jsonValue[string](t, o.request.Params["role"]) == "operator" {
			operator = o
		} else {
			node = o
		}
		hello(t, o, "synthetic-token")
	}
	waitReady(t, c, true)
	if err := operator.socket.WriteJSON(map[string]any{"type": "event", "event": "talk.event", "payload": map[string]any{"type": "synthetic"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-c.Events():
		if !strings.Contains(string(event.Payload), "synthetic") {
			t.Fatal("event payload missing")
		}
	case <-time.After(time.Second):
		t.Fatal("Talk event missing")
	}
	if err := node.socket.WriteJSON(map[string]any{"type": "event", "event": "node.invoke.request", "payload": map[string]any{"id": "invoke-test", "nodeId": c.identity.id, "command": "system.run"}}); err != nil {
		t.Fatal(err)
	}
	var rejected struct {
		Method string `json:"method"`
		Params struct {
			OK bool `json:"ok"`
		} `json:"params"`
	}
	if err := node.socket.ReadJSON(&rejected); err != nil || rejected.Method != "node.invoke.result" || rejected.Params.OK {
		t.Fatal("node invocation was not denied")
	}
	callDone := make(chan error, 1)
	go func() { callDone <- c.Call(ctx, "talk.session.appendAudio", map[string]any{"synthetic": true}, nil) }()
	var request map[string]any
	if err := operator.socket.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	operator.socket.Close()
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("pending call succeeded after close")
		}
	case <-time.After(time.Second):
		t.Fatal("pending call stranded")
	}
	waitReady(t, c, false)
	cancel()
	<-done
}

func TestURLAndStateAdmission(t *testing.T) {
	for _, url := range []string{"ws://example.test", "ws://127.0.0.2", "ws://localhost.example.test", "wss://user:secret@example.test", "wss://example.test?secret=value", "wss://example.test/#fragment", "ftp://localhost"} {
		t.Run(url, func(t *testing.T) {
			if _, err := New(Config{URL: url, StateDir: t.TempDir()}); err == nil {
				t.Fatal("unsafe URL admitted")
			}
		})
	}
	for _, url := range []string{"ws://localhost:1234", "ws://127.0.0.1:1234", "ws://[::1]:1234", "wss://gateway.example.test"} {
		t.Run(url, func(t *testing.T) { newTestClient(t, url) })
	}
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	if err := os.WriteFile(filepath.Join(dir, "identity.seed"), make([]byte, 32), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{URL: "ws://localhost", StateDir: dir}); err == nil {
		t.Fatal("public identity private key admitted")
	}
	bound := newTestClient(t, "wss://gateway.example.test/")
	if _, err := New(Config{URL: "wss://GATEWAY.EXAMPLE.TEST", StateDir: bound.config.StateDir}); err != nil {
		t.Fatal("canonical equivalent Gateway rejected")
	}
	if _, err := New(Config{URL: "wss://other.example.test", StateDir: bound.config.StateDir}); err == nil {
		t.Fatal("paired state admitted a changed Gateway")
	}
}

func TestCallCancellationAndErrorRedaction(t *testing.T) {
	url, offers := mockGateway(t)
	c := newTestClient(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	var operator offer
	for range 2 {
		o := receiveOffer(t, offers)
		if jsonValue[string](t, o.request.Params["role"]) == "operator" {
			operator = o
		}
		hello(t, o, "synthetic-token")
	}
	waitReady(t, c, true)
	callCtx, stopCall := context.WithCancel(ctx)
	callDone := make(chan error, 1)
	go func() { callDone <- c.Call(callCtx, "talk.catalog", map[string]any{}, nil) }()
	var request map[string]any
	if err := operator.socket.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	stopCall()
	if err := <-callDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Call cancellation: %v", err)
	}
	waitReady(t, c, false)
	if got := safeError(&websocket.CloseError{Code: 1008, Text: "synthetic-secret"}); strings.Contains(got, "synthetic-secret") {
		t.Fatal("close reason leaked")
	}
	if got := safeError(errors.New("synthetic-secret")); strings.Contains(got, "synthetic-secret") {
		t.Fatal("transport error leaked")
	}
	cancel()
	<-done
}

func TestRejectedPairingDoesNotPersistOrFallback(t *testing.T) {
	url, offers := mockGateway(t)
	c := newTestClient(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.serve(ctx, "operator") }()
	o := receiveOffer(t, offers)
	if _, present := o.request.Params["auth"]; present {
		t.Fatal("unexpected fabricated bootstrap authentication")
	}
	if err := o.socket.WriteJSON(map[string]any{"type": "res", "id": o.request.ID, "ok": false, "error": map[string]any{"code": "PAIRING_REQUIRED", "message": "synthetic-secret"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || err.Error() != "PAIRING_REQUIRED" {
			t.Fatalf("unredacted or missing pairing failure: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("rejected pairing did not finish")
	}
	if c.Ready() {
		t.Fatal("rejected pairing became ready")
	}
	if _, err := os.Stat(filepath.Join(c.config.StateDir, "operator.token")); !os.IsNotExist(err) {
		t.Fatal("rejection created a role credential")
	}
}

func TestWrongRoleReceiptNeverStored(t *testing.T) {
	url, offers := mockGateway(t)
	c := newTestClient(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.serve(ctx, "operator") }()
	o := receiveOffer(t, offers)
	if err := o.socket.WriteJSON(map[string]any{"type": "res", "id": o.request.ID, "ok": true, "payload": map[string]any{"type": "hello-ok", "protocol": 4, "auth": map[string]any{"role": "node", "scopes": []string{}, "deviceToken": "synthetic-wrong-role"}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("wrong role receipt admitted")
		}
	case <-time.After(time.Second):
		t.Fatal("wrong receipt stranded handshake")
	}
	if _, err := os.Stat(filepath.Join(c.config.StateDir, "operator.token")); !os.IsNotExist(err) {
		t.Fatal("wrong role token persisted")
	}
}

func TestNodeLossFencesPendingOperatorCall(t *testing.T) {
	url, offers := mockGateway(t)
	c := newTestClient(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	var node, operator offer
	for range 2 {
		o := receiveOffer(t, offers)
		if jsonValue[string](t, o.request.Params["role"]) == "node" {
			node = o
		} else {
			operator = o
		}
		hello(t, o, "synthetic-token")
	}
	waitReady(t, c, true)
	callDone := make(chan error, 1)
	go func() { callDone <- c.Call(ctx, "talk.session.create", map[string]any{}, nil) }()
	var request map[string]any
	if err := operator.socket.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	node.socket.Close()
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("operator call survived loss of its node")
		}
	case <-time.After(time.Second):
		t.Fatal("node loss stranded operator call")
	}
	if c.Ready() {
		t.Fatal("pair remains ready after node loss")
	}
	cancel()
	<-done
}

func TestExactRoleScopeReceipts(t *testing.T) {
	for _, tc := range []struct {
		name, role string
		scopes     []string
		valid      bool
	}{
		{"node-empty", "node", []string{}, true},
		{"operator-native", "operator", []string{"operator.read", "operator.talk"}, true},
		{"operator-reordered", "operator", []string{"operator.talk", "operator.read"}, true},
		{"missing-read", "operator", []string{"operator.talk"}, false},
		{"missing-talk", "operator", []string{"operator.read"}, false},
		{"missing-both", "operator", []string{}, false},
		{"duplicate-read", "operator", []string{"operator.read", "operator.read"}, false},
		{"duplicate-with-both", "operator", []string{"operator.read", "operator.talk", "operator.talk"}, false},
		{"extra-write", "operator", []string{"operator.read", "operator.talk", "operator.write"}, false},
		{"extra-admin", "operator", []string{"operator.read", "operator.talk", "operator.admin"}, false},
		{"node-read", "node", []string{"operator.read"}, false},
		{"node-talk", "node", []string{"operator.talk"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, offers := mockGateway(t)
			c := newTestClient(t, url)
			socket, _, err := websocket.DefaultDialer.Dial(url, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer socket.Close()
			conn := &connection{socket: socket, pending: make(map[string]chan frame)}
			done := make(chan error, 1)
			go func() { done <- c.handshake(conn, tc.role) }()
			o := receiveOffer(t, offers)
			if err := o.socket.WriteJSON(map[string]any{"type": "res", "id": o.request.ID, "ok": true, "payload": map[string]any{"type": "hello-ok", "protocol": 4, "auth": map[string]any{"role": tc.role, "scopes": tc.scopes, "deviceToken": "synthetic-receipt"}}}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if (err == nil) != tc.valid {
					t.Fatalf("valid=%v, handshake error=%v", tc.valid, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("scope receipt stranded handshake")
			}
			raw, err := privateFile(filepath.Join(c.config.StateDir, tc.role+".token"))
			if tc.valid {
				if err != nil || string(raw) != "synthetic-receipt" {
					t.Fatal("valid receipt not stored")
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("invalid scope receipt persisted a token")
			}
		})
	}
}

func TestReadScopedTalkEventDelivery(t *testing.T) {
	url, offers := mockGateway(t)
	c := newTestClient(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	var operator offer
	var operatorScopes []string
	for range 2 {
		o := receiveOffer(t, offers)
		if jsonValue[string](t, o.request.Params["role"]) == "operator" {
			operator = o
			operatorScopes = jsonValue[[]string](t, o.request.Params["scopes"])
		}
		hello(t, o, "synthetic-token")
	}
	waitReady(t, c, true)
	// Match the native Gateway: targeted delivery still applies the event's
	// operator.read guard. A talk-only handshake would receive none of these.
	for _, payload := range []map[string]any{
		{"sessionId": "test-session", "type": "audio", "audioBase64": "AAAAAA=="},
		{"sessionId": "test-session", "type": "transcript", "role": "user", "text": "synthetic", "final": true},
		{"sessionId": "test-session", "type": "event", "talkEvent": map[string]any{"type": "session.error"}},
	} {
		canRead := false
		for _, scope := range operatorScopes {
			canRead = canRead || scope == "operator.read"
		}
		if canRead {
			if err := operator.socket.WriteJSON(map[string]any{"type": "event", "event": "talk.event", "payload": payload}); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case e := <-c.Events():
			if got := jsonValue[map[string]json.RawMessage](t, e.Payload); jsonValue[string](t, got["type"]) != payload["type"] {
				t.Fatal("wrong Talk event delivered")
			}
		case <-time.After(time.Second):
			t.Fatal("native read guard filtered required Talk event")
		}
	}
	if err := c.Call(ctx, "sessions.list", map[string]any{}, nil); err == nil {
		t.Fatal("read grant enabled a non-Talk RPC")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRejectedCachedOperatorTokenNeverFallsBackToSetupToken(t *testing.T) {
	t.Run("default", func(t *testing.T) { testRejectedCachedOperatorToken(t, false) })
	t.Run("personal", func(t *testing.T) { testRejectedCachedOperatorToken(t, true) })
}

func testRejectedCachedOperatorToken(t *testing.T, personal bool) {
	url, offers := mockGateway(t)
	c := newTestClient(t, url)
	c.config.PersonalAgentAccess = personal
	bootstrap := filepath.Join(c.config.StateDir, "bootstrap")
	c.config.BootstrapTokenFile = bootstrap
	for name, value := range map[string]string{"bootstrap": "synthetic-setup", "node.token": "synthetic-node", "operator.token": "synthetic-stale-operator"} {
		if err := os.WriteFile(filepath.Join(c.config.StateDir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.serve(ctx, "operator") }()
	o := receiveOffer(t, offers)
	auth := jsonValue[map[string]string](t, o.request.Params["auth"])
	if auth["deviceToken"] != "synthetic-stale-operator" || auth["bootstrapToken"] != "" || auth["token"] != "" {
		t.Fatal("cached operator token was bypassed")
	}
	if err := o.socket.WriteJSON(map[string]any{"type": "res", "id": o.request.ID, "ok": false, "error": map[string]any{"code": "DEVICE_TOKEN_MISMATCH"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || err.Error() != "DEVICE_TOKEN_MISMATCH" {
			t.Fatal("cached-token rejection was not returned")
		}
	case <-time.After(time.Second):
		t.Fatal("cached-token rejection stranded handshake")
	}
	for name, want := range map[string]string{"bootstrap": "synthetic-setup", "node.token": "synthetic-node", "operator.token": "synthetic-stale-operator"} {
		got, err := privateFile(filepath.Join(c.config.StateDir, name))
		if err != nil || string(got) != want {
			t.Fatal("rejected handshake changed local credentials")
		}
	}
}

func TestNetworkChangeReplacesBothRolesWithoutReplayingInput(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(strconv.FormatBool(ready), func(t *testing.T) {
			url, offers := mockGateway(t)
			c := newTestClient(t, url)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- c.Run(ctx) }()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("client did not stop")
				}
			}()
			for range 2 {
				o := receiveOffer(t, offers)
				if ready {
					hello(t, o, "synthetic-token")
				}
			}
			if ready {
				waitReady(t, c, true)
			}
			generation := c.ConnectionGeneration()
			c.NetworkChanged()
			roles := map[string]bool{}
			for range 2 {
				o := receiveOffer(t, offers)
				if o.request.Method != "connect" {
					t.Fatal("network change replayed input")
				}
				roles[jsonValue[string](t, o.request.Params["role"])] = true
				hello(t, o, "synthetic-new-token")
			}
			if len(roles) != 2 {
				t.Fatal("both roles were not replaced")
			}
			waitReady(t, c, true)
			if c.ConnectionGeneration() <= generation {
				t.Fatal("replacement transport pair retained its Talk connection generation")
			}
		})
	}
}
