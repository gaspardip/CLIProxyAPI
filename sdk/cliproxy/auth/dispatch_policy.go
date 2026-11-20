package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var (
	// errCredentialRevisionChanged: the live credential moved past the revision a result was computed from.
	errCredentialRevisionChanged = errors.New("credential revision changed")
	// errCredentialOwnershipChanged: the live credential now belongs to another provider, registration or account.
	errCredentialOwnershipChanged = fmt.Errorf("credential ownership changed: %w", errCredentialRevisionChanged)
	// errDispatchRevisionChanged: the credential moved while the policy judged it; selection runs again.
	errDispatchRevisionChanged = errors.New("dispatch credential revision changed")
)

// maxDispatchRevisionRetries bounds re-selection and re-preparation against a credential that keeps moving.
const maxDispatchRevisionRetries = 3

// validateDispatchAuth runs the policy on the exact snapshot about to be sent, without rebinding
// affinity, then confirms that snapshot is still the live revision.
func (m *Manager) validateDispatchAuth(ctx context.Context, model string, opts cliproxyexecutor.Options, account *Auth) error {
	// Home owns its dispatched credential authorization.
	if m.HomeEnabled() {
		return nil
	}
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	if filter, ok := selector.(CandidateFilter); ok {
		candidates, err := filter.FilterCandidates(ctx, "mixed", model, opts, []*Auth{account.Clone()})
		if err != nil {
			return err
		}
		authorized := false
		for _, candidate := range candidates {
			if candidate != nil && candidate.ID == account.ID && candidate.Provider == account.Provider && candidate.RegistrationEpoch == account.RegistrationEpoch && candidate.CredentialVersion == account.CredentialVersion && candidate.Generation == account.Generation {
				authorized = true
				break
			}
		}
		if !authorized {
			// A policy may deny a snapshot only because it was superseded; selection then retries on the live revision.
			if !m.isLiveRevision(account) {
				return errDispatchRevisionChanged
			}
			return &Error{Code: "dispatch_policy_denied", Message: "prepared credential is no longer authorized"}
		}
	}
	if !m.isLiveRevision(account) {
		return errDispatchRevisionChanged
	}
	return nil
}

func (m *Manager) isLiveRevision(account *Auth) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return sameRevision(account, m.auths[account.ID])
}

// retryDispatchSelection reports whether a dispatch gate miss should send selection back to the live credential.
func retryDispatchSelection(err error, authID string, tried, attempted map[string]struct{}, retries *int) bool {
	if !errors.Is(err, errDispatchRevisionChanged) || *retries >= maxDispatchRevisionRetries {
		return false
	}
	*retries++
	delete(tried, authID)
	delete(attempted, authID)
	return true
}

// A prepared credential cannot reuse an executor/model resolved for another provider.
func sameDispatchProvider(selected, current *Auth) bool {
	return selected != nil && current != nil && selected.ID == current.ID && selected.Provider == current.Provider && executorKeyFromAuth(selected) == executorKeyFromAuth(current)
}

// sameCredentialOwner treats an identity known on only one side as unknown, so discovery is not a replacement.
func sameCredentialOwner(selected, current *Auth) bool {
	return sameDispatchProvider(selected, current) && selected.RegistrationEpoch == current.RegistrationEpoch && !credentialAccountChanged(selected, current, true)
}

// sameRevision is the identity every write-back and dispatch compares against: same owner and same secret grant.
// Generation is excluded because cooldown bookkeeping bumps it without changing what is sent upstream.
func sameRevision(snapshot, live *Auth) bool {
	return sameCredentialOwner(snapshot, live) && snapshot.CredentialVersion == live.CredentialVersion && !CredentialsChanged(snapshot, live)
}

// liveRevisionLocked returns the live credential while it is still snapshot's revision, or nil when a
// write computed from snapshot must be discarded. The caller holds m.mu.
func (m *Manager) liveRevisionLocked(snapshot *Auth) (*Auth, error) {
	current := m.auths[snapshot.ID]
	if !sameCredentialOwner(snapshot, current) {
		return nil, errCredentialOwnershipChanged
	}
	if !sameRevision(snapshot, current) {
		return nil, nil
	}
	return current, nil
}

func credentialAccount(a *Auth) []string {
	var account []string
	for _, key := range []string{"email", "account_id", "account_uuid", "organization_uuid"} {
		for _, value := range []string{authMetadataString(a, key), a.Attributes[key]} {
			value = strings.TrimSpace(value)
			if key == "email" {
				value = strings.ToLower(value)
			}
			account = append(account, value)
		}
	}
	return account
}

// ignoreMissing allows profile discovery without treating it as an account replacement.
func credentialAccountChanged(base, current *Auth, ignoreMissing bool) bool {
	before, after := credentialAccount(base), credentialAccount(current)
	for i := range before {
		if before[i] != after[i] && (!ignoreMissing || (before[i] != "" && after[i] != "")) {
			return true
		}
	}
	return false
}

// credentialAccountConflict reports a result that contradicts identity learned after its base.
// A result may still change the identity it started from; the dispatch policy judges that revision.
func credentialAccountConflict(base, current, result *Auth) bool {
	before, live, next := credentialAccount(base), credentialAccount(current), credentialAccount(result)
	for i := range before {
		if live[i] != before[i] && next[i] != before[i] && next[i] != live[i] {
			return true
		}
	}
	return false
}

type dispatchProviderContextKey struct{}
