package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

func TestWriteConfigRejectsAmbiguousOAuthChannels(t *testing.T) {
	for _, field := range []string{"model-alias", "excluded-models", "settings", "request-scoped-errors"} {
		for _, layout := range []string{"legacy", "v8"} {
			for _, exists := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/existing=%t", field, layout, exists), func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "config.yaml")
					const original = "port: 8317\n"
					if exists {
						if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					body := fmt.Sprintf("oauth-%s: {codex: [], ' CODEX ': []}\n", field)
					if layout == "v8" {
						body = fmt.Sprintf("config-version: 8\noauth:\n  %s: {codex: [], ' CODEX ': []}\n", field)
					}
					if _, err := config.ParseConfigBytes([]byte(body)); err == nil {
						t.Fatal("fixture must be rejected by the public config parser")
					}
					err := api.WriteConfig(path, []byte(body))
					if err == nil || !strings.Contains(err.Error(), "normalize to") {
						t.Errorf("WriteConfig error = %v, want ambiguous channel rejection", err)
					}
					saved, errRead := os.ReadFile(path)
					if exists {
						if errRead != nil || string(saved) != original {
							t.Fatal("rejected save changed the existing document")
						}
					} else if !os.IsNotExist(errRead) {
						t.Fatal("rejected save created a document")
					}
				})
			}
		}
	}
}

func TestConfigV8SecretWritesKeepSnapshotAndETag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct{ method, url, body string }{
		{http.MethodPut, "/v8/management/config/management/secret-key", `"public-api-fixture"`},
		{http.MethodPatch, "/v8/management/config", `{"management":{"secret-key":"public-api-fixture"}}`},
	} {
		t.Run(tc.method, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			h := api.NewHandler(cfg, path, nil)
			var validated *config.Config
			h.SetConfigValidator(func(next *config.Config) error {
				validated = next.CloneForRuntime()
				return nil
			})
			reloads := make(chan *config.Config, 2)
			h.SetConfigReloadHook(func(_ context.Context, next *config.Config) { reloads <- next })
			router := gin.New()
			router.Handle(tc.method, "/v8/management/config", h.ConfigV8)
			router.Handle(tc.method, "/v8/management/config/*path", h.ConfigV8)
			router.GET("/v8/management/config", h.ConfigV8)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.url, strings.NewReader(tc.body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("save status = %d, want 200", rec.Code)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var disk config.Config
			if err := yaml.Unmarshal(saved, &disk); err != nil {
				t.Fatal(err)
			}
			if err := bcrypt.CompareHashAndPassword([]byte(disk.RemoteManagement.SecretKey), []byte("public-api-fixture")); err != nil {
				t.Fatal("save did not persist a hash of the fixture")
			}
			if validated == nil || disk.RemoteManagement.SecretKey != validated.RemoteManagement.SecretKey {
				t.Fatal("persisted hash differs from the validated snapshot")
			}
			select {
			case snapshot := <-reloads:
				if snapshot.RemoteManagement.SecretKey != disk.RemoteManagement.SecretKey || snapshot.SourceHash != validated.SourceHash {
					t.Fatal("reload received a different finalized snapshot")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("save did not publish a reload snapshot")
			}
			loaded, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(saved) {
				t.Fatal("loading a saved configuration rewrote the document")
			}
			if loaded.SourceHash != validated.SourceHash || rec.Header().Get("ETag") != `"`+loaded.SourceHash+`"` {
				t.Fatal("disk, validated snapshot, and write ETag disagree")
			}
			get := httptest.NewRecorder()
			router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v8/management/config", nil))
			if get.Code != http.StatusOK || get.Header().Get("ETag") != rec.Header().Get("ETag") {
				t.Fatal("ETag changed after loading the saved config")
			}
		})
	}
}

func TestConfigSavesPreserveBlockScalarContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scalar := range []struct{ name, yaml, want string }{
		{"literal", "|-\n  # literal content\n", "# literal content"},
		{"folded", ">-\n  first line\n  # folded content\n", "first line # folded content"},
		{"explicit indent", "|2+\n    # indented content\n\n", "  # indented content\n\n"},
	} {
		for _, endpoint := range []string{"SDK", "v0", "v8"} {
			t.Run(scalar.name+"/"+endpoint, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				// The management hash rewrite and layout migration both re-encode YAML.
				body := "remote-management: {secret-key: block-fixture}\nproxy-url: " + scalar.yaml
				if endpoint == "v8" {
					body = "config-version: 8\nmanagement: {secret-key: block-fixture}\nrequests:\n  proxy-url: " + strings.ReplaceAll(strings.TrimSuffix(scalar.yaml, "\n"), "\n", "\n  ") + "\n"
				}
				if endpoint == "SDK" {
					if err := api.WriteConfig(path, []byte(body)); err != nil {
						t.Fatal(err)
					}
				} else {
					cfg, err := config.LoadConfig(path)
					if err != nil {
						t.Fatal(err)
					}
					h := api.NewHandler(cfg, path, nil)
					h.SetConfigValidator(func(next *config.Config) error {
						if next.ProxyURL != scalar.want {
							t.Error("application validation received changed block scalar content")
						}
						return nil
					})
					router := gin.New()
					url := "/" + endpoint + "/management/config.yaml"
					if endpoint == "v0" {
						router.PUT(url, h.PutConfigYAML)
					} else {
						router.PUT(url, h.ConfigV8)
					}
					rec := httptest.NewRecorder()
					router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, url, strings.NewReader(body)))
					if rec.Code != http.StatusOK {
						t.Fatalf("save status = %d, want 200", rec.Code)
					}
				}
				saved, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var persisted config.Config
				if err := yaml.Unmarshal(saved, &persisted); err != nil {
					t.Fatal(err)
				}
				if persisted.ProxyURL != scalar.want {
					t.Errorf("saved scalar = %q, want %q", persisted.ProxyURL, scalar.want)
				}
			})
		}
	}
}

func TestConfigSavesValidateSubmittedBlockScalars(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"SDK", "v0", "v8"} {
		t.Run(endpoint, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			const original = "host: 127.0.0.1\nport: 8317\n"
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			body := "trusted-proxies:\n  - |-\n    127.0.0.1\n    # part of the submitted value\n"
			if endpoint == "v8" {
				body = "config-version: 8\nserver:\n  trusted-proxies:\n    - |-\n      127.0.0.1\n      # part of the submitted value\n"
			}
			if _, err := config.ParseConfigBytes([]byte(body)); err == nil {
				t.Fatal("fixture must be rejected by the public config parser")
			}
			if endpoint == "SDK" {
				if err := api.WriteConfig(path, []byte(body)); err == nil {
					t.Error("WriteConfig accepted an invalid block scalar")
				}
			} else {
				cfg, err := config.LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				h := api.NewHandler(cfg, path, nil)
				h.SetConfigValidator(func(*config.Config) error {
					t.Error("invalid submitted config reached application validation")
					return nil
				})
				router := gin.New()
				url := "/" + endpoint + "/management/config.yaml"
				if endpoint == "v0" {
					router.PUT(url, h.PutConfigYAML)
				} else {
					router.PUT(url, h.ConfigV8)
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, url, strings.NewReader(body)))
				if rec.Code != http.StatusUnprocessableEntity {
					t.Errorf("save status = %d, want 422", rec.Code)
				}
			}
			saved, err := os.ReadFile(path)
			if err != nil || string(saved) != original {
				t.Fatal("invalid submitted config changed disk")
			}
		})
	}
}

func TestWriteConfigPreservesScalarContent(t *testing.T) {
	for _, tc := range []struct{ name, scalar, want string }{
		{"literal", "|-\n  first\n  # scalar data\n", "first\n# scalar data"},
		{"folded", ">-\n  first\n  # scalar data\n", "first # scalar data"},
		{"explicit indent", "|2-\n  # scalar data\n", "# scalar data"},
		{"single quoted", "'first\n  # scalar data'\n", "first # scalar data"},
		{"double quoted", "\"first\n  # scalar data\"\n", "first # scalar data"},
	} {
		for _, v8 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/v8=%t", tc.name, v8), func(t *testing.T) {
				body := "# operator comment\nhost: " + tc.scalar + "port: 8317\n"
				if v8 {
					body = "config-version: 8\nserver:\n  " + strings.ReplaceAll(body, "\n", "\n  ")
				}
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := api.WriteConfig(path, []byte(body)); err != nil {
					t.Fatal(err)
				}
				loaded, err := config.LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				if loaded.Host != tc.want {
					t.Fatalf("saved scalar = %q, want %q", loaded.Host, tc.want)
				}
			})
		}
	}
}

func TestWriteConfigPreservesDistinctStringMapKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "claude-api-key:\n  - api-key: map-key-fixture\n    headers:\n      0x1: |-\n        first\n        # scalar data\n      1: stable\n"
	if err := api.WriteConfig(path, []byte(body)); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ClaudeKey) != 1 {
		t.Fatal("saved config lost the credential")
	}
	values := cfg.ClaudeKey[0].Headers
	if values["0x1"] != "first\n# scalar data" || values["1"] != "stable" {
		t.Fatalf("saved string map: %q", values)
	}
}
