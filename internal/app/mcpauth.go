package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type ResourceAuth struct{ Issuer, Introspection, Client, Secret string }

func resourceAuthConfig() (ResourceAuth, error) {
	c := ResourceAuth{os.Getenv("MCP_OAUTH_ISSUER"), os.Getenv("MCP_OAUTH_INTROSPECTION_URL"), os.Getenv("MCP_OAUTH_CLIENT_ID"), os.Getenv("MCP_OAUTH_CLIENT_SECRET")}
	if c.Issuer == "" && c.Introspection == "" && c.Client == "" && c.Secret == "" {
		return c, nil
	}
	if !validURL(c.Issuer) || !validURL(c.Introspection) || c.Client == "" || c.Secret == "" {
		return c, errors.New("configure HTTPS MCP OAuth issuer and introspection endpoint, ID and secret together")
	}
	return c, nil
}
func (a *Service) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	if a.C.Auth.Issuer == "" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, fields{"resource": a.C.Public + "/mcp", "authorization_servers": []string{a.C.Auth.Issuer}, "scopes_supported": []string{"social:mcp"}, "bearer_methods_supported": []string{"header"}})
}
func audienceHas(raw json.RawMessage, expected string) bool {
	var values []string
	if json.Unmarshal(raw, &values) == nil {
		for _, value := range values {
			if value == expected {
				return true
			}
		}
	}
	var value string
	return json.Unmarshal(raw, &value) == nil && value == expected
}
func (a *Service) introspect(ctx context.Context, raw string) (Principal, bool) {
	c := a.C.Auth
	if c.Issuer == "" {
		return Principal{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	q := url.Values{"token": {raw}, "token_type_hint": {"access_token"}}
	credentials := url.QueryEscape(c.Client) + ":" + url.QueryEscape(c.Secret)
	header := http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(credentials))}}
	var token struct {
		Active          bool
		Sub, Iss, Scope string
		Aud             json.RawMessage
		Exp             int64
	}
	if _, err := exchange(ctx, a.authClient, http.MethodPost, c.Introspection, header, "application/x-www-form-urlencoded", strings.NewReader(q.Encode()), -1, &token, false); err != nil {
		return Principal{}, false
	}
	if !token.Active || token.Sub == "" || token.Iss != c.Issuer || token.Exp <= time.Now().Unix() || !audienceHas(token.Aud, a.C.Public+"/mcp") {
		return Principal{}, false
	}
	scoped := false
	for _, scope := range strings.Fields(token.Scope) {
		if scope == "social:mcp" {
			scoped = true
		}
	}
	if scoped {
		for _, p := range a.C.Principals {
			if p.Subject == token.Sub {
				return p, true
			}
		}
	}
	return Principal{}, false
}
