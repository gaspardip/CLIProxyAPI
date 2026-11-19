package config

import (
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
)

func TestConfigFileHelpersWaitForWrite(t *testing.T) {
	for name, operation := range map[string]func(string) error{
		"read":          func(path string) error { _, err := ReadConfigFile(path); return err },
		"load":          func(path string) error { _, err := LoadConfig(path); return err },
		"optional load": func(path string) error { _, err := LoadConfigOptional(path, true); return err },
		"staged load":   func(path string) error { _, err := LoadConfigForSave(path); return err },
		"save":          func(path string) error { return SaveConfigPreserveComments(path, &Config{Port: 8317, RequestRetry: 3}) },
		"scalar save": func(path string) error {
			return SaveConfigPreserveCommentsUpdateNestedScalar(path, []string{"host"}, "127.0.0.1")
		},
		"upload preparation": PrepareConfigFileForUpload,
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				const previous = "# accepted config\nport: 8317\nrequest-retry: 3\n"
				if err := os.WriteFile(path, []byte(previous), 0o600); err != nil {
					t.Fatal(err)
				}
				written, restore := make(chan struct{}), make(chan struct{})
				writeDone := make(chan error, 1)
				go func() {
					writeDone <- WithFileAccess(func() error {
						if err := os.WriteFile(path, []byte("port: 8320\nrequest-retry: 9\n"), 0o600); err != nil {
							return err
						}
						close(written)
						<-restore
						return os.WriteFile(path, []byte(previous), 0o600)
					})
				}()
				select {
				case <-written:
				case err := <-writeDone:
					t.Fatal(err)
				}
				done := false
				go func() {
					if err := operation(path); err != nil {
						t.Error(err)
					}
					done = true
				}()
				synctest.Wait()
				if done {
					t.Error("config helper accessed an uncommitted write")
				}
				close(restore)
				if err := <-writeDone; err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if !done {
					t.Fatal("config helper did not resume after restoration")
				}
				cfg, err := LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Port != 8317 || cfg.RequestRetry != 3 {
					t.Errorf("restored config changed: port=%d, request-retry=%d", cfg.Port, cfg.RequestRetry)
				}
			})
		})
	}
}
