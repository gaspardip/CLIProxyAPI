package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// revisionExecutor records every dispatched credential; hooks race other writers against it.
type revisionExecutor struct {
	mu            sync.Mutex
	calls         []*Auth
	shouldPrepare func(*Auth) bool
	prepare       func(*Auth) *Auth
	refresh       func(*Auth) (*Auth, error)
	execute       func(*Auth) error
}

func (*revisionExecutor) Identifier() string { return "antigravity" }
func (e *revisionExecutor) ShouldPrepareRequestAuth(a *Auth) bool {
	return e.shouldPrepare != nil && e.shouldPrepare(a)
}
func (e *revisionExecutor) PrepareRequestAuth(_ context.Context, a *Auth) (*Auth, error) {
	return e.prepare(a), nil
}
func (e *revisionExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) {
	if e.refresh == nil {
		return a, nil
	}
	return e.refresh(a)
}
func (e *revisionExecutor) Execute(_ context.Context, a *Auth, _ exec.Request, _ exec.Options) (exec.Response, error) {
	e.mu.Lock()
	e.calls = append(e.calls, a.Clone())
	e.mu.Unlock()
	if e.execute != nil {
		if err := e.execute(a); err != nil {
			return exec.Response{}, err
		}
	}
	return exec.Response{Payload: []byte(authAccessToken(a))}, nil
}
func (e *revisionExecutor) ExecuteStream(ctx context.Context, a *Auth, req exec.Request, opts exec.Options) (*exec.StreamResult, error) {
	response, err := e.Execute(ctx, a, req, opts)
	if err != nil {
		return nil, err
	}
	chunks := make(chan exec.StreamChunk, 1)
	chunks <- exec.StreamChunk{Payload: response.Payload}
	close(chunks)
	return &exec.StreamResult{Chunks: chunks}, nil
}
func (e *revisionExecutor) CountTokens(ctx context.Context, a *Auth, req exec.Request, opts exec.Options) (exec.Response, error) {
	return e.Execute(ctx, a, req, opts)
}
func (*revisionExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("unused")
}
func (e *revisionExecutor) dispatched() []*Auth {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*Auth(nil), e.calls...)
}

func registerRevisionFixture(t *testing.T, m *Manager, metadata map[string]any) *Auth {
	t.Helper()
	a, err := m.Register(context.Background(), &Auth{ID: t.Name(), Provider: "antigravity", Status: StatusActive, Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "fixture-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	m.RefreshSchedulerEntry(a.ID)
	return a
}

func executeRevisionFixture(m *Manager, mode string) error {
	ctx := context.Background()
	req := exec.Request{Model: "fixture-model"}
	if mode == "execute" {
		_, err := m.Execute(ctx, []string{"antigravity"}, req, exec.Options{})
		return err
	}
	result, err := m.ExecuteStream(ctx, []string{"antigravity"}, req, exec.Options{})
	if err != nil {
		return err
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			err = chunk.Err
		}
	}
	return err
}

// Finding 1: a refresh snapshotted before profile discovery must not pair its token with the
// profile preparation discovered meanwhile, whether the refresh resolved another account or none.
func TestRefreshCannotPairForeignTokenWithDiscoveredProfile(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		initial                    map[string]any
		refreshAccount, refreshOrg string
	}{
		{name: "other-account", initial: map[string]any{}, refreshAccount: "account-b", refreshOrg: "org-b"},
		{name: "failed-profile", initial: map[string]any{}},
		{name: "failed-profile-cleared-base", initial: map[string]any{"account_uuid": "", "organization_uuid": ""}},
	} {
		for _, mode := range []string{"execute", "stream"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				m := NewManager(nil, nil, nil)
				entered, release := make(chan struct{}), make(chan struct{})
				e := &revisionExecutor{
					shouldPrepare: func(a *Auth) bool {
						return authMetadataString(a, "account_uuid") == "" && authAccessToken(a) == "token-a"
					},
					prepare: func(a *Auth) *Auth {
						a.Metadata["account_uuid"] = "account-a"
						a.Metadata["organization_uuid"] = "org-a"
						return a
					},
					refresh: func(a *Auth) (*Auth, error) {
						close(entered)
						<-release
						a.Metadata["access_token"] = "token-b"
						a.Metadata["account_uuid"] = tc.refreshAccount
						a.Metadata["organization_uuid"] = tc.refreshOrg
						return a, nil
					},
				}
				m.RegisterExecutor(e)
				metadata := map[string]any{"access_token": "token-a", "refresh_token": "fixture-refresh"}
				for k, v := range tc.initial {
					metadata[k] = v
				}
				a := registerRevisionFixture(t, m, metadata)
				refreshed := make(chan struct{})
				go func() { _, _ = m.ForceRefreshAuth(ctx, a.ID); close(refreshed) }()
				<-entered
				if _, err := m.PrepareRequestAuth(ctx, e, a); err != nil {
					t.Fatal(err)
				}
				close(release)
				<-refreshed
				_ = executeRevisionFixture(m, mode)
				live, _ := m.GetByID(a.ID)
				for _, view := range append(e.dispatched(), live) {
					if authAccessToken(view) == "token-b" && (authMetadataString(view, "account_uuid") != tc.refreshAccount || authMetadataString(view, "organization_uuid") != tc.refreshOrg) {
						t.Fatalf("token-b paired with account %q org %q, want %q %q", authMetadataString(view, "account_uuid"), authMetadataString(view, "organization_uuid"), tc.refreshAccount, tc.refreshOrg)
					}
				}
			})
		}
	}
}

