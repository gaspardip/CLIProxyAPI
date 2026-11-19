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
	gitindex "github.com/go-git/go-git/v6/plumbing/format/index"
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

func TestEnsureRepositoryPreservesIgnoredActiveConfig(t *testing.T) {
	for _, layout := range []struct {
		name   string
		ignore string
		target string
		links  [][2]string
	}{
		{name: "file", ignore: "config/*.yaml\n", target: "config/config.yaml"},
		{name: "ancestor", ignore: "config/\n", target: "config/config.yaml"},
		{name: "file link", ignore: "config/\nprivate/\n", target: "private/settings.yaml", links: [][2]string{{"config/config.yaml", "../private/settings.yaml"}}},
		{name: "directory link", ignore: "config\nprivate/\n", target: "private/config.yaml", links: [][2]string{{"config", "private"}}},
		{name: "link chain", ignore: "config\nprivate/\n", target: "private/settings/active.yaml", links: [][2]string{{"config", "private/alias"}, {"private/alias", "settings"}, {"private/settings/config.yaml", "active.yaml"}}},
		{name: "absolute link", ignore: "config\nprivate/\n", target: "private/config.yaml", links: [][2]string{{"config", "$repo/private"}}},
		{name: "absolute root link", ignore: "config\n*.yaml\n", target: "config.yaml", links: [][2]string{{"config", "$repo"}}},
		{name: "external link", ignore: "config/\n", target: "../external.yaml", links: [][2]string{{"config/config.yaml", "../../external.yaml"}}},
		{name: "external file returns", ignore: "config/\nprivate/\n", target: "private/settings.yaml", links: [][2]string{{"config/config.yaml", "../../external.yaml"}, {"../external.yaml", "workspace/private/settings.yaml"}}},
		{name: "external directory returns", ignore: "config/\nprivate/\n", target: "private/config.yaml", links: [][2]string{{"config/config.yaml", "../../external/config.yaml"}, {"../external", "$repo/private"}}},
		{name: "absolute external returns via intermediate", ignore: "config/\nprivate/\n", target: "private/settings.yaml", links: [][2]string{{"config/config.yaml", "$repo/../external.yaml"}, {"../external.yaml", "$repo/private/alias.yaml"}, {"private/alias.yaml", "settings.yaml"}}},
		{name: "parent after directory link", ignore: "config/\nprivate/\n", target: "private/settings.yaml", links: [][2]string{{"config/config.yaml", "../../external/../settings.yaml"}, {"../external", "$repo/private/nested"}}},
		{name: "backslash uses OS semantics", ignore: "config/\nprivate*\n*.yaml\n", target: "private\\settings.yaml", links: [][2]string{{"config/config.yaml", "../private\\settings.yaml"}}},
	} {
		t.Run(layout.name, func(t *testing.T) {
			root := t.TempDir()
			remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "seed\n"})
			store := NewGitTokenStore(remote, "", "", "master")
			repoDir := filepath.Join(root, "workspace")
			store.SetBaseDir(filepath.Join(repoDir, "auths"))
			if err := store.EnsureRepository(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte(layout.ignore+"unrelated/\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			commitRecoveryFixture(t, repoDir)
			if err := os.Remove(filepath.Join(repoDir, "config")); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(repoDir, layout.target)
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := api.WriteConfig(target, []byte("port: 8317\n")); err != nil {
				t.Fatal(err)
			}
			if layout.name == "parent after directory link" {
				if err := os.Mkdir(filepath.Join(repoDir, "private", "nested"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for _, link := range layout.links {
				path := filepath.Join(repoDir, link[0])
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(strings.ReplaceAll(link[1], "$repo", repoDir), path); err != nil {
					t.Fatal(err)
				}
			}
			const latest = "# successful local save\r\nport: 8321\r\n"
			if err := api.WriteConfig(store.ConfigPath(), []byte(latest)); err != nil {
				t.Fatal(err)
			}
			ignored := []string{"unrelated/notes.yaml", filepath.Join(filepath.Dir(layout.target), "other.yaml")}
			for _, path := range ignored {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(repoDir, path)), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := api.WriteConfig(filepath.Join(repoDir, path), []byte("port: 9000\n")); err != nil {
					t.Fatal(err)
				}
			}
			removeHeadFileObject(t, repoDir, "corrupt-object.txt")
			if err := store.EnsureRepository(); err != nil {
				t.Fatal(err)
			}
			assertLocalFileContents(t, store.ConfigPath(), latest)
			assertLocalFileContents(t, target, latest)
			for _, link := range layout.links {
				want := strings.ReplaceAll(link[1], "$repo", repoDir)
				if got, err := os.Readlink(filepath.Join(repoDir, link[0])); err != nil || got != want {
					t.Fatalf("link %s = %q, %v; want %q", link[0], got, err, want)
				}
			}
			for _, path := range ignored {
				if strings.HasPrefix(path, "..") {
					assertLocalFileContents(t, filepath.Join(repoDir, path), "port: 9000\n")
					continue
				}
				if _, err := os.Lstat(filepath.Join(repoDir, path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unrelated ignored path %s survived recovery: %v", path, err)
				}
			}
			repo, err := git.PlainOpen(repoDir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := repo.Close(); err != nil {
					t.Error(err)
				}
			}()
			index, err := repo.Storer.Index()
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range index.Entries {
				if entry.Name != ".gitignore" && entry.Name != "branch.txt" {
					t.Errorf("recovery staged local path %s", entry.Name)
				}
			}
			assertRemoteTreePath(t, remote, "master", "config/config.yaml", false)
			if !strings.HasPrefix(layout.target, "..") {
				assertRemoteTreePath(t, remote, "master", layout.target, false)
			}
		})
	}
}

func TestPersistConfigRespectsIgnorePolicy(t *testing.T) {
	for _, recovery := range []string{"none", "persist", "ensure"} {
		for _, policy := range []struct {
			name, ignore, nested, exclude string
			tracked, persist              bool
		}{
			{name: "nonignored", persist: true},
			{name: "file", ignore: "config/*.yaml\n"},
			{name: "directory", ignore: "config/\n"},
			{name: "negated file", ignore: "config/*.yaml\n!config/config.yaml\n", persist: true},
			{name: "excluded parent wins", ignore: "config/\n!config/config.yaml\n"},
			{name: "negated parent", ignore: "config/\n!config/\nconfig/*\n!config/config.yaml\n", persist: true},
			{name: "nested", nested: "*.yaml\n"},
			{name: "nested negation", ignore: "*.yaml\n", nested: "!config.yaml\n", persist: true},
			{name: "local exclude", exclude: "config/config.yaml\n"},
			{name: "local exclude overridden", exclude: "config/config.yaml\n", ignore: "!config/config.yaml\n", persist: true},
			{name: "tracked ignored", ignore: "config/\n", tracked: true, persist: true},
			{name: "tracked locally excluded", exclude: "config/config.yaml\n", tracked: true, persist: true},
		} {
			name := policy.name + "/" + recovery
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "seed\n"})
				store := NewGitTokenStore(remote, "", "", "master")
				repoDir := filepath.Join(root, "workspace")
				store.SetBaseDir(filepath.Join(repoDir, "auths"))
				if err := store.EnsureRepository(); err != nil {
					t.Fatal(err)
				}
				if policy.tracked {
					if err := api.WriteConfig(store.ConfigPath(), []byte("port: 8317\n")); err != nil {
						t.Fatal(err)
					}
					commitRecoveryFixture(t, repoDir)
				}
				for path, contents := range map[string]string{".gitignore": policy.ignore, "config/.gitignore": policy.nested} {
					if err := os.WriteFile(filepath.Join(repoDir, path), []byte(contents), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				commitRecoveryFixture(t, repoDir)
				if err := os.MkdirAll(filepath.Join(repoDir, ".git", "info"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repoDir, ".git", "info", "exclude"), []byte(policy.exclude), 0o600); err != nil {
					t.Fatal(err)
				}
				marker := filepath.Join(repoDir, ".git", "unrelated-metadata")
				if err := os.WriteFile(marker, []byte("do not preserve\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				const contents = "# accepted local config\r\nport: 8321\r\n"
				if err := api.WriteConfig(store.ConfigPath(), []byte(contents)); err != nil {
					t.Fatal(err)
				}
				if recovery != "none" {
					removeHeadFileObject(t, repoDir, "corrupt-object.txt")
				}
				if recovery == "ensure" {
					if err := store.EnsureRepository(); err != nil {
						t.Fatal(err)
					}
				}
				// This is the real watcher persister, including recovery, staging and push.
				if err := store.PersistConfig(context.Background()); err != nil {
					t.Fatal(err)
				}
				assertLocalFileContents(t, store.ConfigPath(), contents)
				assertConfigPublication(t, repoDir, remote, policy.persist, contents)
				if recovery != "none" {
					if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("recovery copied unrelated Git metadata: %v", err)
					}
				}
				for _, reopen := range []bool{false, true} {
					if reopen {
						store = NewGitTokenStore(remote, "", "", "master")
						store.SetBaseDir(filepath.Join(repoDir, "auths"))
					}
					updated := contents + "# next watcher save\n"
					if reopen {
						updated += "# reopened store\n"
					}
					if err := api.WriteConfig(store.ConfigPath(), []byte(updated)); err != nil {
						t.Fatal(err)
					}
					if err := store.PersistConfig(context.Background()); err != nil {
						t.Fatal(err)
					}
					assertLocalFileContents(t, store.ConfigPath(), updated)
					assertConfigPublication(t, repoDir, remote, policy.persist, updated)
				}
				assertLocalFileContents(t, filepath.Join(repoDir, ".git", "info", "exclude"), policy.exclude)
			})
		}
	}
}

func assertConfigPublication(t *testing.T, repoDir, remote string, tracked bool, contents string) {
	t.Helper()
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := verifyRepositoryHead(repo); err != nil {
		t.Fatalf("persistence left repository corrupt: %v", err)
	}
	index, err := repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	_, err = index.Entry("config/config.yaml")
	if tracked && err != nil || !tracked && !errors.Is(err, gitindex.ErrEntryNotFound) {
		t.Errorf("config index visibility = %v, want tracked %v", err, tracked)
	}
	for _, entry := range index.Entries {
		if strings.HasPrefix(entry.Name, ".git/") {
			t.Errorf("staged local metadata: %s", entry.Name)
		}
	}
	assertRemoteTreePath(t, remote, "master", "config/config.yaml", tracked)
	assertRemoteTreePath(t, remote, "master", ".git/info/exclude", false)
	if tracked {
		assertRemoteFileContents(t, remote, "master", "config/config.yaml", contents)
	}
}

func TestPersistConfigHonorsLocalExcludeEditsAfterRecovery(t *testing.T) {
	for _, edit := range []string{"remove", "empty", "negate"} {
		t.Run(edit, func(t *testing.T) {
			root := t.TempDir()
			remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "seed\n"})
			repoDir := filepath.Join(root, "workspace")
			store := NewGitTokenStore(remote, "", "", "master")
			store.SetBaseDir(filepath.Join(repoDir, "auths"))
			if err := store.EnsureRepository(); err != nil {
				t.Fatal(err)
			}
			exclude := filepath.Join(repoDir, ".git", "info", "exclude")
			if err := os.MkdirAll(filepath.Dir(exclude), 0o700); err != nil {
				t.Fatal(err)
			}
			const rules = "# operator rules\r\nconfig/config.yaml\r\n"
			if err := os.WriteFile(exclude, []byte(rules), 0o600); err != nil {
				t.Fatal(err)
			}
			const contents = "port: 8321\n"
			if err := api.WriteConfig(store.ConfigPath(), []byte(contents)); err != nil {
				t.Fatal(err)
			}
			removeHeadFileObject(t, repoDir, "corrupt-object.txt")
			if err := store.EnsureRepository(); err != nil {
				t.Fatal(err)
			}
			assertLocalFileContents(t, exclude, rules)
			if err := store.PersistConfig(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertConfigPublication(t, repoDir, remote, false, contents)
			var edited string
			if edit == "remove" {
				if err := os.Remove(exclude); err != nil {
					t.Fatal(err)
				}
			} else {
				if edit == "negate" {
					edited = rules + "!config/config.yaml\r\n"
				}
				if err := os.WriteFile(exclude, []byte(edited), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			store = NewGitTokenStore(remote, "", "", "master")
			store.SetBaseDir(filepath.Join(repoDir, "auths"))
			removeHeadFileObject(t, repoDir, "corrupt-object.txt")
			if err := store.PersistConfig(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertConfigPublication(t, repoDir, remote, true, contents)
			if edit == "remove" {
				if _, err := os.Lstat(exclude); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("recovery restored deleted exclude: %v", err)
				}
			} else {
				assertLocalFileContents(t, exclude, edited)
			}
		})
	}
}

func TestPersistConfigRepairsMissingIndexBeforeIgnoreCheck(t *testing.T) {
	root := t.TempDir()
	remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "seed\n"})
	repoDir := filepath.Join(root, "workspace")
	store := NewGitTokenStore(remote, "", "", "master")
	store.SetBaseDir(filepath.Join(repoDir, "auths"))
	if err := store.EnsureRepository(); err != nil {
		t.Fatal(err)
	}
	if err := api.WriteConfig(store.ConfigPath(), []byte("port: 8317\n")); err != nil {
		t.Fatal(err)
	}
	commitRecoveryFixture(t, repoDir)
	if err := os.MkdirAll(filepath.Join(repoDir, ".git", "info"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".git", "info", "exclude"), []byte("config/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repoDir, ".git", "index")); err != nil {
		t.Fatal(err)
	}
	const contents = "port: 8321\n"
	if err := api.WriteConfig(store.ConfigPath(), []byte(contents)); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertConfigPublication(t, repoDir, remote, true, contents)
}

func TestEnsureRepositoryRejectsUnsafeLocalExclude(t *testing.T) {
	for _, layout := range []string{"git link", "info link", "exclude link", "dangling link", "info file", "exclude directory", "unreadable exclude"} {
		t.Run(layout, func(t *testing.T) {
			root := t.TempDir()
			remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "seed\n"})
			repoDir := filepath.Join(root, "workspace")
			store := NewGitTokenStore(remote, "", "", "master")
			store.SetBaseDir(filepath.Join(repoDir, "auths"))
			if err := store.EnsureRepository(); err != nil {
				t.Fatal(err)
			}
			removeHeadFileObject(t, repoDir, "corrupt-object.txt")
			infoDir := filepath.Join(repoDir, ".git", "info")
			if err := os.MkdirAll(infoDir, 0o700); err != nil {
				t.Fatal(err)
			}
			exclude := filepath.Join(infoDir, "exclude")
			const rules = "config/config.yaml\n"
			if err := os.WriteFile(exclude, []byte(rules), 0o600); err != nil {
				t.Fatal(err)
			}
			path := exclude
			switch layout {
			case "git link":
				path = filepath.Dir(infoDir)
			case "info link", "info file":
				path = infoDir
			}
			external := filepath.Join(root, "original-metadata")
			if err := os.Rename(path, external); err != nil {
				t.Fatal(err)
			}
			switch layout {
			case "info file":
				if err := os.WriteFile(path, []byte("not a directory\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "exclude directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "unreadable exclude":
				if err := os.WriteFile(path, []byte(rules), 0); err != nil {
					t.Fatal(err)
				}
			default:
				target := external
				if layout == "dangling link" {
					target += "-missing"
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			err = store.EnsureRepository()
			if err == nil || layout != "git link" && !strings.Contains(err.Error(), "preserve local exclude") {
				t.Fatalf("unsafe exclude recovery = %v, want preservation error", err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("rejected metadata changed: %v", err)
			}
			switch layout {
			case "git link":
				external = filepath.Join(external, "info", "exclude")
			case "info link", "info file":
				external = filepath.Join(external, "exclude")
			}
			assertLocalFileContents(t, external, rules)
			assertRemoteTreePath(t, remote, "master", "config/config.yaml", false)
		})
	}
}

func TestEnsureRepositoryIgnoredConfigLinkWithoutTarget(t *testing.T) {
	for _, target := range []string{"missing.yaml", "config.yaml", "../private/file/../settings.yaml", "../private/file/"} {
		t.Run(target, func(t *testing.T) {
			root := t.TempDir()
			remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "seed\n"})
			store := NewGitTokenStore(remote, "", "", "master")
			repoDir := filepath.Join(root, "workspace")
			store.SetBaseDir(filepath.Join(repoDir, "auths"))
			if err := store.EnsureRepository(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte("config/\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			commitRecoveryFixture(t, repoDir)
			if err := os.Mkdir(filepath.Join(repoDir, "private"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"file", "settings.yaml"} {
				if err := os.WriteFile(filepath.Join(repoDir, "private", name), []byte("harmless fixture\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, store.ConfigPath()); err != nil {
				t.Fatal(err)
			}
			if err := store.PersistConfig(context.Background()); err != nil {
				t.Fatalf("persist ignored config link before corruption: %v", err)
			}
			assertConfigPublication(t, repoDir, remote, false, "")
			removeHeadFileObject(t, repoDir, "corrupt-object.txt")
			err := store.EnsureRepository()
			if target == "config.yaml" {
				if err == nil || !strings.Contains(err.Error(), "too many config symlinks") {
					t.Fatalf("cyclic config link recovery error = %v", err)
				}
				assertLocalFileContents(t, filepath.Join(repoDir, "corrupt-object.txt"), "corrupt me\n")
			} else if strings.HasPrefix(target, "../private/file/") {
				if err == nil || !strings.Contains(err.Error(), "not a directory") {
					t.Fatalf("non-directory path component recovery error = %v", err)
				}
				assertLocalFileContents(t, filepath.Join(repoDir, "corrupt-object.txt"), "corrupt me\n")
				assertLocalFileContents(t, filepath.Join(repoDir, "private", "file"), "harmless fixture\n")
				assertLocalFileContents(t, filepath.Join(repoDir, "private", "settings.yaml"), "harmless fixture\n")
			} else if err != nil {
				t.Fatal(err)
			}
			if got, err := os.Readlink(store.ConfigPath()); err != nil || got != target {
				t.Fatalf("config link = %q, %v; want %q", got, err, target)
			}
		})
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
	for _, fixture := range []string{"regular", "file link", "directory link", "ignored regular", "ignored file link", "ignored directory link"} {
		ignored := strings.HasPrefix(fixture, "ignored ")
		layout := strings.TrimPrefix(fixture, "ignored ")
		for _, suppliedBaseline := range []bool{false, true} {
			name := "inspect baseline"
			if suppliedBaseline {
				name = "pre-pull baseline"
			}
			t.Run(fixture+"/"+name, func(t *testing.T) {
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
				if ignored {
					if err := os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte("config\nsettings*\n"), 0o600); err != nil {
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
					if ignored {
						assertRemoteTreePath(t, remote, "master", remotePath, false)
						return
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
			exclude := filepath.Join(repoDir, ".git", "info", "exclude")
			if err := os.MkdirAll(filepath.Dir(exclude), 0o700); err != nil {
				t.Fatal(err)
			}
			const rules = "# local policy\r\nconfig/config.yaml\r\n"
			if err := os.WriteFile(exclude, []byte(rules), 0o600); err != nil {
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
				assertLocalFileContents(t, exclude, rules)
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
					reopened := NewGitTokenStore(remote, "", "", "master")
					reopened.SetBaseDir(filepath.Join(repoDir, "auths"))
					if err := reopened.PersistConfig(context.Background()); err != nil {
						t.Fatal(err)
					}
					assertConfigPublication(t, repoDir, remote, false, original)
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
