package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
)

type recoveryTransport func(context.Context, *transport.Request) (transport.Session, error)

func (f recoveryTransport) Handshake(ctx context.Context, req *transport.Request) (transport.Session, error) {
	return f(ctx, req)
}

func TestGitRecoveryPreservesConfigSavedDuringClone(t *testing.T) {
	for _, suppliedBaseline := range []bool{false, true} {
		name := "inspect baseline"
		if suppliedBaseline {
			name = "pre-pull baseline"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "seed\n"})
			store := NewGitTokenStore(remote, "", "", "master")
			repoDir := filepath.Join(root, "workspace")
			store.SetBaseDir(filepath.Join(repoDir, "auths"))
			if err := store.EnsureRepository(); err != nil {
				t.Fatal(err)
			}
			const original, latest = "port: 8317\n", "port: 8321\n"
			if err := api.WriteConfig(store.ConfigPath(), []byte(original)); err != nil {
				t.Fatal(err)
			}
			if err := store.PersistConfig(context.Background()); err != nil {
				t.Fatal(err)
			}
			baselineRepo, baselineTree, dirtyPaths, err := inspectRecoveryBaseline(repoDir)
			if err != nil {
				t.Fatal(err)
			}
			if !suppliedBaseline {
				baselineTree, dirtyPaths = nil, nil
			}
			localTransport, err := client.New().Transport("file")
			if err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				entered, release := make(chan struct{}), make(chan struct{})
				opts := []client.Option{client.WithTransport("file", recoveryTransport(func(ctx context.Context, req *transport.Request) (transport.Session, error) {
					close(entered)
					<-release
					return localTransport.Handshake(ctx, req)
				}))}
				done := make(chan error, 1)
				go func() {
					store.mu.Lock()
					defer store.mu.Unlock()
					done <- store.recoverRepositoryLocked(repoDir, opts, baselineRepo, baselineTree, dirtyPaths, (*git.Repository).Close, os.Rename)
				}()
				<-entered
				saved := make(chan error, 1)
				go func() { saved <- api.WriteConfig(store.ConfigPath(), []byte(latest)) }()
				synctest.Wait()
				if len(saved) != 1 {
					t.Error("clone held the config file gate")
				}
				close(release)
				if err := <-saved; err != nil {
					t.Fatal(err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				assertLocalFileContents(t, store.ConfigPath(), latest)
				if err := store.PersistConfig(context.Background()); err != nil {
					t.Fatal(err)
				}
				assertRemoteFileContents(t, remote, "master", "config/config.yaml", latest)
			})
		})
	}
}

func TestGitRecoveryHoldsConfigGateThroughReplacementAndRollback(t *testing.T) {
	for _, failure := range []string{"none", "install", "restore", "close"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "seed\n"})
			store := NewGitTokenStore(remote, "", "", "master")
			repoDir := filepath.Join(root, "workspace")
			store.SetBaseDir(filepath.Join(repoDir, "auths"))
			if err := store.EnsureRepository(); err != nil {
				t.Fatal(err)
			}
			const original = "port: 8317\n"
			if err := api.WriteConfig(store.ConfigPath(), []byte(original)); err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				entered, release := make(chan struct{}), make(chan struct{})
				injected := errors.New("injected recovery " + failure + " failure")
				rename := func(source, target string) error {
					if filepath.Base(source) == "config" && strings.Contains(source, string(filepath.Separator)+"clone"+string(filepath.Separator)) {
						if failure != "close" && failure != "restore" {
							close(entered)
							<-release
						}
						if failure == "install" || failure == "restore" {
							return injected
						}
					}
					if failure == "restore" && filepath.Base(source) == "config" && strings.Contains(source, string(filepath.Separator)+"worktree"+string(filepath.Separator)) {
						close(entered)
						<-release
						return injected
					}
					return os.Rename(source, target)
				}
				closes := 0
				closeRepo := func(repo *git.Repository) error {
					closes++
					err := repo.Close()
					if failure == "close" && closes == 3 {
						close(entered)
						<-release
						return errors.Join(err, injected)
					}
					return err
				}
				done := make(chan error, 1)
				go func() {
					store.mu.Lock()
					defer store.mu.Unlock()
					done <- store.recoverRepositoryLocked(repoDir, nil, nil, nil, nil, closeRepo, rename)
				}()
				<-entered
				saved := make(chan error, 1)
				go func() { saved <- api.WriteConfig(filepath.Join(root, "other.yaml"), []byte("port: 8321\n")) }()
				synctest.Wait()
				if len(saved) != 0 {
					t.Error("config gate released before replacement/rollback finished")
				}
				close(release)
				err := <-done
				if failure == "none" && err != nil || failure != "none" && !errors.Is(err, injected) {
					t.Fatalf("recovery error: %v", err)
				}
				if err := <-saved; err != nil {
					t.Fatal(err)
				}
				backups, err := filepath.Glob(filepath.Join(root, ".gitstore-recovery-*"))
				if err != nil {
					t.Fatal(err)
				}
				if failure == "restore" {
					if len(backups) != 1 {
						t.Fatalf("retained backups = %v, want one", backups)
					}
					assertLocalFileContents(t, filepath.Join(backups[0], "worktree", "config", "config.yaml"), original)
				} else {
					if len(backups) != 0 {
						t.Errorf("unexpected retained backups: %v", backups)
					}
					assertLocalFileContents(t, store.ConfigPath(), original)
				}
			})
		})
	}
}

