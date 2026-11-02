// Package openclaw provides paired Gateway transports, without agent or speech policy.
package openclaw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type Config struct {
	URL, StateDir, DisplayName, BootstrapTokenFile, BootstrapRole string
	PersonalAgentAccess                                           bool
}
type Event struct{ Payload json.RawMessage }
type frame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Event   string          `json:"event"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload"`
	Error   struct {
		Code string `json:"code"`
	} `json:"error"`
}
type connection struct {
	socket  *websocket.Conn
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan frame
	closed  bool
}
type Client struct {
	config               Config
	identity             identity
	events               chan Event
	mu                   sync.Mutex
	connections          map[string]*connection
	running              bool
	attempts             map[string]context.CancelFunc
	networkWake          map[string]chan struct{}
	sequence             atomic.Uint64
	connectionGeneration uint64     // guarded by mu; losing either role revokes the pair
	tokenMu              sync.Mutex // both role connections publish into one private state directory
}

const heartbeatInterval = 15 * time.Second
const peerTimeout = 45 * time.Second

func New(config Config) (*Client, error) {
	u, err := url.Parse(config.URL)
	if err != nil || len(config.URL) > 4096 || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(config.URL, "#") || u.Opaque != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid OpenClaw Gateway URL")
	}
	if u.Scheme != "wss" && !(u.Scheme == "ws" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return nil, errors.New("OpenClaw requires wss, except exact loopback ws")
	}
	if config.StateDir == "" {
		return nil, errors.New("OpenClaw state directory is required")
	}
	i, err := loadIdentity(config.StateDir)
	if err != nil {
		return nil, err
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = ""
	config.URL = u.String()
	if err := bindGateway(config.StateDir, config.URL); err != nil {
		return nil, err
	}
	if config.DisplayName == "" {
		config.DisplayName = "OpenClaw Echo"
	}
	return &Client{config: config, identity: i, events: make(chan Event, 128), connections: make(map[string]*connection)}, nil
}

func (c *Client) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connections["node"] != nil && c.connections["operator"] != nil
}
func (c *Client) Events() <-chan Event { return c.events }

// ConnectionGeneration fences logical Talk sessions against even a reconnect
// that completes between the device's readiness polls. It grants no authority.
func (c *Client) ConnectionGeneration() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectionGeneration
}

// Run reconnects each role independently. Pairing never grants additional scopes.
func (c *Client) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("OpenClaw client already running")
	}
	c.running = true
	c.attempts = make(map[string]context.CancelFunc)
	c.networkWake = map[string]chan struct{}{"node": make(chan struct{}, 1), "operator": make(chan struct{}, 1)}
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.running = false; c.mu.Unlock() }()
	log.Printf("OpenClaw device identity: %s", c.identity.id)
	var group sync.WaitGroup
	for _, role := range []string{"node", "operator"} {
		group.Add(1)
		go func(role string) {
			defer group.Done()
			delay := time.Second
			for ctx.Err() == nil {
				attempt, cancel := context.WithCancel(ctx)
				c.mu.Lock()
				c.attempts[role] = cancel
				c.mu.Unlock()
				err := c.serve(attempt, role)
				cancel()
				c.mu.Lock()
				delete(c.attempts, role)
				c.mu.Unlock()
				if ctx.Err() != nil {
					return
				}
				log.Printf("OpenClaw %s connection unavailable: %s", role, safeError(err))
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				case <-c.networkWake[role]:
					timer.Stop()
					delay = time.Second
					continue
				}
				if delay < 30*time.Second {
					delay *= 2
					if delay > 30*time.Second {
						delay = 30 * time.Second
					}
				}
			}
		}(role)
	}
	group.Wait()
	return ctx.Err()
}

