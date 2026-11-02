package openclaw

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type identity struct {
	key    ed25519.PrivateKey
	id     string
	public string
}

func privateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 4096 {
		return nil, errors.New("OpenClaw state file must be regular and mode 0600")
	}
	return os.ReadFile(path)
}

func loadIdentity(dir string) (identity, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return identity{}, errors.New("cannot create OpenClaw state directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return identity{}, errors.New("OpenClaw state directory must be regular and mode 0700")
	}
	path := filepath.Join(dir, "identity.seed")
	raw, err := privateFile(path)
	if os.IsNotExist(err) {
		raw = make([]byte, ed25519.SeedSize)
		if _, err = rand.Read(raw); err != nil {
			return identity{}, errors.New("cannot generate OpenClaw identity")
		}
		var file *os.File
		file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if os.IsExist(err) {
			raw, err = privateFile(path)
		} else if err == nil {
			_, err = file.Write(raw)
			if err == nil {
				err = file.Sync()
			}
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
		}
	}
	if err != nil || len(raw) != ed25519.SeedSize {
		return identity{}, errors.New("cannot load private OpenClaw identity")
	}
	key := ed25519.NewKeyFromSeed(raw)
	public := key.Public().(ed25519.PublicKey)
	hash := sha256.Sum256(public)
	return identity{key: key, id: hex.EncodeToString(hash[:]), public: base64.RawURLEncoding.EncodeToString(public)}, nil
}

func (i identity) device(role string, scopes []string, nonce string, timestamp int64, token string) map[string]any {
	// Byte contract: OpenClaw packages/gateway-client/src/device-auth.ts, v3.
	payload := strings.Join([]string{"v3", i.id, "node-host", "node", role, strings.Join(scopes, ","), strconv.FormatInt(timestamp, 10), token, nonce, "linux", "echo"}, "|")
	return map[string]any{"id": i.id, "publicKey": i.public, "signature": base64.RawURLEncoding.EncodeToString(ed25519.Sign(i.key, []byte(payload))), "signedAt": timestamp, "nonce": nonce}
}

func validToken(token string) bool {
	return token != "" && strings.TrimSpace(token) == token && !strings.ContainsAny(token, "\r\n|") && len(token) <= 4096
}

func storeToken(dir, role, token string) error {
	if !validToken(token) {
		return errors.New("invalid OpenClaw token receipt")
	}
	path := filepath.Join(dir, role+".token")
	if _, err := privateFile(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(dir, ".token-")
	if err != nil {
		return errors.New("cannot save OpenClaw role token")
	}
	defer os.Remove(file.Name())
	err = file.Chmod(0600)
	if err == nil {
		_, err = file.WriteString(token)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	if err != nil {
		return errors.New("cannot save OpenClaw role token")
	}
	return nil
}

// A paired credential belongs to its original canonical Gateway endpoint.
// Settings imports must select a new state directory to enroll another Gateway.
func bindGateway(dir, gatewayURL string) error {
	path := filepath.Join(dir, "gateway.url")
	raw, err := privateFile(path)
	if os.IsNotExist(err) {
		for _, role := range []string{"node", "operator"} {
			if _, tokenErr := os.Lstat(filepath.Join(dir, role+".token")); !os.IsNotExist(tokenErr) {
				return errors.New("unbound OpenClaw credentials require a new state directory")
			}
		}
		var file *os.File
		file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if os.IsExist(err) {
			raw, err = privateFile(path)
		} else if err == nil {
			_, err = file.WriteString(gatewayURL)
			if err == nil {
				err = file.Sync()
			}
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
			raw = []byte(gatewayURL)
		}
	}
	if err != nil {
		return errors.New("cannot load OpenClaw Gateway binding")
	}
	if string(raw) != gatewayURL {
		return errors.New("OpenClaw Gateway changed; select a new state directory for pairing")
	}
	return nil
}
