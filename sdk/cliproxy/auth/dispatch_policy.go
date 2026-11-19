package auth

import (
	"context"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Preparation and 401 recovery may replace the selected credential. Reauthorize
// the actual dispatch candidate against the policy's full pool, without rebinding affinity.
func (m *Manager) validateDispatchAuth(ctx context.Context, model string, opts cliproxyexecutor.Options, account *Auth) error {
	// Home owns its dispatched credential authorization.
	if m.HomeEnabled() {
		return nil
	}
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	filter, ok := selector.(CandidateFilter)
	if !ok {
		return nil
	}
	candidates, err := filter.FilterCandidates(ctx, "mixed", model, opts, []*Auth{account.Clone()})
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if candidate != nil && candidate.ID == account.ID && candidate.Provider == account.Provider && candidate.RegistrationEpoch == account.RegistrationEpoch && candidate.CredentialVersion == account.CredentialVersion && candidate.Generation == account.Generation {
			return nil
		}
	}
	return &Error{Code: "dispatch_policy_denied", Message: "prepared credential is no longer authorized"}
}

// A prepared credential cannot reuse an executor/model resolved for another provider.
func sameDispatchProvider(selected, current *Auth) bool {
	return selected != nil && current != nil && selected.ID == current.ID && selected.Provider == current.Provider && executorKeyFromAuth(selected) == executorKeyFromAuth(current)
}

type dispatchProviderContextKey struct{}
