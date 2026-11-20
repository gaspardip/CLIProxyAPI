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
	if e.stage == "refresh" {
		e.replace()
	}
	a = a.Clone()
	a.Metadata["access_token"] = "renewed"
	return a, nil
}

func TestOwnershipConflictLeavesReplacementUnchanged(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream", "bootstrap"} {
		for _, stage := range []string{"prepare", "refresh", "before-refresh"} {
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