// NetworkChanged revokes both old transports, including in-flight handshakes,
// and wakes their normal reconnect owners. It never creates or replays a turn.
func (c *Client) NetworkChanged() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cancel := range c.attempts {
		cancel()
	}
	for _, wake := range c.networkWake {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

var codePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

type codeError string

func (e codeError) Error() string { return string(e) }
func safeError(err error) string {
	var code codeError
	if errors.As(err, &code) && codePattern.MatchString(string(code)) {
		return string(code)
	}
	var closed *websocket.CloseError
	if errors.As(err, &closed) {
		return fmt.Sprintf("connection closed (%d)", closed.Code)
	}
	return "connection failed"
}

func (c *Client) serve(ctx context.Context, role string) error {
	socket, _, err := websocket.DefaultDialer.DialContext(ctx, c.config.URL, nil)
	if err != nil {
		return err
	}
	defer socket.Close()
	socket.SetReadLimit(4 << 20)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			socket.Close()
		case <-done:
		}
	}()
	conn := &connection{socket: socket, pending: make(map[string]chan frame)}
	if err = c.handshake(conn, role); err != nil {
		return err
	}
	if err = socket.SetReadDeadline(time.Now().Add(peerTimeout)); err != nil {
		return err
	}
	socket.SetPongHandler(func(string) error { return socket.SetReadDeadline(time.Now().Add(peerTimeout)) })
	go func() {
		tick := time.NewTicker(heartbeatInterval)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				if socket.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					socket.Close()
					return
				}
			}
		}
	}()
	c.mu.Lock()
	c.connections[role] = conn
	c.connectionGeneration++
	c.mu.Unlock()
	defer func() {
		// Both transports describe one device. Losing either fences the complete
		// pair, including pending operator calls; a new pair cannot inherit it.
		var peers []*connection
		c.mu.Lock()
		if c.connections[role] == conn {
			c.connectionGeneration++
			for other, peer := range c.connections {
				delete(c.connections, other)
				if peer != conn {
					peers = append(peers, peer)
				}
			}
		}
		c.mu.Unlock()
		for _, peer := range peers {
			peer.socket.Close()
		}
		conn.mu.Lock()
		conn.closed = true
		for id, response := range conn.pending {
			delete(conn.pending, id)
			close(response)
		}
		conn.mu.Unlock()
	}()
	for {
		var incoming frame
		if err := socket.ReadJSON(&incoming); err != nil {
			return err
		}
		if incoming.Type == "res" {
			conn.mu.Lock()
			if response := conn.pending[incoming.ID]; response != nil {
				delete(conn.pending, incoming.ID)
				response <- incoming
			}
			conn.mu.Unlock()
		} else if incoming.Type == "event" && incoming.Event == "talk.event" && role == "operator" {
			select {
			case c.events <- Event{Payload: incoming.Payload}:
			default:
				return codeError("EVENT_QUEUE_FULL")
			}
		} else if incoming.Type == "event" && incoming.Event == "node.invoke.request" && role == "node" {
			var invoke struct {
				ID     string `json:"id"`
				NodeID string `json:"nodeId"`
			}
			if json.Unmarshal(incoming.Payload, &invoke) != nil || invoke.ID == "" || invoke.NodeID != c.identity.id {
				return codeError("INVALID_NODE_INVOKE")
			}
			err := conn.write(map[string]any{"type": "req", "id": c.nextID(), "method": "node.invoke.result", "params": map[string]any{"id": invoke.ID, "nodeId": c.identity.id, "ok": false, "error": map[string]any{"code": "INVALID_REQUEST", "message": "Echo exposes no node commands"}}})
			if err != nil {
				return err
			}
		}
	}
}

func (c *Client) nextID() string { return fmt.Sprintf("echo-%d", c.sequence.Add(1)) }
func (conn *connection) write(value any) error {
	conn.writeMu.Lock()
	defer conn.writeMu.Unlock()
	if err := conn.socket.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return conn.socket.WriteJSON(value)
}

func (c *Client) roleScopes(role string) []string {
	if role != "operator" {
		return []string{}
	}
	scopes := []string{"operator.read", "operator.talk"}
	if c.config.PersonalAgentAccess {
		scopes = append(scopes, "operator.write")
	}
	return scopes
}

// Receipt order may vary; duplicates, missing and additional scopes may not.
func (c *Client) exactRoleScopes(role string, scopes []string) bool {
	if role != "operator" && role != "node" {
		return false
	}
	want := c.roleScopes(role)
	if len(scopes) != len(want) {
		return false
	}
	seen := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		allowed := false
		for _, expected := range want {
			allowed = allowed || scope == expected
		}
		if !allowed || seen[scope] {
			return false
		}
		seen[scope] = true
	}
	return true
}

