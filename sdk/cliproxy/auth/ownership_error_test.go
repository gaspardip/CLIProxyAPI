package auth

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type ownershipConflictExecutor struct {
	dispatchPolicyExecutor
	stage   string
	replace func()
}

func (e *ownershipConflictExecutor) ShouldPrepareRequestAuth(*Auth) bool { return e.stage == "prepare" }
func (e *ownershipConflictExecutor) PrepareRequestAuth(_ context.Context, a *Auth) (*Auth, error) {
	e.replace()
	a = a.Clone()
	a.Metadata["project_id"] = "old-project"
	return a, nil
}
func (e *ownershipConflictExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) {
	if e.stage == "refresh" || e.stage == "refresh-error" || e.stage == "refresh-cancel" {
		e.replace()
	}
	if e.stage == "refresh-cancel" {
		return nil, context.Canceled
	}
	if e.stage == "refresh-error" || e.stage == "result" {
		return nil, fmt.Errorf("fixture refresh failed")
	}
	a = a.Clone()
	a.Metadata["access_token"] = "renewed"
	return a, nil
}

func TestOwnershipConflictLeavesReplacementUnchanged(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream", "bootstrap"} {
		for _, stage := range []string{"prepare", "refresh", "before-refresh", "refresh-error", "refresh-cancel", "result"} {
			if mode == "bootstrap" && stage == "prepare" {
				continue
			}
			t.Run(fmt.Sprintf("%s/%s", mode, stage), func(t *testing.T) {
				ctx := context.Background()
				store := &requestPrepareStore{}
				cooldowns := &recordingCooldownStore{}
				m := NewManager(store, nil, nil)
				m.SetCooldownStateStore(cooldowns)
				base, err := m.Register(ctx, &Auth{ID: t.Name(), Provider: "antigravity", Metadata: map[string]any{"email": "original@example.com", "access_token": "unchanged-token", "refresh_token": "fixture-refresh"}})
				if err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(base.ID, base.Provider, []*registry.ModelInfo{{ID: "fixture-model"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(base.ID) })
				m.RefreshSchedulerEntry(base.ID)
				var before, disk *Auth
				var records []CooldownStateRecord
				var saves int32
				replace := func() {
					current, _ := m.GetByID(base.ID)
					current.Metadata["email"] = "replacement@example.com"
					if _, err := m.Update(ctx, current); err != nil {
						t.Fatal(err)
					}
					before, _ = m.GetByID(base.ID)
					if before.RejectedAccessToken != "" || before.RejectedSIWCAccessToken != "" {
						t.Fatal("replacement inherited old owner token rejection")
					}
					disk = store.lastAuth()
					records = cooldowns.getRecords()
					saves = store.saveCount.Load()
				}
				boundary := "401"
				if stage == "prepare" {
					boundary = "prepare"
				} else if mode == "bootstrap" {
					boundary = "bootstrap"
				}
				e := &ownershipConflictExecutor{dispatchPolicyExecutor: dispatchPolicyExecutor{boundary: boundary}, stage: stage, replace: replace}
				if stage == "before-refresh" {
					e.onBoundary = replace
				}
				if stage == "result" {
					m.SetResultPolicy(ResultPolicyFunc(func(_ context.Context, r Result) Result { replace(); return r }))
				}
				m.RegisterExecutor(e)
				req := exec.Request{Model: "fixture-model"}
				switch mode {
				case "execute":
					_, err = m.Execute(ctx, []string{base.Provider}, req, exec.Options{})
				case "count":
					_, err = m.ExecuteCount(ctx, []string{base.Provider}, req, exec.Options{})
				default:
					var result *exec.StreamResult
					result, err = m.ExecuteStream(ctx, []string{base.Provider}, req, exec.Options{})
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				}
				if err == nil {
					t.Error("ownership conflict did not decline request")
				}
				if before == nil {
					t.Fatal("replacement boundary not exercised")
				}
				after, _ := m.GetByID(base.ID)
				if !reflect.DeepEqual(before, after) {
					t.Error("old request changed replacement memory")
				}
				if !reflect.DeepEqual(disk, store.lastAuth()) || saves != store.saveCount.Load() {
					t.Error("old request saved replacement")
				}
				if !reflect.DeepEqual(records, cooldowns.getRecords()) {
					t.Error("old request persisted replacement cooldown")
				}
			})
		}
	}
}

type ownershipSaveStore struct {
	requestPrepareStore
	beforeSave func()
}

func (s *ownershipSaveStore) Save(ctx context.Context, a *Auth) (string, error) {
	if s.beforeSave != nil {
		s.beforeSave()
	}
	return s.requestPrepareStore.Save(ctx, a)
}

