package auth

import (
	"context"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"reflect"
	"testing"
)

func TestPreparedUpdateCannotMutateReplacement(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("refresh=%t", refresh), func(t *testing.T) {
			for _, kind := range []string{"provider", "token", "account", "account_uuid", "organization_uuid", "executor", "registration"} {
				t.Run(kind, func(t *testing.T) {
					ctx := context.Background()
					store := &requestPrepareStore{}
					m := NewManager(store, nil, nil)
					base, err := m.Register(ctx, &Auth{ID: "fixture", Provider: "antigravity", Attributes: map[string]string{"compat_name": "fixture", "provider_key": "original"}, Metadata: map[string]any{"access_token": "original", "email": "original@example.com", "account_uuid": "original-account", "organization_uuid": "original-organization"}})
					if err != nil {
						t.Fatal(err)
					}
					replacement := base.Clone()
					switch kind {
					case "provider":
						replacement.Provider = "claude"
						replacement.Metadata["access_token"] = "replacement"
					case "token":
						replacement.Metadata["access_token"] = "replacement"
					case "account":
						replacement.Metadata["email"] = "replacement@example.com"
					case "account_uuid", "organization_uuid":
						replacement.Metadata[kind] = "replacement"
					case "executor":
						replacement.Attributes["provider_key"] = "replacement"
					}
					if kind == "registration" {
						_, err = m.Register(ctx, replacement)
					} else {
						_, err = m.Update(ctx, replacement)
					}
					if err != nil {
						t.Fatal(err)
					}
					before, _ := m.GetByID(base.ID)
					disk := store.lastAuth()
					saves := store.saveCount.Load()
					prepared := base.Clone()
					prepared.Metadata["project_id"] = "stale-project"
					prepared.Metadata["access_token"] = "stale-prepared-token"
					if refresh {
						_, err = m.UpdateRefreshedAuth(ctx, base, prepared)
					} else {
						_, err = m.UpdatePreparedAuth(ctx, base, prepared)
					}
					if err == nil && !(refresh && kind == "token") {
						t.Error("stale update accepted")
					}
					after, _ := m.GetByID(base.ID)
					if !reflect.DeepEqual(before, after) {
						t.Error("replacement mutated in memory")
					}
					if !reflect.DeepEqual(disk, store.lastAuth()) || store.saveCount.Load() != saves {
						t.Error("replacement mutated in storage")
					}
				})
			}
		})
	}
}

func TestPreparedUpdatePreservesSameAccountRenewal(t *testing.T) {
	ctx := context.Background()
	store := &requestPrepareStore{}
	m := NewManager(store, nil, nil)
	base, err := m.Register(ctx, &Auth{ID: "fixture", Provider: "antigravity", Metadata: map[string]any{"access_token": "original", "email": "same@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	renewed := base.Clone()
	renewed.Metadata["access_token"] = "renewed"
	current, err := m.UpdateRefreshedAuth(ctx, base, renewed)
	if err != nil {
		t.Fatal(err)
	}
	prepared := current.Clone()
	prepared.Metadata["project_id"] = "fresh-project"
	saved, err := m.UpdatePreparedAuth(ctx, current, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if authAccessToken(saved) != "renewed" || authMetadataString(saved, "project_id") != "fresh-project" {
		t.Fatal("same-account renewal lost")
	}
	if !reflect.DeepEqual(saved, store.lastAuth()) {
		t.Fatal("renewal not persisted")
	}
}

func TestRefreshSurvivesPreparedAccountDiscovery(t *testing.T) {
	for _, initialEmail := range []string{"", "SAME@example.com"} {
		t.Run(initialEmail, func(t *testing.T) {
			ctx := context.Background()
			store := &requestPrepareStore{}
			m := NewManager(store, nil, nil)
			base, err := m.Register(ctx, &Auth{ID: "fixture", Provider: "claude", Metadata: map[string]any{"access_token": "original", "email": initialEmail}})
			if err != nil {
				t.Fatal(err)
			}
			prepared := base.Clone()
			prepared.Metadata["email"] = "same@example.com"
			prepared.Metadata["account_uuid"] = "fixture-account"
			prepared.Metadata["organization_uuid"] = "fixture-organization"
			if _, err := m.UpdatePreparedAuth(ctx, base, prepared); err != nil {
				t.Fatal(err)
			}
			renewed := base.Clone()
			renewed.Metadata["access_token"] = "renewed"
			saved, err := m.UpdateRefreshedAuth(ctx, base, renewed)
			if err != nil {
				t.Fatal(err)
			}
			if authAccessToken(saved) != "renewed" || authMetadataString(saved, "account_uuid") != "fixture-account" || authMetadataString(saved, "organization_uuid") != "fixture-organization" || authMetadataString(saved, "email") != "same@example.com" {
				t.Fatal("renewal lost discovered identity or token")
			}
			current, _ := m.GetByID(base.ID)
			if !reflect.DeepEqual(saved, current) || !reflect.DeepEqual(saved, store.lastAuth()) {
				t.Fatal("renewal not published consistently")
			}
		})
	}
}

type ownershipPreparer struct {
	dispatchPolicyExecutor
	prepare func(*Auth) *Auth
}

func (*ownershipPreparer) ShouldPrepareRequestAuth(*Auth) bool { return true }
func (e *ownershipPreparer) PrepareRequestAuth(_ context.Context, a *Auth) (*Auth, error) {
	return e.prepare(a), nil
}

func TestPreparationOwnershipAtDispatch(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, replace := range []bool{false, true} {
			for _, writesToken := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t/replace=%t/token=%t", stream, replace, writesToken), func(t *testing.T) {
					ctx := context.Background()
					m := NewManager(nil, nil, nil)
					base, err := m.Register(ctx, &Auth{ID: t.Name(), Provider: "antigravity", Metadata: map[string]any{"email": "same@example.com", "access_token": "original"}})
					if err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(base.ID, base.Provider, []*registry.ModelInfo{{ID: "fixture-model"}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(base.ID) })
					e := &ownershipPreparer{dispatchPolicyExecutor: dispatchPolicyExecutor{boundary: "prepare"}}
					e.prepare = func(a *Auth) *Auth {
						current := a.Clone()
						current.Metadata["access_token"] = "current"
						if replace {
							current.Metadata["email"] = "different@example.com"
						}
						if _, err := m.Update(ctx, current); err != nil {
							t.Fatal(err)
						}
						a.Metadata["project_id"] = "discovered-project"
						if writesToken {
							a.Metadata["access_token"] = "stale-prepared"
						}
						return a
					}
					m.RegisterExecutor(e)
					m.RefreshSchedulerEntry(base.ID)
					req := exec.Request{Model: "fixture-model"}
					if stream {
						var result *exec.StreamResult
						result, err = m.ExecuteStream(ctx, []string{base.Provider}, req, exec.Options{})
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					} else {
						_, err = m.Execute(ctx, []string{base.Provider}, req, exec.Options{})
					}
					if replace || writesToken {
						if err == nil || len(e.calls) != 0 {
							t.Fatal("stale preparation dispatched")
						}
						current, _ := m.GetByID(base.ID)
						if authAccessToken(current) != "current" || current.Metadata["project_id"] != nil {
							t.Fatal("rejected preparation changed replacement")
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						if len(e.calls) != 1 || authAccessToken(e.calls[0]) != "current" || authMetadataString(e.calls[0], "project_id") != "discovered-project" {
							t.Fatal("metadata-only preparation did not preserve renewed token")
						}
					}
				})
			}
		}
	}
}
