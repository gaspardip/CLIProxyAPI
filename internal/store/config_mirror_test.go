package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/go-git/go-git/v6"
	"github.com/minio/minio-go/v7"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
)

func TestConfigMirrorsRestorePersistedBytesOnStartup(t *testing.T) {
	for _, adapter := range []string{"postgres", "object"} {
		t.Run(adapter, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			var persist, restore func(context.Context) error
			if adapter == "postgres" {
				store, _ := newSeedTestPostgresStore(t, path)
				persist = store.PersistConfig
				restore = func(ctx context.Context) error { return store.syncConfigFromDatabase(ctx, "") }
			} else {
				var content string
				client, err := minio.New("mirror.invalid", &minio.Options{
					Region: "us-east-1", Secure: true, BucketLookup: minio.BucketLookupPath,
					Transport: configMirrorRoundTripper(func(req *http.Request) (*http.Response, error) {
						if req.URL.Path != "/bucket/config/config.yaml" {
							return nil, fmt.Errorf("unexpected object %s", req.URL.Path)
						}
						switch req.Method {
						case http.MethodPut:
							data, err := io.ReadAll(req.Body)
							if err = errors.Join(err, req.Body.Close()); err != nil {
								return nil, err
							}
							content = string(data)
						case http.MethodHead, http.MethodGet:
						default:
							return nil, fmt.Errorf("unexpected object method %s", req.Method)
						}
						return &http.Response{StatusCode: http.StatusOK, Request: req,
							Header: http.Header{"Content-Length": {strconv.Itoa(len(content))}, "Last-Modified": {"Mon, 02 Jan 2006 15:04:05 GMT"}},
							Body:   io.NopCloser(strings.NewReader(content)), ContentLength: int64(len(content)),
						}, nil
					}),
				})
				if err != nil {
					t.Fatal(err)
				}
				store := &ObjectTokenStore{client: client, cfg: ObjectStoreConfig{Bucket: "bucket"}, configPath: path}
				persist = store.PersistConfig
				restore = func(ctx context.Context) error { return store.syncConfigFromBucket(ctx, "") }
			}
			for _, want := range []string{"# restored config\nport: 8317\n", "# later save\nport: 8321\n"} {
				if err := api.WriteConfig(path, []byte(want)); err != nil {
					t.Fatal(err)
				}
				if err := persist(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("port: 9999\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := restore(context.Background()); err != nil {
					t.Fatal(err)
				}
				if got := readSeedTestFile(t, path); string(got) != want {
					t.Fatalf("startup restored %q, want %q", got, want)
				}
			}
		})
	}
}

type configMirrorRoundTripper func(*http.Request) (*http.Response, error)

func (f configMirrorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestGitConfigMirrorKeepsStagedSnapshotWhilePushStalls(t *testing.T) {
	root := t.TempDir()
	remote := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "seed\n"})
	store := NewGitTokenStore(remote, "", "", "master")
	store.SetBaseDir(filepath.Join(root, "workspace", "auths"))
	if err := store.EnsureRepository(); err != nil {
		t.Fatal(err)
	}
	const original = "# accepted config\nport: 8317\n"
	const next = "# later save\nport: 8321\n"
	if err := api.WriteConfig(store.ConfigPath(), []byte(original)); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		first := true
		store.pushFn = func(ctx context.Context, repo *git.Repository, opts *git.PushOptions) error {
			if first {
				first = false
				close(entered)
				<-release
			}
			return repo.PushContext(ctx, opts)
		}
		done := make(chan error, 1)
		go func() { done <- store.PersistConfig(context.Background()) }()
		<-entered
		saved := false
		go func() {
			if err := api.WriteConfig(store.ConfigPath(), []byte(next)); err != nil {
				t.Error(err)
			}
			saved = true
		}()
		synctest.Wait()
		if !saved {
			t.Error("stalled git push blocked a local save")
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		assertRemoteFileContents(t, remote, "master", "config/config.yaml", original)
		if local, err := os.ReadFile(store.ConfigPath()); err != nil || string(local) != next {
			t.Fatalf("mirror changed later local save: %q, %v", local, err)
		}
		if err := store.PersistConfig(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertRemoteFileContents(t, remote, "master", "config/config.yaml", next)
	})
	// Startup restores from the actual mirror, with no dependency on the old spool.
	restarted := NewGitTokenStore(remote, "", "", "master")
	restarted.SetBaseDir(filepath.Join(root, "restart", "auths"))
	if err := restarted.EnsureRepository(); err != nil {
		t.Fatal(err)
	}
	if restored, err := os.ReadFile(restarted.ConfigPath()); err != nil || string(restored) != next {
		t.Fatalf("startup restored %q, %v; want latest committed config", restored, err)
	}
}
