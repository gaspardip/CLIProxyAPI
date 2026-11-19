package management

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/go-git/go-git/v6"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/minio/minio-go/v7"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type configMirror interface {
	coreauth.Store
	PersistConfig(context.Context) error
	PersistAuthFiles(context.Context, string, ...string) error
	ConfigPath() string
	AuthDir() string
}

// Delay the watcher job, not the adapter's file read, to reproduce a successful
// reload whose persistence starts during the next save's Sync or Close.
type queuedConfigMirror struct {
	configMirror
	start   chan struct{}
	done    chan error
	authDir string
}

func (s *queuedConfigMirror) AuthDir() string { return s.authDir }

func (s *queuedConfigMirror) PersistConfig(ctx context.Context) error {
	<-s.start
	err := s.configMirror.PersistConfig(ctx)
	s.done <- err
	return err
}

func TestConfigSaveMirrorsNeverPublishFailedWrite(t *testing.T) {
	for _, adapter := range []string{"postgres", "object", "git"} {
		for _, operation := range []string{"sync", "close"} {
			t.Run(adapter+"/"+operation, func(t *testing.T) {
				root := t.TempDir()
				workspace := root
				if adapter == "git" {
					workspace = filepath.Join(root, "workspace")
				}
				path := filepath.Join(workspace, "config", "config.yaml")
				queued := &queuedConfigMirror{authDir: filepath.Join(workspace, "auths")}
				previous := sdkauth.GetTokenStore()
				sdkauth.RegisterTokenStore(queued)
				t.Cleanup(func() { sdkauth.RegisterTokenStore(previous) })
				w, err := watcher.NewWatcher(path, queued.AuthDir(), nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := w.Stop(); err != nil {
						t.Error(err)
					}
				})
				synctest.Test(t, func(t *testing.T) {
					mirror, observed := newOfflineConfigMirror(t, adapter, root, nil)
					queued.configMirror = mirror
					queued.start, queued.done = make(chan struct{}), make(chan error, 1)
					const original = "# accepted config\nport: 8317\n"
					if err := WriteConfig(path, []byte(original)); err != nil {
						t.Fatal(err)
					}
					w.ReloadConfigIfChanged()
					synctest.Wait()
					fault, _ := injectConfigWriteFault(t, path, operation, "")
					paused, release := make(chan struct{}), make(chan struct{})
					fault.after = func() { close(paused); <-release }
					saved := make(chan error, 1)
					go func() { saved <- WriteConfig(path, []byte("port: 8320\n")) }()
					select {
					case <-paused:
					case err := <-saved:
						t.Fatalf("save returned before injected fault: %v", err)
					}
					if data, err := os.ReadFile(path); err != nil || string(data) != "port: 8320\n" {
						t.Errorf("save did not expose candidate at %s: %q, %v", operation, data, err)
					}
					close(queued.start)
					synctest.Wait()
					select {
					case err := <-queued.done:
						t.Errorf("mirror finished before save restored: %v", err)
						queued.done <- err
					default:
					}
					close(release)
					if err := <-saved; !errors.Is(err, fault.err) {
						t.Errorf("save error = %v, want injected failure", err)
					}
					if err := <-queued.done; err != nil {
						t.Fatal(err)
					}
					if got := observed(); got != original {
						t.Errorf("mirror received failed candidate: %q, want %q", got, original)
					}
					for _, external := range []bool{false, true} {
						want := "port: 8321\n"
						if external {
							want = "# external edit\nport: 8322\n"
							if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
								t.Fatal(err)
							}
						} else if err := WriteConfig(path, []byte(want)); err != nil {
							t.Fatal(err)
						}
						w.ReloadConfigIfChanged()
						if err := <-queued.done; err != nil {
							t.Fatal(err)
						}
						if got := observed(); got != want {
							t.Errorf("later config did not reach mirror: %q, want %q", got, want)
						}
					}
				})
			})
		}
	}
}

