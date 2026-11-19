package management

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Faults happen on real temporary files after opening and performing I/O.
type configFileFault struct {
	*os.File
	operation string
	err       error
	hit       bool
	after     func()
}

func (f *configFileFault) fail() error {
	f.hit = true
	if f.after != nil {
		f.after()
	}
	return f.err
}

func (f *configFileFault) Read(p []byte) (int, error) {
	if f.operation == "read" && !f.hit {
		n, err := f.File.Read(p[:1])
		return n, errors.Join(err, f.fail())
	}
	return f.File.Read(p)
}

func (f *configFileFault) WriteAt(p []byte, offset int64) (int, error) {
	if (f.operation == "short write" || f.operation == "partial write") && !f.hit {
		n, err := f.File.WriteAt(p[:len(p)/2], offset)
		fault := f.fail()
		if f.operation == "short write" {
			return n, err
		}
		return n, errors.Join(err, fault)
	}
	return f.File.WriteAt(p, offset)
}

func (f *configFileFault) Truncate(size int64) error {
	err := f.File.Truncate(size)
	if f.operation == "truncate" && !f.hit {
		return errors.Join(err, f.fail())
	}
	return err
}

func (f *configFileFault) Sync() error {
	err := f.File.Sync()
	if f.operation == "sync" && !f.hit {
		return errors.Join(err, f.fail())
	}
	return err
}

func (f *configFileFault) Close() error {
	err := f.File.Close()
	if f.operation == "close" && !f.hit {
		return errors.Join(err, f.fail())
	}
	return err
}

func injectConfigWriteFault(t *testing.T, path, operation, restoreOperation string) (*configFileFault, *configFileFault) {
	t.Helper()
	first := &configFileFault{operation: operation, err: errors.New("injected " + operation + " failure")}
	restore := &configFileFault{operation: restoreOperation, err: errors.New("injected restoration " + restoreOperation + " failure")}
	open := openConfigWriteFile
	t.Cleanup(func() { openConfigWriteFile = open })
	openConfigWriteFile = func(name string, flag int, perm os.FileMode) (configWriteFile, error) {
		if name != path {
			return open(name, flag, perm)
		}
		if first.File != nil && restoreOperation == "open" {
			return nil, restore.fail()
		}
		f, err := os.OpenFile(name, flag, perm)
		if err != nil {
			return nil, err
		}
		if first.File == nil {
			first.File = f
			return first, nil
		}
		restore.File = f
		return restore, nil
	}
	return first, restore
}

func TestConfigSaveRestoresPostOpenFailures(t *testing.T) {
	for _, endpoint := range []string{"WriteConfig", "/v0/management/config.yaml", "/v8/management/config.yaml", "/v8/management/config"} {
		for _, operation := range []string{"read", "short write", "partial write", "truncate", "sync", "close"} {
			t.Run(endpoint+"/"+operation, func(t *testing.T) {
				h, path, router := newFullDocumentHandler(t)
				h.SetConfigValidator(func(*config.Config) error { return nil })
				before := h.cfg.CloneForRuntime()
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				router.GET("/v8/management/config", h.ConfigV8)
				get := httptest.NewRecorder()
				router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v8/management/config", nil))
				etag := get.Header().Get("ETag")
				fault, _ := injectConfigWriteFault(t, path, operation, "")
				body := `{"server":{"port":8320}}`
				if endpoint == "WriteConfig" {
					err = WriteConfig(path, []byte("port: 8320\n"))
					want := fault.err
					if operation == "short write" {
						want = io.ErrShortWrite
					}
					if !errors.Is(err, want) {
						t.Errorf("save error = %v, want %v", err, want)
					}
				} else {
					if endpoint == "/v0/management/config.yaml" {
						body = "port: 8320\n"
					}
					rec := httptest.NewRecorder()
					router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, endpoint, strings.NewReader(body)))
					if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "write_failed") {
						t.Errorf("save status=%d body=%s", rec.Code, rec.Body.String())
					}
				}
				if !fault.hit {
					t.Fatal("save did not reach the post-open fault")
				}
				after, err := os.ReadFile(path)
				if err != nil || string(after) != string(original) {
					t.Errorf("failed save changed bytes: got %q, error=%v", after, err)
				}
				afterInfo, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(info, afterInfo) || info.Mode() != afterInfo.Mode() {
					t.Error("failed save changed inode or permissions")
				}
				if !reflect.DeepEqual(h.cfg, before) || !reflect.DeepEqual(h.acceptedConfig, before) || h.reloadGeneration != 0 {
					t.Error("failed save changed runtime, accepted snapshot, or reload generation")
				}
				get = httptest.NewRecorder()
				router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v8/management/config", nil))
				if get.Code != http.StatusOK || get.Header().Get("ETag") != etag {
					t.Error("failed save changed readable config or ETag")
				}
			})
		}
	}
}