type replacingFilterPolicy struct {
	mu       sync.Mutex
	onFilter func()
}

func (p *replacingFilterPolicy) Pick(_ context.Context, _, _ string, _ exec.Options, auths []*Auth) (*Auth, error) {
	return auths[0], nil
}
func (p *replacingFilterPolicy) FilterCandidates(_ context.Context, _, _ string, _ exec.Options, auths []*Auth) ([]*Auth, error) {
	p.mu.Lock()
	f := p.onFilter
	p.onFilter = nil
	p.mu.Unlock()
	if f != nil {
		f()
	}
	return auths, nil
}

// Finding 2: a replacement that lands while the dispatch policy runs must not let the
// old credential's token be sent; selection retries on the current credential.
func TestDispatchPolicyReplacementDuringFilterIsNotDispatched(t *testing.T) {
	for _, mode := range []string{"execute", "stream"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			policy := &replacingFilterPolicy{}
			m := NewManager(nil, policy, nil)
			e := &revisionExecutor{}
			m.RegisterExecutor(e)
			a := registerRevisionFixture(t, m, map[string]any{"email": "a@example.com", "access_token": "token-a"})
			policy.onFilter = func() {
				replacement := &Auth{ID: a.ID, Provider: a.Provider, Status: StatusActive, Metadata: map[string]any{"email": "b@example.com", "access_token": "token-b"}}
				if _, err := m.Update(ctx, replacement); err != nil {
					t.Error(err)
				}
			}
			err := executeRevisionFixture(m, mode)
			calls := e.dispatched()
			for _, call := range calls {
				if authAccessToken(call) == "token-a" {
					t.Fatal("replaced credential's token was dispatched")
				}
			}
			if err != nil || len(calls) != 1 || authAccessToken(calls[0]) != "token-b" {
				t.Fatalf("selection did not retry on the current credential: err=%v calls=%d", err, len(calls))
			}
		})
	}
}

// Finding 3: a credential-wide cooldown belongs to the account that earned it.
func TestReplacementForAnotherAccountDoesNotInheritCredentialCooldown(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, mode := range []string{"execute", "stream"} {
		for _, sameAccount := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/same-account=%t", mode, sameAccount), func(t *testing.T) {
				ctx := context.Background()
				m := NewManager(nil, nil, nil)
				e := &revisionExecutor{}
				m.RegisterExecutor(e)
				a := registerRevisionFixture(t, m, map[string]any{"email": "a@example.com", "access_token": "token-a"})
				hour := time.Hour
				m.MarkResult(ctx, Result{AuthID: a.ID, Provider: a.Provider, Model: "fixture-model", CredentialScope: true, RetryAfter: &hour, AccessToken: "token-a", CredentialVersion: a.CredentialVersion, RegistrationEpoch: a.RegistrationEpoch, Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "fixture quota"}})
				if cooled, _ := m.GetByID(a.ID); !cooled.Quota.Exceeded {
					t.Fatal("credential cooldown was not recorded")
				}
				email := "b@example.com"
				if sameAccount {
					email = "a@example.com"
				}
				if _, err := m.Update(ctx, &Auth{ID: a.ID, Provider: a.Provider, Status: StatusActive, Metadata: map[string]any{"email": email, "access_token": "token-b"}}); err != nil {
					t.Fatal(err)
				}
				err := executeRevisionFixture(m, mode)
				if sameAccount {
					if err == nil || len(e.dispatched()) != 0 {
						t.Fatal("token rotation dropped the account's cooldown")
					}
					return
				}
				if err != nil || len(e.dispatched()) != 1 {
					t.Fatalf("clean replacement inherited the old account's cooldown: %v", err)
				}
			})
		}
	}
}

