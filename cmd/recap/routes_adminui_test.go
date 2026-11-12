package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/virtUOS/bbb-recap/internal/config"
)

// TestMountAdminUI: without RECAP_OIDC_ISSUER /admin/ is not mounted (404);
// with it, /admin/ and the views (job page, audit list, an action's
// confirmation page) are served and redirect to the login, not 404.
// Mounting does not contact the IdP.
func TestMountAdminUI(t *testing.T) {
	const viewJob = "/admin/jobs/01a18dc6-a921-7f10-b662-7ef3155bd410"
	for _, tc := range []struct {
		name  string
		cfg   config.Config
		paths []string
		want  int
	}{
		{"no issuer", config.Config{}, []string{"/admin/", "/admin/login", "/admin/callback", "/admin/audit", viewJob}, http.StatusNotFound},
		{"issuer set", config.Config{
			PublicBaseURL:    "https://recap.test",
			OIDCIssuer:       "https://idp.recap.test/realms/fixture",
			OIDCClientID:     "recap-admin-ui",
			OIDCClientSecret: "FIXTURE-NOT-A-SECRET-oidc-00000000000000000000000000000000",
			OIDCRoleClaim:    "roles",
			SessionKey:       "FIXTURE-NOT-A-SECRET-session-0000000000000000000000000000000",
		}, []string{"/admin/", "/admin/audit", viewJob, viewJob + "/erase"}, http.StatusFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			cfg := tc.cfg
			if err := mountAdminUI(mux, &deps{cfg: &cfg, log: slog.New(slog.DiscardHandler)}); err != nil {
				t.Fatal(err)
			}
			for _, path := range tc.paths {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				if rec.Code != tc.want {
					t.Errorf("GET %s = %d, want %d", path, rec.Code, tc.want)
				}
			}
		})
	}
}
