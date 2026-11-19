package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestConfigSavesRecoverFromPartialWrite(t *testing.T) {
	const childEnv = "CLIPROXY_TEST_CONFIG_PARTIAL_WRITE"
	if os.Getenv(childEnv) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestConfigSavesRecoverFromPartialWrite$")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("partial write regression: %v\n%s", err, output)
		}
		return
	}
	gin.SetMode(gin.TestMode)
	signal.Ignore(syscall.SIGXFSZ)
	var originalLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &originalLimit); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"SDK existing", "SDK create", "SDK dangling link", "v0", "v8"} {
		t.Run(endpoint, func(t *testing.T) {
			t.Cleanup(func() {
				if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &originalLimit); err != nil {
					t.Error(err)
				}
			})
			limitWrites := func() {
				t.Helper()
				// The original fits; the replacement fails after 64 real bytes.
				if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 64, Max: originalLimit.Max}); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			const original = "host: 127.0.0.1\nport: 8317\n"
			body := "host: 127.0.0.1\nport: 8320\n# " + strings.Repeat("synthetic comment ", 16) + "\n"
			if endpoint == "SDK create" || endpoint == "SDK dangling link" {
				if endpoint == "SDK dangling link" {
					if err := os.Symlink("target.yaml", path); err != nil {
						t.Fatal(err)
					}
				}
				limitWrites()
				if err := api.WriteConfig(path, []byte(body)); !errors.Is(err, syscall.EFBIG) {
					t.Fatalf("save error = %v, want file-size failure", err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("failed creation left a partial configuration")
				}
				if endpoint == "SDK dangling link" {
					if target, err := os.Readlink(path); err != nil || target != "target.yaml" {
						t.Fatal("failed creation changed the dangling symlink")
					}
				}
				return
			}
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			h := api.NewHandler(cfg, path, nil)
			reloads := make(chan struct{}, 1)
			h.SetConfigReloadHook(func(context.Context, *config.Config) { reloads <- struct{}{} })
			router := gin.New()
			router.GET("/v8/management/config", h.ConfigV8)
			router.GET("/runtime", h.GetConfig)
			get := func(url string) *httptest.ResponseRecorder {
				t.Helper()
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s status = %d", url, rec.Code)
				}
				return rec
			}
			etag := get("/v8/management/config").Header().Get("ETag")
			runtime := get("/runtime").Body.String()
			if endpoint == "SDK existing" {
				limitWrites()
				if err := api.WriteConfig(path, []byte(body)); !errors.Is(err, syscall.EFBIG) {
					t.Fatalf("save error = %v, want file-size failure", err)
				}
			} else {
				validated := false
				h.SetConfigValidator(func(*config.Config) error {
					validated = true
					// v0 staging must finish before restricting the final write.
					limitWrites()
					return nil
				})
				url := "/" + endpoint + "/management/config.yaml"
				if endpoint == "v0" {
					router.PUT(url, h.PutConfigYAML)
				} else {
					router.PUT(url, h.ConfigV8)
					body = "config-version: 8\nserver:\n  host: 127.0.0.1\n  port: 8320\n# " + strings.Repeat("synthetic comment ", 16) + "\n"
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, url, strings.NewReader(body)))
				if !validated || rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "write_failed") {
					t.Fatalf("final write failure: validated=%t status=%d body=%s", validated, rec.Code, rec.Body.String())
				}
			}
			saved, err := os.ReadFile(path)
			if err != nil || string(saved) != original {
				t.Errorf("failed save changed original bytes: got %d bytes, error=%v", len(saved), err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Error("failed save changed inode or permissions")
			}
			if get("/v8/management/config").Header().Get("ETag") != etag {
				t.Error("failed save changed ETag")
			}
			if get("/runtime").Body.String() != runtime {
				t.Error("failed save changed runtime configuration")
			}
			select {
			case <-reloads:
				t.Error("failed save published a reload")
			default:
			}
		})
	}
}

func TestWriteConfigPreservesFileContracts(t *testing.T) {
	for _, kind := range []string{"file", "hard link", "symlink", "dangling symlink", "missing", "read-only directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "target.yaml")
			path := target
			var before os.FileInfo
			var reader *os.File
			if kind != "missing" && kind != "dangling symlink" {
				if err := os.WriteFile(target, []byte("# original comment\nhost: 127.0.0.1\nport: 8317\n"), 0o640); err != nil {
					t.Fatal(err)
				}
				var err error
				before, err = os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				reader, err = os.Open(target)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := reader.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if kind == "symlink" || kind == "dangling symlink" {
				path = filepath.Join(dir, "link.yaml")
				if err := os.Symlink("target.yaml", path); err != nil {
					t.Fatal(err)
				}
			} else if kind == "hard link" {
				path = filepath.Join(dir, "link.yaml")
				if err := os.Link(target, path); err != nil {
					t.Fatal(err)
				}
			} else if kind == "read-only directory" {
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(dir, 0o700); err != nil {
						t.Error(err)
					}
				})
			}
			for _, body := range []string{"port: 8320\n", "# retained comment\nhost: 127.0.0.1\nport: 8321\n"} {
				if err := api.WriteConfig(path, []byte(body)); err != nil {
					t.Fatal(err)
				}
				saved, err := os.ReadFile(target)
				if err != nil || string(saved) != body {
					t.Fatalf("saved bytes = %q, error=%v", saved, err)
				}
				after, err := os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				if before != nil && (!os.SameFile(before, after) || before.Mode() != after.Mode()) {
					t.Fatal("save replaced the original inode or permissions")
				}
				if reader != nil {
					buf := make([]byte, len(body))
					if _, err := reader.ReadAt(buf, 0); err != nil || string(buf) != body {
						t.Fatal("reader holding the original inode cannot see the saved bytes")
					}
				}
			}
			if kind == "symlink" || kind == "dangling symlink" {
				if link, err := os.Readlink(path); err != nil || link != "target.yaml" {
					t.Fatal("save replaced the symlink")
				}
			}
		})
	}
}
