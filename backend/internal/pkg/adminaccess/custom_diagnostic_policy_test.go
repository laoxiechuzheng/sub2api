//go:build unit

package adminaccess

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Diagnostics contain raw request/response bodies and must never be accessible to scoped keys.
func TestScopedAdminPolicyNeverAllowsRequestDiagnostics(t *testing.T) {
	var all Policy
	for _, permission := range PermissionCatalog() {
		if permission.Scope == "global" {
			all.GlobalPermissions = append(all.GlobalPermissions, permission.ID)
		}
	}
	require.True(t, RouteAllowed(all, "GET", "/api/v1/admin/usage"))
	require.False(t, RouteAllowed(all, "GET", "/api/v1/admin/usage/:id/diagnostic"))

	for _, definition := range definitions {
		for _, route := range definition.Routes {
			require.NotContains(t, route, "diagnostic", definition.ID)
		}
	}
}
