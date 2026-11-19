package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type dispatchPolicy struct{ checked []string }

func (p *dispatchPolicy) FilterCandidates(_ context.Context, _, _ string, _ exec.Options, accounts []*Auth) ([]*Auth, error) {
	var kept []*Auth
	for _, a := range accounts {
		p.checked = append(p.checked, authMetadataString(a, "access_token"))
		if authMetadataString(a, "email") == "authorized@example.com" {
			kept = append(kept, a)
		}
	}
	return kept, nil
}
func (p *dispatchPolicy) Pick(ctx context.Context, provider, model string, opts exec.Options, accounts []*Auth) (*Auth, error) {
	kept, err := p.FilterCandidates(ctx, provider, model, opts, accounts)
	if err != nil {
		return nil, err
	}
	if len(kept) == 0 {
		return nil, &Error{Code: "policy_denied", Message: "fixture denied"}
	}
	return kept[0], nil
}

type dispatchPolicyExecutor struct {
	boundary string
	replace  bool
	calls    []*Auth
}

func (*dispatchPolicyExecutor) Identifier() string                    { return "antigravity" }
func (e *dispatchPolicyExecutor) ShouldPrepareRequestAuth(*Auth) bool { return e.boundary == "prepare" }
func (e *dispatchPolicyExecutor) change(a *Auth) *Auth {
	a = a.Clone()
	a.Metadata["access_token"] = "fixture-refreshed"
	if e.replace {
		a.Metadata["email"] = "replacement@example.com"
	}
	return a
}
func (e *dispatchPolicyExecutor) PrepareRequestAuth(_ context.Context, a *Auth) (*Auth, error) {
	return e.change(a), nil
}
func (e *dispatchPolicyExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) {
	return e.change(a), nil
}
func (e *dispatchPolicyExecutor) Execute(_ context.Context, a *Auth, _ exec.Request, _ exec.Options) (exec.Response, error) {
	e.calls = append(e.calls, a.Clone())
	if e.boundary != "prepare" && len(e.calls) == 1 {
		return exec.Response{}, &Error{HTTPStatus: 401, Message: "fixture unauthorized"}
	}
	return exec.Response{Payload: []byte("ok")}, nil
}
func (e *dispatchPolicyExecutor) ExecuteStream(ctx context.Context, a *Auth, req exec.Request, opts exec.Options) (*exec.StreamResult, error) {
	response, err := e.Execute(ctx, a, req, opts)
	if err != nil && e.boundary != "bootstrap" {
		return nil, err
	}
	chunks := make(chan exec.StreamChunk, 1)
	chunks <- exec.StreamChunk{Payload: response.Payload, Err: err}
	close(chunks)
	return &exec.StreamResult{Chunks: chunks}, nil
}
func (e *dispatchPolicyExecutor) CountTokens(ctx context.Context, a *Auth, req exec.Request, opts exec.Options) (exec.Response, error) {
	return e.Execute(ctx, a, req, opts)
}
func (*dispatchPolicyExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("unused")
}

func TestDispatchPolicyRevalidatesPreparedAndRefreshedAuth(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "count"} {
		for _, boundary := range []string{"prepare", "401", "bootstrap"} {
			if boundary == "bootstrap" && mode != "stream" {
				continue
			}
			for _, replace := range []bool{false, true} {
				for _, affinity := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/replace=%t/affinity=%t", mode, boundary, replace, affinity), func(t *testing.T) {
						ctx := context.Background()
						policy := &dispatchPolicy{}
						var selector Selector = policy
						if affinity {
							a := NewSessionAffinitySelector(policy)
							defer a.Stop()
							selector = a
						}
						m := NewManager(nil, selector, nil)
						e := &dispatchPolicyExecutor{boundary: boundary, replace: replace}
						m.RegisterExecutor(e)
						a := &Auth{ID: t.Name(), Provider: "antigravity", Metadata: map[string]any{"email": "authorized@example.com", "access_token": "fixture-original", "refresh_token": "fixture-refresh"}}
						if _, err := m.Register(ctx, a); err != nil {
							t.Fatal(err)
						}
						registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "fixture-model"}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
						m.RefreshSchedulerEntry(a.ID)
						opts := exec.Options{Headers: http.Header{"Session_id": {"fixture-policy"}}}
						req := exec.Request{Model: "fixture-model"}
						var err error
						switch mode {
						case "execute":
							_, err = m.Execute(ctx, []string{a.Provider}, req, opts)
						case "count":
							_, err = m.ExecuteCount(ctx, []string{a.Provider}, req, opts)
						case "stream":
							var result *exec.StreamResult
							result, err = m.ExecuteStream(ctx, []string{a.Provider}, req, opts)
							if err == nil {
								for chunk := range result.Chunks {
									if chunk.Err != nil {
										err = chunk.Err
									}
								}
							}
						}
						if replace {
							if err == nil {
								t.Error("replacement dispatched without authorization")
							}
							for _, actual := range e.calls {
								if authMetadataString(actual, "email") != "authorized@example.com" {
									t.Error("replacement account reached executor")
								}
							}
						} else {
							if err != nil {
								t.Fatal(err)
							}
							if len(e.calls) == 0 || authAccessToken(e.calls[len(e.calls)-1]) != "fixture-refreshed" {
								t.Fatal("same-grant refresh did not execute")
							}
						}
						if len(policy.checked) == 0 || policy.checked[len(policy.checked)-1] != "fixture-refreshed" {
							t.Error("actual dispatch credential was not checked")
						}
					})
				}
			}
		}
	}
}
