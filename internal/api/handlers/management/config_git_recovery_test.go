package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
)

func TestConfigSaveGitRecoveryNeverPublishesFailedWrite(t *testing.T) {
	for _, operation := range []string{"sync", "close"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "workspace")
			path := filepath.Join(workspace, "config", "config.yaml")
			h, _, router := newFullDocumentHandler(t)
			h.configFilePath = path
			h.SetConfigValidator(func(*config.Config) error { return nil })
			queued := &queuedConfigMirror{authDir: filepath.Join(workspace, "auths")}
			previous := sdkauth.GetTokenStore()
			sdkauth.RegisterTokenStore(queued)
			t.Cleanup(func() { sdkauth.RegisterTokenStore(previous) })
			var observed []int
			w, err := watcher.NewWatcher(path, queued.AuthDir(), func(cfg *config.Config) {
				observed = append(observed, cfg.Port)
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := w.Stop(); err != nil {
					t.Error(err)
				}
			})
			synctest.Test(t, func(t *testing.T) {
				mirror, mirrored := newOfflineConfigMirror(t, "git", root, nil)
				queued.configMirror = mirror
				queued.start, queued.done = make(chan struct{}), make(chan error, 3)
				if err := WriteConfig(path, []byte("port: 8317\n")); err != nil {
					t.Fatal(err)
				}
				if err := mirror.PersistConfig(context.Background()); err != nil {
					t.Fatal(err)
				}
				forceConfigMirrorRecovery(t, workspace)
				save := func(body string) *httptest.ResponseRecorder {
					rec := httptest.NewRecorder()
					router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v8/management/config", strings.NewReader(body)))
					return rec
				}
				if rec := save(`{"server":{"port":8318}}`); rec.Code != http.StatusOK {
					t.Fatalf("initial save: %d %s", rec.Code, rec.Body.String())
				}
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				inode, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				w.ReloadConfigIfChanged()
				synctest.Wait()
				fault, _ := injectConfigWriteFault(t, path, operation, "")
				paused, release := make(chan struct{}), make(chan struct{})
				fault.after = func() { close(paused); <-release }
				saved := make(chan *httptest.ResponseRecorder, 1)
				go func() { saved <- save(`{"server":{"port":8320}}`) }()
				select {
				case <-paused:
				case rec := <-saved:
					t.Fatalf("save returned before fault: %d %s", rec.Code, rec.Body.String())
				}
				close(queued.start)
				synctest.Wait()
				// Recovery must neither capture failed bytes in its temporary trees nor
				// replace the inode that the paused save needs for restoration.
				current, err := os.Stat(path)
				if err != nil || !os.SameFile(inode, current) {
					t.Error("recovery replaced config before failed save restored it")
				}
				assertRecoveryTreesExcludeCandidate(t, root)
				close(release)
				rec := <-saved
				if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "write_failed") || strings.Contains(rec.Body.String(), "restore config failed") {
					t.Errorf("failed save: %d %s", rec.Code, rec.Body.String())
				}
				if err := <-queued.done; err != nil {
					t.Fatal(err)
				}
				if got, err := os.ReadFile(path); err != nil || string(got) != string(original) {
					t.Errorf("disk retained failed candidate: %q, %v", got, err)
				}
				if got := mirrored(); got != string(original) {
					t.Errorf("git mirror retained failed candidate: %q", got)
				}
				w.ReloadConfigIfChanged()
				synctest.Wait()
				for _, port := range observed {
					if port != 8318 {
						t.Errorf("watcher published failed config: %v", observed)
					}
				}
				if len(observed) == 0 || h.acceptedConfig.Port != 8318 {
					t.Error("runtime did not retain last accepted config")
				}
				if rec := save(`{"server":{"port":8321}}`); rec.Code != http.StatusOK {
					t.Fatalf("subsequent save: %d %s", rec.Code, rec.Body.String())
				}
				w.ReloadConfigIfChanged()
				synctest.Wait()
				if observed[len(observed)-1] != 8321 || h.acceptedConfig.Port != 8321 {
					t.Error("subsequent save did not reach runtime")
				}
				got, err := config.ParseConfigBytes([]byte(mirrored()))
				if err != nil || got.Port != 8321 {
					t.Errorf("subsequent save did not reach git mirror: %v, %v", got, err)
				}
				assertRecoveryTreesExcludeCandidate(t, root)
				for len(queued.done) > 0 {
					if err := <-queued.done; err != nil {
						t.Error(err)
					}
				}
			})
		})
	}
}

func forceConfigMirrorRecovery(t *testing.T, workspace string) {
	t.Helper()
	repo, err := git.PlainOpen(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	}()
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	const marker = "recovery-marker.txt"
	if err := os.WriteFile(filepath.Join(workspace, marker), []byte("recover this local repository\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := worktree.Add(marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Commit("Recovery fixture", &git.CommitOptions{Author: &object.Signature{
		Name: "Test", Email: "test@local", When: time.Unix(1711929600, 0),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(workspace, ".git", "objects", hash.String()[:2], hash.String()[2:])); err != nil {
		t.Fatal(err)
	}
}

func assertRecoveryTreesExcludeCandidate(t *testing.T, root string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, ".gitstore-recovery-*", "*", "config", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "8320") {
			t.Errorf("recovery tree retained failed candidate: %s", path)
		}
	}
}
