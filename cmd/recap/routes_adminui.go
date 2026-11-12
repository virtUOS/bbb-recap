package main

import (
	"net/http"

	"github.com/virtUOS/bbb-recap/internal/admin"
	"github.com/virtUOS/bbb-recap/internal/adminui"
)

// mountAdminUI mounts the admin web UI under /admin/ (architecture.md §11)
// with its views and actions when RECAP_OIDC_ISSUER is set; otherwise
// /admin/ stays unmounted (404).
func mountAdminUI(mux *http.ServeMux, d *deps) error {
	if d.cfg.OIDCIssuer == "" {
		return nil
	}
	ui, err := adminui.New(adminui.Config{
		PublicBaseURL: d.cfg.PublicBaseURL,
		Issuer:        d.cfg.OIDCIssuer,
		ClientID:      d.cfg.OIDCClientID,
		ClientSecret:  d.cfg.OIDCClientSecret,
		RoleClaim:     d.cfg.OIDCRoleClaim,
		SessionKey:    []byte(d.cfg.SessionKey),
		Log:           d.log,
		// The views call the admin API's functions, configured as in mountAdmin.
		Admin: admin.New(admin.Config{
			Pool:                   d.pool,
			Blobs:                  d.blobs,
			Results:                d.results,
			ConfigHash:             d.cfg.Hash(),
			RetentionAfterDelivery: d.cfg.RetentionAfterDelivery,
			Log:                    d.log,
		}),
	})
	if err != nil {
		return err
	}
	ui.Mount(mux)
	d.log.Info("admin UI enabled")
	return nil
}