func (c *Client) handshake(conn *connection, role string) error {
	conn.socket.SetReadDeadline(time.Now().Add(15 * time.Second))
	var challenge frame
	if err := conn.socket.ReadJSON(&challenge); err != nil {
		return err
	}
	var payload struct {
		Nonce string `json:"nonce"`
		TS    int64  `json:"ts"`
	}
	if challenge.Type != "event" || challenge.Event != "connect.challenge" || json.Unmarshal(challenge.Payload, &payload) != nil || payload.Nonce == "" || len(payload.Nonce) > 4096 || strings.Contains(payload.Nonce, "|") || payload.TS < 0 {
		return codeError("INVALID_CHALLENGE")
	}
	if delta := time.Now().UnixMilli() - payload.TS; delta > 120000 || delta < -120000 {
		return codeError("CHALLENGE_CLOCK_SKEW")
	}
	scopes := c.roleScopes(role)
	raw, err := privateFile(filepath.Join(c.config.StateDir, role+".token"))
	if err != nil && !os.IsNotExist(err) {
		return codeError("INVALID_TOKEN_FILE")
	}
	token := string(raw)
	if (err == nil && token == "") || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\r\n|") {
		return codeError("INVALID_TOKEN_FILE")
	}
	bootstrap := false
	// Fresh voice-node setup starts as node and hands off the operator grant.
	// The default remains operator for existing role-specific renewal setups.
	bootstrapRole := c.config.BootstrapRole
	if bootstrapRole == "" {
		bootstrapRole = "operator"
	}
	if bootstrapRole != "operator" && bootstrapRole != "node" {
		return codeError("INVALID_BOOTSTRAP_ROLE")
	}
	if token == "" && role == bootstrapRole && c.config.BootstrapTokenFile != "" {
		raw, err = privateFile(c.config.BootstrapTokenFile)
		if err != nil {
			return codeError("INVALID_BOOTSTRAP_FILE")
		}
		token = string(raw)
		if token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\r\n|") {
			return codeError("INVALID_BOOTSTRAP_FILE")
		}
		bootstrap = true
	}
	params := map[string]any{"minProtocol": 4, "maxProtocol": 4, "client": map[string]any{"id": "node-host", "mode": "node", "version": "0.1.0", "platform": "linux", "deviceFamily": "echo", "displayName": c.config.DisplayName, "instanceId": c.identity.id}, "role": role, "scopes": scopes, "device": c.identity.device(role, scopes, payload.Nonce, payload.TS, token)}
	if role == "node" {
		params["caps"] = []string{"talk"}
		params["commands"] = []string{}
	}
	if token != "" {
		key := "deviceToken"
		if bootstrap {
			// Native short-lived device setup credential, never a shared Gateway secret.
			key = "bootstrapToken"
		}
		params["auth"] = map[string]string{key: token}
	}
	id := c.nextID()
	if err := conn.write(map[string]any{"type": "req", "id": id, "method": "connect", "params": params}); err != nil {
		return err
	}
	var response frame
	// Pairing and Node events may precede hello-ok. They are not admitted to
	// the application before authentication; the original deadline still applies.
	for skipped := 0; ; skipped++ {
		if err := conn.socket.ReadJSON(&response); err != nil {
			return err
		}
		if response.Type != "event" {
			break
		}
		if skipped >= 15 || response.Event == "connect.challenge" {
			return codeError("INVALID_CONNECT_RESPONSE")
		}
	}
	if response.Type != "res" || response.ID != id {
		return codeError("INVALID_CONNECT_RESPONSE")
	}
	if !response.OK {
		if codePattern.MatchString(response.Error.Code) {
			return codeError(response.Error.Code)
		}
		return codeError("CONNECT_REJECTED")
	}
	var hello struct {
		Type     string `json:"type"`
		Protocol int    `json:"protocol"`
		Auth     struct {
			DeviceToken  string   `json:"deviceToken"`
			Role         string   `json:"role"`
			Scopes       []string `json:"scopes"`
			DeviceTokens []struct {
				DeviceToken string   `json:"deviceToken"`
				Role        string   `json:"role"`
				Scopes      []string `json:"scopes"`
			} `json:"deviceTokens"`
		} `json:"auth"`
	}
	if json.Unmarshal(response.Payload, &hello) != nil || hello.Type != "hello-ok" || hello.Protocol != 4 || hello.Auth.Role != role {
		return codeError("INVALID_HELLO")
	}
	if !c.exactRoleScopes(role, hello.Auth.Scopes) {
		return codeError("UNEXPECTED_ROLE_SCOPES")
	}
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	// Accept exactly the complementary grant from the selected bootstrap role.
	// Validate all grants before persisting either, preserving cached receipts.
	var companion string
	companionRole := "node"
	if role == "node" {
		companionRole = "operator"
	}
	if len(hello.Auth.DeviceTokens) > 0 {
		if !bootstrap || len(hello.Auth.DeviceTokens) != 1 || !validToken(hello.Auth.DeviceToken) {
			return codeError("UNEXPECTED_COMPANION_GRANT")
		}
		grant := hello.Auth.DeviceTokens[0]
		grantScopesOK := c.exactRoleScopes(companionRole, grant.Scopes)
		if grant.Role != companionRole || !grantScopesOK || !validToken(grant.DeviceToken) || grant.DeviceToken == hello.Auth.DeviceToken {
			return codeError("UNEXPECTED_COMPANION_GRANT")
		}
		previous, err := privateFile(filepath.Join(c.config.StateDir, companionRole+".token"))
		if err == nil {
			if string(previous) != grant.DeviceToken {
				return codeError("EXISTING_COMPANION_GRANT_CHANGED")
			}
		} else if os.IsNotExist(err) {
			companion = grant.DeviceToken
		} else {
			return codeError("INVALID_TOKEN_FILE")
		}
	}
	if companion != "" {
		if err := storeToken(c.config.StateDir, companionRole, companion); err != nil {
			return codeError("TOKEN_STORE_FAILED")
		}
	}
	// A cached receipt can be echoed unchanged. Keep its inode and timestamps,
	// especially the existing node credential during operator-only renewal.
	if hello.Auth.DeviceToken != "" && (bootstrap || hello.Auth.DeviceToken != token) {
		if err := storeToken(c.config.StateDir, role, hello.Auth.DeviceToken); err != nil {
			return codeError("TOKEN_STORE_FAILED")
		}
	}
	return conn.socket.SetReadDeadline(time.Time{})
}

