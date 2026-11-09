package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
}

func newRuntime() *pluginRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	return &pluginRuntime{cfg: defaultSettings(), ctx: ctx, cancel: cancel, active: map[string]int{}, changed: make(chan struct{})}
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

func saveCredential(ctx context.Context, callback hostCallbackFunc, cred credential) (string, error) {
	raw, _ := json.Marshal(cred)
	name := credentialName(cred.SessionToken)
	if _, err := callback(ctx, pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: name, JSON: raw}); err != nil {
		return "", pluginabi.NewError("storage_error", "Unable to save Perplexity account", 503)
	}
	return name, nil
}

func readCatalogFile(dir, base string, ttl time.Duration) *catalogCache {
	if dir == "" {
		return nil
	}
	file, err := os.Open(filepath.Join(dir, ".perplexity-catalog.json"))
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
	if dir == "" {
		return
	}
	file, err := os.CreateTemp(dir, ".perplexity-catalog-*")
	if err != nil {
		return
	}
	defer os.Remove(file.Name())
	encodeErr := json.NewEncoder(file).Encode(cache)
	closeErr := file.Close()
	if encodeErr == nil && closeErr == nil {
		_ = os.Rename(file.Name(), filepath.Join(dir, ".perplexity-catalog.json"))
	}
}
