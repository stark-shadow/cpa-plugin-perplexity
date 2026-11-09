package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type hostCallbackFunc func(context.Context, string, any) (json.RawMessage, error)

type pluginRuntime struct {
	mu          sync.Mutex
	cfg         settings
	authDir     string
	catalog     *catalogCache
	ctx         context.Context
	cancel      context.CancelFunc
	closed      bool
	active      map[string]int
	changed     chan struct{}
	workers     sync.WaitGroup
	catalogMu   sync.Mutex
	lifecycleMu sync.Mutex
	stateKey    []byte
	pending     map[string]*pendingFlow
}

func newRuntime() *pluginRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	return &pluginRuntime{cfg: defaultSettings(), ctx: ctx, cancel: cancel, active: map[string]int{}, changed: make(chan struct{}), pending: map[string]*pendingFlow{}}
}

var runtime = newRuntime()

func (rt *pluginRuntime) snapshot() (settings, string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.cfg, rt.authDir
}

func (rt *pluginRuntime) configure(cfg settings) {
	rt.lifecycleMu.Lock()
	defer rt.lifecycleMu.Unlock()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.closed {
		rt.ctx, rt.cancel = context.WithCancel(context.Background())
		rt.closed = false
	}
	rt.cfg, rt.catalog = cfg, nil
	close(rt.changed)
	rt.changed = make(chan struct{})
}

func (rt *pluginRuntime) rememberHost(host pluginapi.HostConfigSummary) {
	if host.AuthDir != "" {
		rt.mu.Lock()
		rt.authDir = host.AuthDir
		rt.mu.Unlock()
	}
}

func (rt *pluginRuntime) requestContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	rt.mu.Lock()
	parent := rt.ctx
	rt.mu.Unlock()
	return context.WithTimeout(parent, timeout)
}

func (rt *pluginRuntime) stop() {
	rt.lifecycleMu.Lock()
	defer rt.lifecycleMu.Unlock()
	rt.mu.Lock()
	rt.closed = true
	rt.cancel()
	rt.stateKey = nil
	clear(rt.pending)
	rt.mu.Unlock()
	rt.workers.Wait()
}

func (rt *pluginRuntime) beginCall() (func(), error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.closed {
		return nil, pluginabi.NewError("client_closed", "Perplexity plugin is quiescing", 499)
	}
	rt.workers.Add(1)
	return rt.workers.Done, nil
}

func (rt *pluginRuntime) startWorker(work func()) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.closed {
		return pluginabi.NewError("client_closed", "Perplexity request cancelled", 499)
	}
	rt.workers.Add(1)
	go func() {
		defer rt.workers.Done()
		work()
	}()
	return nil
}

func (rt *pluginRuntime) acquire(ctx context.Context, token string) (func(), error) {
	key := tokenHash(token)
	for {
		if err := ctx.Err(); err != nil {
			return nil, classifyError(err)
		}
		rt.mu.Lock()
		if rt.active[key] < rt.cfg.MaxConcurrency {
			rt.active[key]++
			rt.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					rt.mu.Lock()
					rt.active[key]--
					if rt.active[key] == 0 {
						delete(rt.active, key)
					}
					close(rt.changed)
					rt.changed = make(chan struct{})
					rt.mu.Unlock()
				})
			}, nil
		}
		changed := rt.changed
		rt.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, classifyError(ctx.Err())
		case <-changed:
		}
	}
}

func tokenHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func credentialName(token string) string { return "perplexity-" + tokenHash(token)[:12] + ".json" }

const (
	maxStateLength  = 128
	maxPendingFlows = 64
	cookieLoginKind = byte(1)
	loginLifetime   = 10 * time.Minute
)

type loginState struct {
	Kind    byte
	Expires int64
	Nonce   []byte
}

type pendingFlow struct {
	Expires   int64
	Auth      *pluginapi.AuthData
	Importing bool
}

func sealState(key []byte, state loginState) (string, error) {
	if state.Kind == 0 || len(state.Nonce) != 16 || state.Expires <= 0 || state.Expires > 1<<32-1 {
		return "", errors.New("invalid login state")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", errors.New("cannot generate login state")
	}
	plain := make([]byte, 21)
	plain[0] = state.Kind
	binary.BigEndian.PutUint32(plain[1:5], uint32(state.Expires))
	copy(plain[5:], state.Nonce)
	sealed := gcm.Seal(nonce, nonce, plain, []byte("perplexity-state-v1"))
	encoded := base64.RawURLEncoding.EncodeToString(sealed)
	if len(encoded) > maxStateLength {
		return "", errors.New("login state exceeds CPA limit")
	}
	return encoded, nil
}

