package main

import (
	"errors"
	"net/http"
	"strings"

	"github.com/virtUOS/bbb-recap/internal/adminui/mockidp"
)

// mountOIDC serves the mock OIDC provider for the admin UI when
// -oidc-issuer is set (internal/adminui/mockidp): discovery, JWKS, an
// authorization endpoint that approves the fixture user -oidc-sub with the
// roles -oidc-roles at once, and the token endpoint. The signing key is
// generated at start. Only -oidc-redirect-uri is accepted as redirect URI.
// Without -oidc-issuer nothing is mounted.
func mountOIDC(mux *http.ServeMux, c config) error {
	if c.OIDCIssuer == "" {
		return nil
	}
	secret, err := readKey(c.OIDCClientSecretFile)
	if err != nil {
		return err
	}
	if secret == "" {
		return errors.New("-oidc-client-secret is required with -oidc-issuer")
	}
	var roles []string
	for _, r := range strings.Split(c.OIDCRoles, ",") {
		if r = strings.TrimSpace(r); r != "" {
			roles = append(roles, r)
		}
	}
	p, err := mockidp.New(mockidp.Config{
		Issuer:           c.OIDCIssuer,
		AuthorizeBaseURL: c.OIDCBrowserURL,
		ClientID:         c.OIDCClientID,
		ClientSecret:     secret,
		RedirectURI:      c.OIDCRedirectURI,
		Subject:          c.OIDCSubject,
		Roles:            roles,
		Log:              c.Log,
	})
	if err != nil {
		return err
	}
	p.Mount(mux)
	return nil
}
