package management

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"
)

func TestConfigSaveWatcherNeverPublishesFailedWrite(t *testing.T) {
	for _, endpoint := range []string{"WriteConfig", "/v0/management/config.yaml", "/v8/management/config.yaml", "/v8/management/config"} {
		for _, operation := range []string{"sync", "close"} {
			t.Run(endpoint+"/"+operation, func(t *testing.T) {
				h, path, router := newFullDocumentHandler(t)
				h.SetConfigValidator(func(*config.Config) error { return nil })
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				inode, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				// This handler holds the runtime snapshot accepted from the real watcher,
				// independently of the management handler's in-progress save.
				runtime := &Handler{cfg: h.cfg.CloneForRuntime()}
				runtime.SetConfigValidator(func(*config.Config) error { return nil })
				var observed []int
				w, err := watcher.NewWatcher(path, t.TempDir(), func(cfg *config.Config) {
					runtime.SetConfig(cfg)
					observed = append(observed, runtime.acceptedConfig.Port)
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := w.Stop(); err != nil {
						t.Error(err)
					}
				})
				w.SetConfig(h.cfg)
				w.SetConfigValidator(func(*config.Config) error { return nil })
				fault, _ := injectConfigWriteFault(t, path, operation, "")

				synctest.Test(t, func(t *testing.T) {
					w.ReloadConfigIfChanged()
					atFailure, release := make(chan struct{}), make(chan struct{})
					fault.after = func() {
						close(atFailure)
						<-release
					}
					save := func() error {
						if endpoint == "WriteConfig" {
							return WriteConfig(path, []byte("port: 8320\n"))
						}
						body := "port: 8320\n"
						if endpoint == "/v8/management/config" {
							body = `{"server":{"port":8320}}`
						} else if endpoint == "/v8/management/config.yaml" {
							body = "config-version: 8\nserver: {port: 8320}\n"
						}
						response := httptest.NewRecorder()
						router.ServeHTTP(response, httptest.NewRequest(http.MethodPut, endpoint, strings.NewReader(body)))
						if response.Code != http.StatusInternalServerError {
							t.Errorf("failed save status = %d, want 500", response.Code)
						}
						return nil
					}
					done := make(chan error, 1)
					go func() { done <- save() }()
					select {
					case <-atFailure:
					case err := <-done:
						t.Fatalf("save returned before injected fault: %v", err)
					}
					candidate, err := os.ReadFile(path)
					if err != nil || string(candidate) == string(original) {
						t.Error("fault did not pause after candidate bytes reached disk")
					}
					// Exercise the actual filesystem-event reload path while the writer is
					// paused after I/O. Wait observes quiescence, without a timing window.
					go w.ReloadConfigIfChanged()
					synctest.Wait()
					if runtime.acceptedConfig.Port != 8317 || !reflect.DeepEqual(observed, []int{8317}) {
						t.Errorf("uncommitted candidate reached runtime: port=%d, observations=%v", runtime.acceptedConfig.Port, observed)
					}
					close(release)
					if err := <-done; endpoint == "WriteConfig" && !errors.Is(err, fault.err) {
						t.Errorf("save error = %v, want injected failure", err)
					}
					synctest.Wait()
					w.ReloadConfigIfChanged()
					if runtime.acceptedConfig.Port != 8317 || h.acceptedConfig.Port != 8317 {
						t.Error("failed save changed accepted runtime or management config")
					}
					for _, port := range observed {
						if port != 8317 {
							t.Errorf("failed candidate was transiently published: %v", observed)
							break
						}
					}
					restored, err := os.ReadFile(path)
					if err != nil || string(restored) != string(original) {
						t.Error("failed save did not restore original bytes")
					}
					if err := WriteConfig(path, []byte("port: 8321\n")); err != nil {
						t.Fatal(err)
					}
					w.ReloadConfigIfChanged()
					if runtime.acceptedConfig.Port != 8321 {
						t.Fatal("subsequent successful save was not published")
					}
					if err := os.WriteFile(path, []byte("port: 8322\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					w.ReloadConfigIfChanged()
					if runtime.acceptedConfig.Port != 8322 {
						t.Fatal("external edit was not published")
					}
					after, err := os.Stat(path)
					if err != nil || !os.SameFile(inode, after) {
						t.Error("save replaced the mounted file inode")
					}
				})
			})
		}
	}
}