func TestConfigSaveReportsRestorationFailures(t *testing.T) {
	for _, operation := range []string{"open", "short write", "partial write", "truncate", "sync", "close"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			fault, restore := injectConfigWriteFault(t, path, "sync", operation)
			err := WriteConfig(path, []byte("port: 8320\n"))
			want := restore.err
			if operation == "short write" {
				want = io.ErrShortWrite
			}
			if !fault.hit || !restore.hit || !errors.Is(err, fault.err) || !errors.Is(err, want) || !strings.Contains(err.Error(), "restore config failed; on-disk state is uncertain") {
				t.Fatalf("save did not report both failures and uncertainty: %v", err)
			}
		})
	}
}

func TestConfigSaveCleansUpFailedCreation(t *testing.T) {
	for _, operation := range []string{"short write", "partial write", "truncate", "sync", "close"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			fault, _ := injectConfigWriteFault(t, path, operation, "")
			if err := WriteConfig(path, []byte("port: 8320\n")); err == nil || !fault.hit {
				t.Fatalf("save did not reach the post-open fault: %v", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("failed creation left a config file")
			}
		})
	}
}

func TestConfigSaveHTTPReportsRestorationFailure(t *testing.T) {
	for _, url := range []string{"/v0/management/config.yaml", "/v8/management/config.yaml"} {
		t.Run(url, func(t *testing.T) {
			h, path, router := newFullDocumentHandler(t)
			h.SetConfigValidator(func(*config.Config) error { return nil })
			before := h.cfg.CloneForRuntime()
			fault, restore := injectConfigWriteFault(t, path, "partial write", "sync")
			body := "port: 8320\n"
			if url == "/v8/management/config.yaml" {
				body = "config-version: 8\nserver: {port: 8320}\n"
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, url, strings.NewReader(body)))
			if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), fault.err.Error()) || !strings.Contains(rec.Body.String(), restore.err.Error()) || !strings.Contains(rec.Body.String(), "on-disk state is uncertain") {
				t.Fatalf("response did not report restoration failure: status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !reflect.DeepEqual(h.cfg, before) || !reflect.DeepEqual(h.acceptedConfig, before) || h.reloadGeneration != 0 {
				t.Fatal("restoration failure published the rejected configuration")
			}
		})
	}
}

func TestConfigSaveDoesNotRestoreOverReplacedFile(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "created", true: "existing"}[exists], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if exists {
				if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			other := filepath.Join(filepath.Dir(path), "other.yaml")
			const concurrent = "port: 9000\n"
			if err := os.WriteFile(other, []byte(concurrent), 0o640); err != nil {
				t.Fatal(err)
			}
			fault, _ := injectConfigWriteFault(t, path, "close", "")
			fault.after = func() {
				if err := os.Rename(other, path); err != nil {
					t.Fatal(err)
				}
			}
			err := WriteConfig(path, []byte("port: 8320\n"))
			if !errors.Is(err, fault.err) || !strings.Contains(err.Error(), "on-disk state is uncertain") || !strings.Contains(err.Error(), "config file changed") {
				t.Fatalf("save did not report replaced file: %v", err)
			}
			saved, err := os.ReadFile(path)
			if err != nil || string(saved) != concurrent {
				t.Fatal("recovery damaged the replacement file")
			}
		})
	}
}