func openState(key []byte, encoded string) (loginState, error) {
	var state loginState
	if len(encoded) > maxStateLength {
		return state, errors.New("invalid login state")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) != 12+21+16 {
		return state, errors.New("invalid login state")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return state, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return state, err
	}
	plain, err := gcm.Open(nil, raw[:12], raw[12:], []byte("perplexity-state-v1"))
	if err != nil || len(plain) != 21 || plain[0] == 0 {
		return state, errors.New("invalid login state")
	}
	return loginState{Kind: plain[0], Expires: int64(binary.BigEndian.Uint32(plain[1:5])), Nonce: append([]byte(nil), plain[5:]...)}, nil
}

func loginExpired() *pluginabi.Error {
	return pluginabi.NewError("login_expired", "Perplexity login expired or the plugin restarted; start a new login", 410)
}

func (rt *pluginRuntime) cleanupFlowsLocked() {
	for nonce, flow := range rt.pending {
		if flow.Expires <= time.Now().Unix() {
			delete(rt.pending, nonce)
		}
	}
}

func (rt *pluginRuntime) loginFlowLocked(encoded string) (*pendingFlow, error) {
	rt.cleanupFlowsLocked()
	state, err := openState(rt.stateKey, encoded)
	if err != nil || state.Kind != cookieLoginKind || state.Expires <= time.Now().Unix() {
		return nil, loginExpired()
	}
	flow := rt.pending[hex.EncodeToString(state.Nonce)]
	if flow == nil || flow.Expires != state.Expires {
		return nil, loginExpired()
	}
	return flow, nil
}

func (rt *pluginRuntime) claimLoginImport(encoded string) (func(*pluginapi.AuthData) error, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	flow, err := rt.loginFlowLocked(encoded)
	if err != nil {
		return nil, err
	}
	if flow.Auth != nil || flow.Importing {
		return nil, pluginabi.NewError("login_conflict", "This login is already complete or an import is in progress", 409)
	}
	flow.Importing = true
	var once sync.Once
	var result error
	return func(auth *pluginapi.AuthData) error {
		once.Do(func() {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			flow.Importing = false
			if auth == nil {
				return
			}
			current, err := rt.loginFlowLocked(encoded)
			if err != nil || current != flow {
				result = loginExpired()
				return
			}
			flow.Auth = auth
		})
		return result
	}, nil
}

func saveCredential(ctx context.Context, callback hostCallbackFunc, cred credential) (string, error) {
	raw, _ := json.Marshal(cred)
	name := credentialName(cred.SessionToken)
	if _, err := callback(ctx, pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: name, JSON: raw}); err != nil {
		return "", pluginabi.NewError("storage_error", "Unable to save Perplexity account", 503)
	}
	return name, nil
}

// catalogCacheDir keeps the catalog snapshot out of the credential directory
// itself: CPA scans every top-level *.json in auth-dir as an auth file, so a
// cache file there is loaded and reported as a broken credential. A subdirectory
// is skipped by that scan.
func catalogCacheDir(authDir string) string {
	if authDir == "" {
		return ""
	}
	return filepath.Join(authDir, ".perplexity-cache")
}

func readCatalogFile(dir, base string, ttl time.Duration) *catalogCache {
	cacheDir := catalogCacheDir(dir)
	if cacheDir == "" {
		return nil
	}
	file, err := os.Open(filepath.Join(cacheDir, "catalog.json"))
	if err != nil {
		return nil
	}
	defer file.Close()
	var cache catalogCache
	if json.NewDecoder(file).Decode(&cache) != nil || cache.BaseURL != base || time.Since(cache.FetchedAt) < 0 || time.Since(cache.FetchedAt) >= ttl || len(cache.Catalog.Models) == 0 {
		return nil
	}
	return &cache
}

func writeCatalogFile(dir string, cache catalogCache) {
	cacheDir := catalogCacheDir(dir)
	if cacheDir == "" {
		return
	}
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return
	}
	file, err := os.CreateTemp(cacheDir, "catalog-*")
	if err != nil {
		return
	}
	defer os.Remove(file.Name())
	encodeErr := json.NewEncoder(file).Encode(cache)
	closeErr := file.Close()
	if encodeErr == nil && closeErr == nil {
		_ = os.Rename(file.Name(), filepath.Join(cacheDir, "catalog.json"))
	}
}
