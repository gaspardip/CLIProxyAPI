# SDK Advanced: Executors & Translators

This guide explains how to extend the embedded proxy with custom providers and schemas using the SDK. You will:
- Implement a provider executor that talks to your upstream API
- Register request/response translators for schema conversion
- Register models so they appear in `/v1/models`

The examples use Go 1.24+ and the v6 module path.

## Concepts

- Provider executor: a runtime component implementing `auth.ProviderExecutor` that performs outbound calls for a given provider key (e.g., `gemini`, `claude`, `codex`). Executors can also implement `RequestPreparer` to inject credentials on raw HTTP requests.
- Translator registry: schema conversion functions routed by `sdk/translator`. The built‑in handlers translate between OpenAI/Gemini/Claude/Codex formats; you can register new ones.
- Model registry: publishes the list of available models per client/provider to power `/v1/models` and routing hints.

## 1) Implement a Provider Executor

Create a type that satisfies `auth.ProviderExecutor`.

```go
package myprov

import (
  "context"
  "net/http"

  coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
  clipexec "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type Executor struct{}

func (Executor) Identifier() string { return "myprov" }

// Optional: mutate outbound HTTP requests with credentials
func (Executor) PrepareRequest(req *http.Request, a *coreauth.Auth) error {
  // Example: req.Header.Set("Authorization", "Bearer "+a.APIKey)
  return nil
}

func (Executor) Execute(ctx context.Context, a *coreauth.Auth, req clipexec.Request, opts clipexec.Options) (clipexec.Response, error) {
  // Build HTTP request based on req.Payload (already translated into provider format)
  // Use per‑auth transport if provided: transport := a.RoundTripper // via RoundTripperProvider
  // Perform call and return provider JSON payload
  return clipexec.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (Executor) ExecuteStream(ctx context.Context, a *coreauth.Auth, req clipexec.Request, opts clipexec.Options) (<-chan clipexec.StreamChunk, error) {
  ch := make(chan clipexec.StreamChunk, 1)
  go func() { defer close(ch); ch <- clipexec.StreamChunk{Payload: []byte("data: {\"done\":true}\n\n")} }()
  return ch, nil
}

func (Executor) Refresh(ctx context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
  // Optionally refresh tokens and return updated auth
  return a, nil
}
```

Register the executor with the core manager before starting the service:

```go
core := coreauth.NewManager(coreauth.NewFileStore(cfg.AuthDir), nil, nil)
core.RegisterExecutor(myprov.Executor{})
svc, _ := cliproxy.NewBuilder().WithConfig(cfg).WithConfigPath(cfgPath).WithCoreAuthManager(core).Build()
```

If your auth entries use provider `"myprov"`, the manager routes requests to your executor.

## 2) Register Translators

The handlers accept OpenAI/Gemini/Claude/Codex inputs. To support a new provider format, register translation functions in `sdk/translator`’s default registry.

Direction matters:
- Request: register from inbound schema to provider schema
- Response: register from provider schema back to inbound schema

Example: Convert OpenAI Chat → MyProv Chat and back.

```go
package myprov

import (
  "context"
  sdktr "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

const (
  FOpenAI = sdktr.Format("openai.chat")
  FMyProv = sdktr.Format("myprov.chat")
)

func init() {
  sdktr.Register(FOpenAI, FMyProv,
    // Request transform (model, rawJSON, stream)
    func(model string, raw []byte, stream bool) []byte { return convertOpenAIToMyProv(model, raw, stream) },
    // Response transform (stream & non‑stream)
    sdktr.ResponseTransform{
      Stream: func(ctx context.Context, model string, originalReq, translatedReq, raw []byte, param *any) []string {
        return convertStreamMyProvToOpenAI(model, originalReq, translatedReq, raw)
      },
      NonStream: func(ctx context.Context, model string, originalReq, translatedReq, raw []byte, param *any) string {
        return convertMyProvToOpenAI(model, originalReq, translatedReq, raw)
      },
    },
  )
}
```

When the OpenAI handler receives a request that should route to `myprov`, the pipeline uses the registered transforms automatically.

## 3) Register Models

Expose models under `/v1/models` by registering them in the global model registry using the auth ID (client ID) and provider name.

```go
models := []*cliproxy.ModelInfo{
  { ID: "myprov-pro-1", Object: "model", Type: "myprov", DisplayName: "MyProv Pro 1" },
}
cliproxy.GlobalModelRegistry().RegisterClient(authID, "myprov", models)
```

The embedded server calls this automatically for built‑in providers; for custom providers, register during startup (e.g., after loading auths) or upon auth registration hooks.

## Credentials & Transports

- Use `Manager.SetRoundTripperProvider` to inject per‑auth `*http.Transport` (e.g., proxy):
  ```go
  core.SetRoundTripperProvider(myProvider) // returns transport per auth
  ```
- For raw HTTP flows, implement `PrepareRequest` and/or call `Manager.InjectCredentials(req, authID)` to set headers.

## Testing Tips

- Enable request logging: Management API GET/PUT `/v0/management/request-log`
- Toggle debug logs: Management API GET/PUT `/v0/management/debug`
- Hot reload changes in `config.yaml` and `auths/` are picked up automatically by the watcher


## Custom account routing (v8)

`Builder.WithSelectorFactory` owns selector construction at startup and when
normalized routing settings change. It overrides the selector even when a core
auth manager is injected. Unrelated config reloads retain the existing selector
and session bindings. A factory error or nil result rejects a routing config
update before publication.

```go
builder.WithSelectorFactory(func(cfg *config.Config) (coreauth.Selector, error) {
    return cliproxy.NewRoutingSelector(cfg, quotaPolicy), nil
})
```

The factory receives an isolated configuration snapshot and must return a fresh,
service-owned selector. Construction must not perform networking or other blocking
runtime work. Keep quota collection and the lifetime of shared policy state in
the embedding application. The service stops discarded or replaced selectors that
implement `auth.StoppableSelector`; native affinity owns its cache, not the
lifetime of its fallback policy.

`NewRoutingSelector(cfg, fallback)` applies native affinity options around the
custom account policy. A nil fallback selects the configured built-in strategy.
A policy implementing `auth.CandidateFilter` can additionally remove candidates
before affinity looks up explicit, parent/subagent, or longest-prefix bindings.
Filter errors abort selection. This lets an application exclude paid accounts
when included quota becomes available, even for previously bound sessions.
Filters must return a subset of their input candidates.

Scheduler plugins and Home dispatch have separate routing paths. This selector
hook does not make those paths obey the policy. Disable them in an application
that relies on the selector to enforce spending rules. Also account for retry
candidate narrowing: never infer global quota exhaustion solely from a retry's
remaining candidates.

Applications can use `Hooks.ValidateConfig` to reject unsafe configuration on
startup (after Home mode's runtime overrides, and again after `OnBeforeStart`),
every watcher reload, and v8 management saves, including plugin-store installs
and plugin deletes, before configuration is persisted or published. Validation
receives an isolated snapshot, must not mutate service state or perform blocking
runtime work, and returns an error to reject the update. Rejected management
saves leave the file and the handler's config unchanged. A rejected config file
is not adopted by the watcher, so no credentials are refreshed from it.
Deprecated `/v0/management` endpoints are not validated. Without a validator,
behavior is unchanged. For example, an embedding application can require API
keys, a private bind address, and disabled scheduler plugins for its quota policy.