// Finding 4: a canceled refresh must not reschedule a newer credential revision.
func TestCanceledRefreshDoesNotRescheduleRotatedCredential(t *testing.T) {
	for _, mode := range []string{"execute", "stream"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			m := NewManager(nil, nil, nil)
			var rotated *Auth
			e := &revisionExecutor{
				execute: func(a *Auth) error {
					if authAccessToken(a) == "token-1" {
						return &Error{HTTPStatus: http.StatusUnauthorized, Message: "fixture unauthorized"}
					}
					return nil
				},
			}
			e.refresh = func(a *Auth) (*Auth, error) {
				current, _ := m.GetByID(a.ID)
				current.Metadata["access_token"] = "token-2"
				if _, err := m.Update(ctx, current); err != nil {
					t.Error(err)
				}
				rotated, _ = m.GetByID(a.ID)
				return nil, context.Canceled
			}
			m.RegisterExecutor(e)
			a := registerRevisionFixture(t, m, map[string]any{"email": "same@example.com", "access_token": "token-1", "refresh_token": "fixture-refresh"})
			_ = executeRevisionFixture(m, mode)
			if rotated == nil {
				t.Fatal("refresh boundary not exercised")
			}
			live, _ := m.GetByID(a.ID)
			if !live.NextRefreshAfter.Equal(rotated.NextRefreshAfter) {
				t.Fatalf("canceled refresh rescheduled the rotated credential: %v -> %v", rotated.NextRefreshAfter, live.NextRefreshAfter)
			}
		})
	}
}

// revisionDenyingPolicy admits only the live revision, as the gateway's shared-route policy does.
type revisionDenyingPolicy struct {
	m        *Manager
	mu       sync.Mutex
	onFilter func()
	denyErr  error // returned when every candidate is excluded, as the gateway's quota errors are
}

func (p *revisionDenyingPolicy) Pick(_ context.Context, _, _ string, _ exec.Options, auths []*Auth) (*Auth, error) {
	return auths[0], nil
}
func (p *revisionDenyingPolicy) FilterCandidates(_ context.Context, _, _ string, _ exec.Options, auths []*Auth) ([]*Auth, error) {
	p.mu.Lock()
	f := p.onFilter
	p.onFilter = nil
	p.mu.Unlock()
	if f != nil {
		f()
	}
	var kept []*Auth
	for _, a := range auths {
		if live, ok := p.m.GetByID(a.ID); ok && sameRevision(a, live) {
			kept = append(kept, a)
		}
	}
	if len(kept) == 0 && p.denyErr != nil {
		return nil, p.denyErr
	}
	return kept, nil
}

// A policy that denies the superseded snapshot sends selection back to the live credential.
func TestDispatchPolicyDenialOfSupersededSnapshotReselects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		denyErr error
	}{{name: "empty"}, {name: "quota-error", denyErr: errors.New("no eligible quota")}} {
		for _, mode := range []string{"execute", "stream"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				policy := &revisionDenyingPolicy{denyErr: tc.denyErr}
				m := NewManager(nil, policy, nil)
				policy.m = m
				e := &revisionExecutor{}
				m.RegisterExecutor(e)
				a := registerRevisionFixture(t, m, map[string]any{"email": "a@example.com", "access_token": "token-a"})
				policy.onFilter = func() {
					replacement := &Auth{ID: a.ID, Provider: a.Provider, Status: StatusActive, Metadata: map[string]any{"email": "a@example.com", "access_token": "token-a2"}}
					if _, err := m.Update(ctx, replacement); err != nil {
						t.Error(err)
					}
				}
				err := executeRevisionFixture(m, mode)
				calls := e.dispatched()
				if err != nil || len(calls) != 1 || authAccessToken(calls[0]) != "token-a2" {
					t.Fatalf("denied superseded snapshot did not reselect the live credential: err=%v calls=%d", err, len(calls))
				}
			})
		}
	}
}