func TestReplacementSaveDoesNotAdoptOldOwnerRefreshFailure(t *testing.T) {
	ctx := context.Background()
	store := &ownershipSaveStore{}
	m := NewManager(store, nil, nil)
	m.RegisterExecutor(&unauthorizedRefreshExecutor{id: "antigravity", refreshErr: fmt.Errorf("fixture refresh failed")})
	base, err := m.Register(ctx, &Auth{ID: "fixture", Provider: "antigravity", Status: StatusActive, Metadata: map[string]any{"email": "old@example.com", "access_token": "unchanged-token", "refresh_token": "fixture-refresh"}})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	store.beforeSave = func() { close(entered); <-release }
	replacement := base.Clone()
	replacement.Metadata["email"] = "new@example.com"
	done := make(chan error, 1)
	go func() { _, err := m.Update(ctx, replacement); done <- err }()
	<-entered
	// Runtime failure recording must remain live while the independent save is blocked.
	_, refreshErr := m.ForceRefreshAuth(ctx, base.ID)
	close(release)
	if refreshErr == nil {
		t.Error("refresh failure not exercised")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, _ := m.GetByID(base.ID)
	for _, a := range []*Auth{current, store.lastAuth()} {
		if authMetadataString(a, "email") != "new@example.com" || a.LastError != nil || a.RefreshError != nil || a.Unavailable || a.Status != StatusActive {
			t.Error("replacement adopted old owner's runtime failure")
		}
	}
}

func TestReplacementClearsBothAccessTokenRejections(t *testing.T) {
	for _, email := range []string{"owner@example.com", "replacement@example.com"} {
		t.Run(email, func(t *testing.T) {
			ctx := context.Background()
			store := &requestPrepareStore{}
			m := NewManager(store, nil, nil)
			a, err := m.Register(ctx, &Auth{ID: "fixture", Provider: "codex", RejectedAccessToken: "outer", RejectedSIWCAccessToken: "inner", Metadata: map[string]any{
				"email": "owner@example.com", "access_token": "outer",
				"siwc": map[string]any{"access_token": "inner"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			a.Metadata["email"] = email
			if _, err := m.Update(ctx, a); err != nil {
				t.Fatal(err)
			}
			current, _ := m.GetByID(a.ID)
			for _, saved := range []*Auth{current, store.lastAuth()} {
				if email == "owner@example.com" {
					if saved.RejectedAccessToken != "outer" || saved.RejectedSIWCAccessToken != "inner" {
						t.Fatal("same owner lost token rejections")
					}
				} else if saved.RejectedAccessToken != "" || saved.RejectedSIWCAccessToken != "" {
					t.Fatal("replacement inherited token rejections")
				}
			}
		})
	}
}

func TestReplacementDispatchesAfterOldOwnerUnauthorized(t *testing.T) {
	for _, mode := range []string{"execute", "stream"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			m := NewManager(nil, nil, nil)
			e := &ownershipConflictExecutor{dispatchPolicyExecutor: dispatchPolicyExecutor{boundary: "prepare"}}
			m.RegisterExecutor(e)
			a, err := m.Register(ctx, &Auth{ID: t.Name(), Provider: "antigravity", Metadata: map[string]any{"email": "old@example.com", "access_token": "shared", "refresh_token": "old-refresh"}})
			if err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "fixture-model"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
			m.MarkResult(ctx, Result{AuthID: a.ID, Provider: a.Provider, Model: "fixture-model", AccessToken: "shared", Error: &Error{HTTPStatus: 401, Message: "fixture unauthorized"}})
			a, _ = m.GetByID(a.ID)
			if a.RejectedAccessToken == "" || !a.ModelStates["fixture-model"].Unavailable {
				t.Fatal("old owner's unauthorized result was not recorded")
			}
			a.Metadata["email"] = "new@example.com"
			a.Metadata["refresh_token"] = "new-refresh"
			if _, err := m.Update(ctx, a); err != nil {
				t.Fatal(err)
			}
			if mode == "execute" {
				_, err = m.Execute(ctx, []string{a.Provider}, exec.Request{Model: "fixture-model"}, exec.Options{})
			} else {
				var response *exec.StreamResult
				response, err = m.ExecuteStream(ctx, []string{a.Provider}, exec.Request{Model: "fixture-model"}, exec.Options{})
				if err == nil {
					for chunk := range response.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			}
			if err != nil {
				t.Fatalf("replacement could not dispatch: %v", err)
			}
			if len(e.calls) != 1 || authMetadataString(e.calls[0], "email") != "new@example.com" {
				t.Fatal("replacement did not reach executor")
			}
		})
	}
}