// These adapters run their real persistence code: PostgreSQL uses pgx over a
// net.Pipe, S3 uses an in-memory HTTP transport, and git pushes to a local bare repo.
func newOfflineConfigMirror(t *testing.T, adapter, root string, beforeWrite func()) (configMirror, func() string) {
	t.Helper()
	var content string
	record := func(data []byte) {
		if beforeWrite != nil {
			beforeWrite()
		}
		content = string(data)
	}
	switch adapter {
	case "postgres":
		cfg, err := pgx.ParseConfig("postgres://test@127.0.0.1/test?sslmode=disable")
		if err != nil {
			t.Fatal(err)
		}
		cfg.DefaultQueryExecMode = pgx.QueryExecModeExec
		cfg.DialFunc = func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			go func() {
				defer func() {
					if err := server.Close(); err != nil {
						t.Error(err)
					}
				}()
				if err := serveConfigMirrorPostgres(server, record); err != nil && !errors.Is(err, io.EOF) {
					t.Error(err)
				}
			}()
			return client, nil
		}
		dsn := stdlib.RegisterConnConfig(cfg)
		t.Cleanup(func() { stdlib.UnregisterConnConfig(dsn) })
		mirror, err := store.NewPostgresStore(context.Background(), store.PostgresStoreConfig{DSN: dsn, SpoolDir: root})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := mirror.Close(); err != nil {
				t.Error(err)
			}
		})
		return mirror, func() string { return content }
	case "object":
		original := minio.DefaultTransport
		t.Cleanup(func() { minio.DefaultTransport = original })
		minio.DefaultTransport = func(bool) (*http.Transport, error) {
			transport := &http.Transport{}
			transport.RegisterProtocol("https", configMirrorTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodPut || req.URL.Path != "/bucket/config/config.yaml" {
					return nil, fmt.Errorf("unexpected mirror request: %s %s", req.Method, req.URL.Path)
				}
				data, err := io.ReadAll(req.Body)
				if err = errors.Join(err, req.Body.Close()); err != nil {
					return nil, err
				}
				record(data)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			}))
			return transport, nil
		}
		mirror, err := store.NewObjectTokenStore(store.ObjectStoreConfig{
			Endpoint: "mirror.invalid", Bucket: "bucket", Region: "us-east-1", AccessKey: "test", SecretKey: "test",
			LocalRoot: root, UseSSL: true, PathStyle: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return mirror, func() string { return content }
	case "git":
		remote := filepath.Join(root, "remote.git")
		repo, err := git.PlainInit(remote, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Close(); err != nil {
			t.Fatal(err)
		}
		mirror := store.NewGitTokenStore(remote, "", "", "master")
		mirror.SetBaseDir(filepath.Join(root, "workspace", "auths"))
		if err := mirror.EnsureRepository(); err != nil {
			t.Fatal(err)
		}
		return mirror, func() string {
			repo, err := git.PlainOpen(remote)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := repo.Close(); err != nil {
					t.Error(err)
				}
			}()
			head, err := repo.Head()
			if err != nil {
				t.Fatal(err)
			}
			commit, err := repo.CommitObject(head.Hash())
			if err != nil {
				t.Fatal(err)
			}
			file, err := commit.File("config/config.yaml")
			if err != nil {
				t.Fatal(err)
			}
			data, err := file.Contents()
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
	default:
		t.Fatalf("unknown mirror %s", adapter)
		return nil, nil
	}
}

type configMirrorTransport func(*http.Request) (*http.Response, error)

func (f configMirrorTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func serveConfigMirrorPostgres(conn net.Conn, record func([]byte)) error {
	backend := pgproto3.NewBackend(conn, conn)
	if _, err := backend.ReceiveStartupMessage(); err != nil {
		return err
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := backend.Flush(); err != nil {
		return err
	}
	var parameters [][]byte
	for {
		message, err := backend.Receive()
		if err != nil {
			return err
		}
		switch msg := message.(type) {
		case *pgproto3.Query: // pgx connection ping
			backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		case *pgproto3.Parse:
			if !strings.Contains(msg.Query, "INSERT INTO") {
				return fmt.Errorf("unexpected mirror SQL: %s", msg.Query)
			}
			backend.Send(&pgproto3.ParseComplete{})
		case *pgproto3.Bind:
			parameters = msg.Parameters
			backend.Send(&pgproto3.BindComplete{})
		case *pgproto3.Describe:
			backend.Send(&pgproto3.NoData{})
		case *pgproto3.Execute:
			if len(parameters) != 2 || string(parameters[0]) != "config" {
				return errors.New("unexpected mirror SQL parameters")
			}
			record(parameters[1])
			backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")})
		case *pgproto3.Sync:
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		case *pgproto3.Terminate:
			return nil
		default:
			return fmt.Errorf("unexpected PostgreSQL message %T", msg)
		}
		// pgx pipelines extended messages; flushing before Sync can deadlock net.Pipe.
		switch message.(type) {
		case *pgproto3.Sync, *pgproto3.Query:
			if err := backend.Flush(); err != nil {
				return err
			}
		}
	}
}

func TestConfigMirrorsReleaseFileGateBeforeRemoteWork(t *testing.T) {
	for _, adapter := range []string{"postgres", "object"} {
		t.Run(adapter, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				entered, release := make(chan struct{}), make(chan struct{})
				first := true
				mirror, observed := newOfflineConfigMirror(t, adapter, t.TempDir(), func() {
					if first {
						first = false
						close(entered)
						<-release
					}
				})
				path := mirror.ConfigPath()
				if err := WriteConfig(path, []byte("port: 8317\n")); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 2)
				go func() { done <- mirror.PersistConfig(context.Background()) }()
				<-entered
				saved := false
				go func() {
					if err := WriteConfig(path, []byte("port: 8321\n")); err != nil {
						t.Error(err)
					}
					saved = true
				}()
				synctest.Wait()
				if !saved {
					t.Error("stalled remote work blocked a local save")
				}
				go func() { done <- mirror.PersistConfig(context.Background()) }()
				close(release)
				for range 2 {
					if err := <-done; err != nil {
						t.Error(err)
					}
				}
				if got := observed(); got != "port: 8321\n" {
					t.Errorf("older persistence overwrote later save: %q", got)
				}
			})
		})
	}
}
