package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// A file reload the selector factory rejects must not be adopted by the watcher:
// otherwise its recorded hash skips the retry once the factory accepts the file.
func TestSelectorFactoryRejectsWatcherReloadBeforeAdoption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	authDir := filepath.Join(dir, "auth")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var rejectFillFirst atomic.Bool
	service, running := factoryErrorService(func(*config.Config) (coreauth.Selector, error) {
		if rejectFillFirst.Load() {
			return nil, errors.New("fill-first policy unavailable")
		}
		return &factoryOwnedSelector{}, nil
	})
	wrapper, err := defaultWatcherFactory(path, authDir, service.applyWatcherConfigUpdate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := wrapper.Stop(); err != nil {
			t.Errorf("stop watcher fixture: %v", err)
		}
	})
	// Run installs the same check on the watcher it creates.
	wrapper.SetConfigValidator(service.checkRoutingSelector)

	rejectFillFirst.Store(true)
	rejected := []byte("auth-dir: " + authDir + "\nrouting: {strategy: fill-first}\n")
	if err := os.WriteFile(path, rejected, 0o600); err != nil {
		t.Fatal(err)
	}
	wrapper.ReloadConfigIfChanged()
	if got := service.currentConfig().Routing.Strategy; got == "fill-first" {
		t.Fatal("service published a config its selector factory rejected")
	}
	if service.coreManager.Selector() != running || running.stops.Load() != 0 {
		t.Fatal("rejected reload replaced or stopped the running selector")
	}
	if onDisk, errRead := os.ReadFile(path); errRead != nil || string(onDisk) != string(rejected) {
		t.Fatalf("rejected reload rewrote the config file: %q %v", onDisk, errRead)
	}

	// Same bytes, factory now accepts: a watcher that adopted the rejected file skips this.
	rejectFillFirst.Store(false)
	wrapper.ReloadConfigIfChanged()
	if got := service.currentConfig().Routing.Strategy; got != "fill-first" {
		t.Fatalf("watcher adopted the rejected file and skipped its retry: strategy %q", got)
	}
	if service.coreManager.Selector() == running || running.stops.Load() != 1 {
		t.Fatal("accepted retry did not replace the previous selector")
	}
}

// A management save the selector factory rejects fails before it reaches disk.
func TestSelectorFactoryRejectsManagementSaveBeforeWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("host: 127.0.0.1\nport: 8317\napi-keys: [client]\nremote-management: {secret-key: test-password}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthDir = t.TempDir()
	installed := &factoryOwnedSelector{}
	manager := coreauth.NewManager(nil, nil, nil)
	service, err := NewBuilder().WithConfig(cfg).WithConfigPath(path).WithCoreAuthManager(manager).
		WithSelectorFactory(func(c *config.Config) (coreauth.Selector, error) {
			if c.Routing.Strategy == "fill-first" {
				return nil, errors.New("fill-first policy unavailable")
			}
			return installed, nil
		}).Build()
	if err != nil {
		t.Fatal(err)
	}
	options := append([]api.ServerOption(nil), service.serverOptions...)
	options = append(options, api.WithLocalManagementPassword("test-password"))
	server := api.NewServer(cfg, nil, nil, path, options...)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Stop(ctx); err != nil {
			t.Errorf("stop API fixture: %v", err)
		}
		if err := service.Shutdown(ctx); err != nil {
			t.Errorf("stop service fixture: %v", err)
		}
	})
	request := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Authorization", "Bearer test-password")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if response := request(http.MethodPatch, "/v8/management/config", `{"routing":{"strategy":"fill-first"}}`); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("management saved a config the selector factory rejects: %d %s", response.Code, response.Body.String())
	}
	if after, errRead := os.ReadFile(path); errRead != nil || string(after) != string(before) {
		t.Fatalf("rejected save changed the config file: %q %v", after, errRead)
	}
	if response := request(http.MethodGet, "/v8/management/config/routing/strategy", ""); strings.Contains(response.Body.String(), "fill-first") {
		t.Fatalf("rejected save changed management state: %s", response.Body.String())
	}
	if manager.Selector() != installed || installed.stops.Load() != 0 {
		t.Fatal("rejected save replaced or stopped the running selector")
	}
}