func TestGitRecoveryRefreshesConfigChangesAndConflicts(t *testing.T) {
	for _, change := range []string{"create", "delete", "revert", "remote", "conflict", "symlink", "directory"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "seed\n"})
			store := NewGitTokenStore(remote, "", "", "master")
			repoDir := filepath.Join(root, "workspace")
			store.SetBaseDir(filepath.Join(repoDir, "auths"))
			if err := store.EnsureRepository(); err != nil {
				t.Fatal(err)
			}
			const original, latest = "port: 8317\n", "port: 8321\n"
			path := store.ConfigPath()
			if change != "create" {
				if err := api.WriteConfig(path, []byte(original)); err != nil {
					t.Fatal(err)
				}
				if err := store.PersistConfig(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if change == "revert" {
				if err := api.WriteConfig(path, []byte("port: 8318\n")); err != nil {
					t.Fatal(err)
				}
			}
			baselineRepo, baselineTree, dirtyPaths, err := inspectRecoveryBaseline(repoDir)
			if err != nil {
				t.Fatal(err)
			}
			if change == "revert" || change == "remote" || change == "conflict" {
				owner := NewGitTokenStore(remote, "", "", "master")
				owner.SetBaseDir(filepath.Join(root, "owner", "auths"))
				if err := owner.EnsureRepository(); err != nil {
					t.Fatal(err)
				}
				if err := api.WriteConfig(owner.ConfigPath(), []byte(latest)); err != nil {
					t.Fatal(err)
				}
				if err := owner.PersistConfig(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			switch change {
			case "delete", "symlink", "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if change == "symlink" {
					target := filepath.Join(root, "target.yaml")
					if err := api.WriteConfig(target, []byte(latest)); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				} else if change == "directory" {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			default:
				contents := latest
				if change == "revert" {
					contents = original
				} else if change == "conflict" {
					contents = "port: 8318\n"
				}
				if err := api.WriteConfig(path, []byte(contents)); err != nil {
					t.Fatal(err)
				}
			}
			store.mu.Lock()
			err = store.recoverRepositoryLocked(repoDir, nil, baselineRepo, baselineTree, dirtyPaths, (*git.Repository).Close, os.Rename)
			store.mu.Unlock()
			switch change {
			case "conflict":
				if err == nil || !strings.Contains(err.Error(), "conflicts with local change config/config.yaml") {
					t.Fatalf("recovery conflict error = %v", err)
				}
				assertLocalFileContents(t, path, "port: 8318\n")
				assertRemoteFileContents(t, remote, "master", "config/config.yaml", latest)
			case "directory":
				if err == nil || !strings.Contains(err.Error(), "unsupported file mode") {
					t.Fatalf("recovery file mode error = %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if change == "delete" {
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("recovery lost config deletion: %v", err)
					}
				} else {
					assertLocalFileContents(t, path, latest)
					if change == "symlink" {
						if _, err := os.Readlink(path); err != nil {
							t.Fatalf("recovery lost config symlink: %v", err)
						}
					}
				}
			}
		})
	}
}
