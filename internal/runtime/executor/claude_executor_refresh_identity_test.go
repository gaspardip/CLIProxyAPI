package executor

import (
	"testing"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// A rotated token whose profile lookup failed has no verified account; it must not keep the old one.
func TestClaudeRefreshWithoutProfileDropsAccountIdentity(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		td                     claudeauth.ClaudeTokenData
		wantAccount, wantOrg   string
		wantOrgName, wantEmail string
	}{
		{name: "profile-failed", td: claudeauth.ClaudeTokenData{AccessToken: "fixture-new", RefreshToken: "fixture-refresh-new"}, wantEmail: "a@example.com"},
		{name: "profile-verified", td: claudeauth.ClaudeTokenData{AccessToken: "fixture-new", RefreshToken: "fixture-refresh-new", Email: "b@example.com", AccountUUID: "account-b", OrganizationUUID: "org-b", OrganizationName: "Org B"}, wantAccount: "account-b", wantOrg: "org-b", wantOrgName: "Org B", wantEmail: "b@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "claude", Metadata: map[string]any{
				"access_token": "fixture-old", "refresh_token": "fixture-refresh-old", "email": "a@example.com",
				"account_uuid": "account-a", "organization_uuid": "org-a", "organization_name": "Org A",
			}}
			td := tc.td
			applyClaudeRefreshedTokens(auth, &td)
			got := func(key string) string { value, _ := auth.Metadata[key].(string); return value }
			if got("access_token") != "fixture-new" || got("refresh_token") != "fixture-refresh-new" {
				t.Fatalf("refreshed tokens not applied: %v", auth.Metadata)
			}
			if got("account_uuid") != tc.wantAccount || got("organization_uuid") != tc.wantOrg || got("organization_name") != tc.wantOrgName || got("email") != tc.wantEmail {
				t.Fatalf("identity after refresh = account %q org %q (%q) email %q, want %q %q (%q) %q", got("account_uuid"), got("organization_uuid"), got("organization_name"), got("email"), tc.wantAccount, tc.wantOrg, tc.wantOrgName, tc.wantEmail)
			}
		})
	}
}
