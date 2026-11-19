package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
)

func commitRecoveryFixture(t *testing.T, dir string) {
	t.Helper()
	if err := disableGitCommitSigning(dir); err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainOpen(dir)
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
	if err := worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Commit("Recovery fixture", &git.CommitOptions{Author: &object.Signature{
		Name: "Test", Email: "test@local", When: time.Unix(1711929600, 0),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&git.PushOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestGitRecoveryPreservesLinkedConfigDirectory(t *testing.T) {
	for _, absolute := range []bool{false, true} {
		name := "relative"
		if absolute {
			name = "absolute"
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
			const contents = "port: 8321\n"
			if err := api.WriteConfig(store.ConfigPath(), []byte("port: 8317\n")); err != nil {
				t.Fatal(err)
			}
			commitRecoveryFixture(t, repoDir)
			if err := os.Rename(filepath.Join(repoDir, "config"), filepath.Join(repoDir, "settings")); err != nil {
				t.Fatal(err)
			}
			link := "settings"
			if absolute {
				link = filepath.Join(repoDir, link)
			}
			if err := os.Symlink(link, filepath.Join(repoDir, "config")); err != nil {
				t.Fatal(err)
			}
			if err := api.WriteConfig(store.ConfigPath(), []byte(contents)); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(store.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			checked := false
			err = store.recoverRepositoryLocked(repoDir, nil, nil, nil, nil, (*git.Repository).Close, func(source, target string) error {
				if !checked && filepath.Dir(source) == repoDir {
					checked = true
					assertLocalFileContents(t, store.ConfigPath(), contents)
					after, err := os.Stat(store.ConfigPath())
					if err != nil || !os.SameFile(before, after) {
						t.Fatalf("local target changed before swap: %v", err)
					}
				}
				return os.Rename(source, target)
			})
			store.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if got, err := os.Readlink(filepath.Join(repoDir, "config")); err != nil || got != link {
				t.Fatalf("config link = %q, %v; want %q", got, err, link)
			}
			assertLocalFileContents(t, store.ConfigPath(), contents)
			assertLocalFileContents(t, filepath.Join(repoDir, "settings", "config.yaml"), contents)
		})
	}
}

type recoveryTransport func(context.Context, *transport.Request) (transport.Session, error)

func (f recoveryTransport) Handshake(ctx context.Context, req *transport.Request) (transport.Session, error) {
	return f(ctx, req)
}

func TestGitRecoveryPreservesConfigSavedDuringClone(t *testing.T) {
	for _, layout := range []string{"regular", "file link", "directory link"} {
		for _, suppliedBaseline := range []bool{false, true} {
			name := "inspect baseline"
			if suppliedBaseline {
				name = "pre-pull baseline"
			}
			t.Run(layout+"/"+name, func(t *testing.T) {
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
				targetPath := store.ConfigPath()
				remotePath := "config/config.yaml"
				switch layout {
				case "file link":
					targetPath = filepath.Join(repoDir, "settings.yaml")
					remotePath = "settings.yaml"
					if err := os.Rename(store.ConfigPath(), targetPath); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("../settings.yaml", store.ConfigPath()); err != nil {
						t.Fatal(err)
					}
				case "directory link":
					targetPath = filepath.Join(repoDir, "settings", "config.yaml")
					remotePath = "settings/config.yaml"
					if err := os.Rename(filepath.Join(repoDir, "config"), filepath.Join(repoDir, "settings")); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("settings", filepath.Join(repoDir, "config")); err != nil {
						t.Fatal(err)
					}
				}
				commitRecoveryFixture(t, repoDir)
				if err := os.WriteFile(filepath.Join(repoDir, "local-note.txt"), []byte("keep local note\n"), 0o600); err != nil {
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
					} else {
						assertLocalFileContents(t, targetPath, latest)
					}
					close(release)
					if err := <-saved; err != nil {
						t.Fatal(err)
					}
					if err := <-done; err != nil {
						t.Fatal(err)
					}
					assertLocalFileContents(t, store.ConfigPath(), latest)
					assertLocalFileContents(t, targetPath, latest)
					assertLocalFileContents(t, filepath.Join(repoDir, "local-note.txt"), "keep local note\n")
					aliasInfo, err := os.Stat(store.ConfigPath())
					if err != nil {
						t.Fatal(err)
					}
					targetInfo, err := os.Stat(targetPath)
					if err != nil || !os.SameFile(aliasInfo, targetInfo) {
						t.Fatalf("saved config no longer names its tracked target: %v", err)
					}
					if layout == "regular" {
						if err := store.PersistConfig(context.Background()); err != nil {
							t.Fatal(err)
						}
					} else {
						commitRecoveryFixture(t, repoDir)
					}
					assertRemoteFileContents(t, remote, "master", remotePath, latest)
				})
			})
		}
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

func TestGitRecoveryRespectsCRLFNormalization(t *testing.T) {
	for _, autoCRLF := range []string{"true", "input", "false"} {
		t.Run(autoCRLF, func(t *testing.T) {
			root := t.TempDir()
			remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "seed\n"})
			store := NewGitTokenStore(remote, "", "", "master")
			repoDir := filepath.Join(root, "workspace")
			store.SetBaseDir(filepath.Join(repoDir, "auths"))
			if err := store.EnsureRepository(); err != nil {
				t.Fatal(err)
			}
			if err := api.WriteConfig(store.ConfigPath(), []byte("port: 8317\n")); err != nil {
				t.Fatal(err)
			}
			commitRecoveryFixture(t, repoDir)
			repo, err := git.PlainOpen(repoDir)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := repo.Config()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Core.AutoCRLF = autoCRLF
			if err := repo.SetConfig(cfg); err != nil {
				t.Fatal(err)
			}
			if err := api.WriteConfig(store.ConfigPath(), []byte("port: 8317\r\n")); err != nil {
				t.Fatal(err)
			}
			worktree, err := repo.Worktree()
			if err != nil {
				t.Fatal(err)
			}
			status, err := worktree.Status()
			if err != nil || status.IsClean() != (autoCRLF != "false") {
				t.Fatalf("initial CRLF status = %v, %v", status, err)
			}
			if err := repo.Close(); err != nil {
				t.Fatal(err)
			}
			owner := NewGitTokenStore(remote, "", "", "master")
			owner.SetBaseDir(filepath.Join(root, "owner", "auths"))
			if err := owner.EnsureRepository(); err != nil {
				t.Fatal(err)
			}
			if err := api.WriteConfig(owner.ConfigPath(), []byte("port: 8321\n")); err != nil {
				t.Fatal(err)
			}
			if err := owner.PersistConfig(context.Background()); err != nil {
				t.Fatal(err)
			}
			removeHeadFileObject(t, repoDir, "corrupt-object.txt")
			err = store.EnsureRepository()
			if autoCRLF == "false" {
				if err == nil || !strings.Contains(err.Error(), "conflicts with local change") {
					t.Fatalf("unnormalized CRLF conflict = %v", err)
				}
				assertLocalFileContents(t, store.ConfigPath(), "port: 8317\r\n")
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertLocalFileContents(t, store.ConfigPath(), "port: 8321\n")
			}
			assertRemoteFileContents(t, remote, "master", "config/config.yaml", "port: 8321\n")
		})
	}
}

func TestGitRecoveryCallersPreserveTrackedConfigTarget(t *testing.T) {
	for _, caller := range []string{"before pull", "repair index", "reconcile", "pull", "after pull"} {
		t.Run(caller, func(t *testing.T) {
			root := t.TempDir()
			remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "seed\n"})
			store := NewGitTokenStore(remote, "", "", "master")
			repoDir := filepath.Join(root, "workspace")
			store.SetBaseDir(filepath.Join(repoDir, "auths"))
			if err := store.EnsureRepository(); err != nil {
				t.Fatal(err)
			}
			targetPath := filepath.Join(repoDir, "settings.yaml")
			if err := api.WriteConfig(targetPath, []byte("port: 8317\n")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../settings.yaml", store.ConfigPath()); err != nil {
				t.Fatal(err)
			}
			commitRecoveryFixture(t, repoDir)
			repo, tree, _, err := inspectRecoveryBaseline(repoDir)
			if err != nil {
				t.Fatal(err)
			}
			entry, err := tree.FindEntry("settings.yaml")
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.Close(); err != nil {
				t.Fatal(err)
			}
			ownerDir := filepath.Join(root, "owner")
			owner := NewGitTokenStore(remote, "", "", "master")
			owner.SetBaseDir(filepath.Join(ownerDir, "auths"))
			if err := owner.EnsureRepository(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ownerDir, "branch.txt"), []byte("remote edit\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			commitRecoveryFixture(t, ownerDir)
			if err := os.WriteFile(filepath.Join(repoDir, "local-note.txt"), []byte("local edit\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			configPath := store.ConfigPath()
			saveAndCorrupt := func() {
				if err := api.WriteConfig(configPath, []byte("port: 8321\n")); err != nil {
					t.Fatal(err)
				}
				hash := entry.Hash.String()
				if err := os.Remove(filepath.Join(repoDir, ".git", "objects", hash[:2], hash[2:])); err != nil {
					t.Fatal(err)
				}
			}
			if caller == "before pull" {
				saveAndCorrupt()
			} else {
				store.pullFn = func(ctx context.Context, worktree *git.Worktree, opts *git.PullOptions) error {
					if caller == "pull" {
						if err := worktree.PullContext(ctx, opts); err != nil {
							return err
						}
					}
					saveAndCorrupt()
					brokenRepo, err := git.PlainOpen(repoDir)
					if err != nil {
						return err
					}
					ref := plumbing.NewBranchReferenceName("master")
					if caller == "reconcile" {
						ref = plumbing.NewRemoteReferenceName("origin", "master")
					}
					if caller != "pull" {
						err = brokenRepo.Storer.SetReference(plumbing.NewHashReference(ref, plumbing.NewHash(strings.Repeat("1", 40))))
					}
					if err := errors.Join(err, brokenRepo.Close()); err != nil {
						return err
					}
					switch caller {
					case "repair index":
						return git.NoErrAlreadyUpToDate
					case "reconcile":
						return git.ErrUnstagedChanges
					case "pull":
						return plumbing.ErrObjectNotFound
					default:
						return nil
					}
				}
			}
			if err := store.EnsureRepository(); err != nil {
				t.Fatal(err)
			}
			assertLocalFileContents(t, store.ConfigPath(), "port: 8321\n")
			assertLocalFileContents(t, targetPath, "port: 8321\n")
			assertLocalFileContents(t, filepath.Join(repoDir, "local-note.txt"), "local edit\n")
			assertLocalFileContents(t, filepath.Join(repoDir, "branch.txt"), "remote edit\n")
			if link, err := os.Readlink(store.ConfigPath()); err != nil || link != "../settings.yaml" {
				t.Fatalf("config link = %q, %v", link, err)
			}
			assertRemoteFileContents(t, remote, "master", "settings.yaml", "port: 8317\n")
		})
	}
}

func TestGitRecoveryRefreshesConfigChangesAndConflicts(t *testing.T) {
	for _, change := range []string{"create", "delete", "revert", "remote", "conflict", "pre-pull conflict", "partial pull", "ignored remote delete", "symlink", "directory"} {
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
			if change == "ignored remote delete" {
				if err := os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte("config/config.yaml\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				commitRecoveryFixture(t, repoDir)
			}
			if change == "pre-pull conflict" {
				if err := api.WriteConfig(path, []byte(latest)); err != nil {
					t.Fatal(err)
				}
			}
			if change == "partial pull" {
				removeHeadFileObject(t, repoDir, "corrupt-object.txt")
			}
			baselineRepo, baselineTree, dirtyPaths, err := inspectRecoveryBaseline(repoDir)
			if err != nil {
				t.Fatal(err)
			}
			if change == "revert" || change == "remote" || change == "conflict" || change == "pre-pull conflict" || change == "partial pull" || change == "ignored remote delete" {
				owner := NewGitTokenStore(remote, "", "", "master")
				owner.SetBaseDir(filepath.Join(root, "owner", "auths"))
				if err := owner.EnsureRepository(); err != nil {
					t.Fatal(err)
				}
				if change == "ignored remote delete" {
					if err := os.Remove(owner.ConfigPath()); err != nil {
						t.Fatal(err)
					}
					commitRecoveryFixture(t, filepath.Dir(owner.AuthDir()))
				} else {
					if err := api.WriteConfig(owner.ConfigPath(), []byte(latest)); err != nil {
						t.Fatal(err)
					}
					if err := owner.PersistConfig(context.Background()); err != nil {
						t.Fatal(err)
					}
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
			if change == "partial pull" {
				if err := baselineRepo.Fetch(&git.FetchOptions{RemoteName: "origin"}); err != nil {
					t.Fatal(err)
				}
				remoteRef, err := baselineRepo.Reference(plumbing.NewRemoteReferenceName("origin", "master"), true)
				if err != nil {
					t.Fatal(err)
				}
				if err := baselineRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("master"), remoteRef.Hash())); err != nil {
					t.Fatal(err)
				}
				worktree, err := baselineRepo.Worktree()
				if err != nil {
					t.Fatal(err)
				}
				if err := worktree.Reset(&git.ResetOptions{Mode: git.MixedReset}); err != nil {
					t.Fatal(err)
				}
			}
			store.mu.Lock()
			err = store.recoverRepositoryLocked(repoDir, nil, baselineRepo, baselineTree, dirtyPaths, (*git.Repository).Close, os.Rename)
			store.mu.Unlock()
			switch change {
			case "conflict", "pre-pull conflict", "ignored remote delete":
				if err == nil || !strings.Contains(err.Error(), "conflicts with local change config/config.yaml") {
					t.Fatalf("recovery conflict error = %v", err)
				}
				want := "port: 8318\n"
				if change == "pre-pull conflict" || change == "ignored remote delete" {
					want = latest
				}
				assertLocalFileContents(t, path, want)
				if change == "ignored remote delete" {
					assertRemoteTreePath(t, remote, "master", "config/config.yaml", false)
				} else {
					assertRemoteFileContents(t, remote, "master", "config/config.yaml", latest)
				}
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
