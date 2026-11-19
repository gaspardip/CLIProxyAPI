package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

func TestFullDocumentSavePublishesValidatedSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, url, body string
		legacy          bool
	}{
		{"v0 legacy YAML", "/v0/management/config.yaml", "# saved comment\nhost: 127.0.0.1\nport: 8320\nremote-management: {secret-key: snapshot-fixture}\noauth-model-alias: {' CODEX ': [{name: upstream, alias: public}]}\n", true},
		{"v0 v8 YAML", "/v0/management/config.yaml", "# saved comment\nconfig-version: 8\nserver: {host: 127.0.0.1, port: 8320}\nmanagement: {secret-key: snapshot-fixture}\noauth: {model-alias: {' CODEX ': [{name: upstream, alias: public}]}}\n", true},
		{"v0 mixed YAML", "/v0/management/config.yaml", "# saved comment\nserver: {host: 127.0.0.1, port: 8320}\nremote-management: {secret-key: snapshot-fixture}\nws-auth: false\noauth-model-alias: {' CODEX ': [{name: upstream, alias: public}]}\n", true},
		{"v8 YAML", "/v8/management/config.yaml", "# saved comment\nconfig-version: 8\nserver: {host: 127.0.0.1, port: 8320}\nmanagement: {secret-key: snapshot-fixture}\noauth: {model-alias: {' CODEX ': [{name: upstream, alias: public}]}}\n", false},
		{"v8 JSON", "/v8/management/config", `{"server":{"host":"127.0.0.1","port":8320},"management":{"secret-key":"snapshot-fixture"},"oauth":{"model-alias":{" CODEX ":[{"name":"upstream","alias":"public"}]}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, path, router := newFullDocumentHandler(t)
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			var validated *config.Config
			h.SetConfigValidator(func(next *config.Config) error {
				validated = next.CloneForRuntime()
				// The validator receives its own clone, not mutable handler state.
				next.Host = "validator-local"
				return nil
			})
			reloads := make(chan *config.Config, 1)
			h.SetConfigReloadHook(func(_ context.Context, next *config.Config) { reloads <- next })
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, tc.url, strings.NewReader(tc.body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("save status=%d body=%s", rec.Code, rec.Body.String())
			}
			if validated == nil {
				t.Fatal("save did not run application validation")
			}
			if validated.Host != "127.0.0.1" || validated.Port != 8320 || len(validated.OAuthModelAlias["codex"]) != 1 || validated.OAuthModelAlias["codex"][0].Name != "upstream" {
				t.Fatal("validator did not see the normalized request")
			}
			if err := bcrypt.CompareHashAndPassword([]byte(validated.RemoteManagement.SecretKey), []byte("snapshot-fixture")); err != nil {
				t.Fatal("validator did not see the prepared management key")
			}
			published := h.cfg.CloneForRuntime()
			if !reflect.DeepEqual(published, validated) {
				t.Fatal("handler published a different config from the validated snapshot")
			}
			if !reflect.DeepEqual(h.acceptedConfig, h.cfg) {
				t.Fatal("accepted rollback baseline differs from the published config")
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var disk config.Config
			if err := yaml.Unmarshal(saved, &disk); err != nil {
				t.Fatal(err)
			}
			if disk.Host != "127.0.0.1" || disk.Port != 8320 || len(disk.OAuthModelAlias[" CODEX "]) != 1 {
				t.Fatal("saved document lost request values")
			}
			if !reflect.DeepEqual(disk.OAuthOnlyFields, h.cfg.OAuthOnlyFields) {
				t.Fatal("published OAuth scope differs from the saved document")
			}
			if strings.Contains(tc.body, "# saved comment") && !strings.Contains(string(saved), "# saved comment") {
				t.Fatal("save dropped the operator comment")
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) {
				t.Fatal("save replaced the mounted config inode")
			}
			if tc.legacy {
				if disk.RemoteManagement.SecretKey != validated.RemoteManagement.SecretKey {
					t.Fatal("legacy save did not persist the validated management-key hash")
				}
				if h.cfg.SourceHash != config.ComputeSourceHash(saved) {
					t.Fatal("legacy snapshot source hash does not identify the saved bytes")
				}
			} else {
				select {
				case next := <-reloads:
					if !reflect.DeepEqual(next, validated) {
						t.Fatal("reload received a different config from the validated snapshot")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("save did not publish its reload snapshot")
				}
			}
		})
	}
}

func newFullDocumentHandler(t *testing.T) (*Handler, string, *gin.Engine) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("host: 127.0.0.1\nport: 8317\nrequest-retry: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.PUT("/v0/management/config.yaml", h.PutConfigYAML)
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		router.Handle(method, "/v8/management/config", h.ConfigV8)
		router.Handle(method, "/v8/management/config.yaml", h.ConfigV8)
		router.Handle(method, "/v8/management/config/*path", h.ConfigV8)
	}
	return h, path, router
}

func TestFullDocumentSaveRejectsAmbiguousOAuthChannels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, field := range []struct{ name, value string }{
		{"model-alias", `[{"name":"upstream","alias":"public"}]`},
		{"excluded-models", `["private-*"]`},
		{"settings", `[{"name":"upstream","max-context-length":8192}]`},
		{"request-scoped-errors", `[{"status":400,"match":["invalid input"],"action":"cooldown"}]`},
	} {
		for _, endpoint := range []struct{ name, method, url, template string }{
			{"v0 legacy YAML", http.MethodPut, "/v0/management/config.yaml", "oauth-%s: %s\n"},
			{"v0 v8 YAML", http.MethodPut, "/v0/management/config.yaml", "config-version: 8\noauth:\n  %s: %s\n"},
			{"v8 YAML", http.MethodPut, "/v8/management/config.yaml", "config-version: 8\noauth:\n  %s: %s\n"},
			{"v8 JSON", http.MethodPut, "/v8/management/config", `{"oauth":{"%s":%s}}`},
			{"v8 PATCH", http.MethodPatch, "/v8/management/config", `{"oauth":{"%s":%s}}`},
			{"v8 map PUT", http.MethodPut, "/v8/management/config/oauth/" + field.name, `%[2]s`},
			{"v8 map PATCH", http.MethodPatch, "/v8/management/config/oauth/" + field.name, `%[2]s`},
		} {
			t.Run(field.name+"/"+endpoint.name, func(t *testing.T) {
				h, path, router := newFullDocumentHandler(t)
				before := h.cfg.CloneForRuntime()
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				h.SetConfigValidator(func(*config.Config) error {
					t.Error("ambiguous config reached application validation")
					return nil
				})
				channels := fmt.Sprintf(`{"codex":%s," CODEX ":%s}`, field.value, field.value)
				body := fmt.Sprintf(endpoint.template, field.name, channels)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(endpoint.method, endpoint.url, strings.NewReader(body)))
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "normalize to") {
					t.Fatalf("ambiguous save status=%d body=%s", rec.Code, rec.Body.String())
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != string(original) || !reflect.DeepEqual(h.cfg, before) || !reflect.DeepEqual(h.acceptedConfig, before) || h.reloadGeneration != 0 {
					t.Fatal("rejected save changed disk, handler, rollback baseline, or reload generation")
				}
			})
		}
	}
}

func TestFullDocumentSaveFailureDoesNotPublish(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, url := range []string{"/v0/management/config.yaml", "/v8/management/config.yaml", "/v8/management/config"} {
		for _, failure := range []string{"application rejection", "write failure"} {
			t.Run(url+"/"+failure, func(t *testing.T) {
				h, path, router := newFullDocumentHandler(t)
				before := h.cfg.CloneForRuntime()
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				wantStatus := http.StatusUnprocessableEntity
				if failure == "write failure" {
					wantStatus = http.StatusInternalServerError
				}
				h.SetConfigValidator(func(*config.Config) error {
					if failure == "application rejection" {
						return errors.New("application rejected the config")
					}
					// Model a filesystem failure after validation without permission
					// assumptions (tests also run as root).
					if err := os.Rename(path, path+".original"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
					return nil
				})
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, url, strings.NewReader(`{"server":{"port":8320}}`)))
				if rec.Code != wantStatus {
					t.Fatalf("save status=%d want=%d body=%s", rec.Code, wantStatus, rec.Body.String())
				}
				if failure == "write failure" {
					path += ".original"
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != string(original) || !reflect.DeepEqual(h.cfg, before) || !reflect.DeepEqual(h.acceptedConfig, before) || h.reloadGeneration != 0 {
					t.Fatal("failed save published a config or changed its rollback baseline")
				}
			})
		}
	}
}