// Call only sends Talk RPCs on the narrowly scoped operator connection.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	if !strings.HasPrefix(method, "talk.") {
		return errors.New("only Talk RPCs are supported")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	conn := c.connections["operator"]
	c.mu.Unlock()
	if conn == nil {
		return errors.New("OpenClaw Talk connection unavailable")
	}
	id := c.nextID()
	response := make(chan frame, 1)
	conn.mu.Lock()
	if conn.closed || len(conn.pending) >= 128 {
		conn.mu.Unlock()
		return errors.New("OpenClaw connection unavailable or request limit reached")
	}
	conn.pending[id] = response
	conn.mu.Unlock()
	defer func() { conn.mu.Lock(); delete(conn.pending, id); conn.mu.Unlock() }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := conn.write(map[string]any{"type": "req", "id": id, "method": method, "params": params}); err != nil {
		conn.socket.Close()
		return errors.New("OpenClaw request send failed")
	}
	select {
	case <-ctx.Done():
		// An accepted mutation can outlive its caller before its result arrives.
		// Close the owning connection instead of retaining unknown native resources.
		conn.socket.Close()
		return ctx.Err()
	case reply, ok := <-response:
		if !ok {
			return errors.New("OpenClaw connection closed")
		}
		if !reply.OK {
			if codePattern.MatchString(reply.Error.Code) {
				return codeError(reply.Error.Code)
			}
			return codeError("REQUEST_REJECTED")
		}
		if result != nil && json.Unmarshal(reply.Payload, result) != nil {
			return errors.New("invalid OpenClaw response")
		}
		return nil
	}
}
