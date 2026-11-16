package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Token struct {
	Access   string    `json:"access_token"`
	Refresh  string    `json:"refresh_token,omitempty"`
	Scope    string    `json:"scope,omitempty"`
	Expires  int64     `json:"expires_in,omitempty"`
	Deadline time.Time `json:"deadline"`
	Identity string    `json:"identity"`
	Provider string    `json:"provider"`
}
type Vault struct {
	s    *Store
	aead cipher.AEAD
	mu   sync.Mutex
}

func newVault(s *Store, key []byte) *Vault {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic("credential key must be validated")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic("invalid encryption configuration")
	}
	return &Vault{s: s, aead: aead}
}
func (v *Vault) read(ctx context.Context, a Account) (Token, error) {
	var b []byte
	var token Token
	err := v.s.DB.QueryRowContext(ctx, "SELECT secret FROM credentials WHERE account=?", a.ID).Scan(&b)
	if err != nil {
		return token, err
	}
	n := v.aead.NonceSize()
	if len(b) < n {
		return token, errors.New("invalid credential record")
	}
	decoded, err := v.aead.Open(nil, b[:n], b[n:], []byte(a.ID))
	if err != nil {
		return token, err
	}
	if err = json.Unmarshal(decoded, &token); err != nil {
		return token, err
	}
	if token.Identity != a.RemoteID || token.Provider != a.Platform {
		return Token{}, errors.New("credential identity differs from configuration")
	}
	return token, nil
}
func (v *Vault) save(ctx context.Context, a Account, token Token) error {
	token.Identity, token.Provider = a.RemoteID, a.Platform
	if token.Expires > 0 && token.Expires < 1<<31 {
		token.Deadline = time.Now().Add(time.Duration(token.Expires) * time.Second)
	}
	raw, err := json.Marshal(token)
	if err != nil {
		return err
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	sealed := v.aead.Seal(nonce, nonce, raw, []byte(a.ID))
	tx, err := v.s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO credentials VALUES(?,?) ON CONFLICT(account) DO UPDATE SET secret=excluded.secret", a.ID, sealed); err != nil {
		return err
	}
	if err = audit(ctx, tx, "operator", "credential_saved", a.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *Service) providerReady(account Account) bool {
	p := a.C.Providers[account.Platform]
	return p.Client != "" && (account.Platform == "x" || p.Secret != "")
}
func canRefresh(account Account, t Token) bool {
	return account.Platform == "instagram" || t.Refresh != ""
}
func (a *Service) connection(ctx context.Context, account Account, check bool) string {
	if account.Platform == "whatsapp" {
		if a.C.WAHAKey == "" {
			return "unconfigured"
		}
		if check && a.Adapters[account.Platform].(*platform).working(ctx, account) != nil {
			return "disconnected"
		}
		if check {
			return "connected"
		}
		return "configured"
	}
	if !a.providerReady(account) {
		return "unconfigured"
	}
	token, err := a.Vault.read(ctx, account)
	if err != nil {
		return "authorization_required"
	}
	if !token.Deadline.IsZero() && time.Now().After(token.Deadline) && (account.Platform == "instagram" || !canRefresh(account, token)) {
		return "authorization_required"
	}
	if check {
		token, err = a.access(ctx, account)
		if err != nil {
			return "unavailable"
		}
		if err = a.verifyAccount(ctx, account, token.Access); err != nil {
			return "unavailable"
		}
		return "connected"
	}
	return "configured"
}
func (a *Service) access(ctx context.Context, account Account) (Token, error) {
	if !a.providerReady(account) {
		return Token{}, fail("auth", "provider OAuth is unconfigured")
	}
	v := a.Vault
	v.mu.Lock()
	defer v.mu.Unlock()
	token, err := v.read(ctx, account)
	if err != nil {
		return token, fail("auth", "account authorization required")
	}
	if token.Deadline.IsZero() || time.Until(token.Deadline) > 24*time.Hour && account.Platform == "instagram" || time.Until(token.Deadline) > time.Minute && account.Platform != "instagram" {
		return token, nil
	}
	if !canRefresh(account, token) || (account.Platform == "instagram" && time.Now().After(token.Deadline)) {
		if time.Now().Before(token.Deadline) {
			return token, nil
		}
		return Token{}, fail("auth", "access token expired; reconnect account")
	}
	refreshed, err := a.tokenExchange(ctx, account, "", "", token)
	if err != nil {
		return Token{}, err
	}
	if refreshed.Refresh == "" {
		refreshed.Refresh = token.Refresh
	}
	if refreshed.Scope == "" {
		refreshed.Scope = token.Scope
	}
	if err = v.save(ctx, account, refreshed); err != nil {
		return Token{}, fail("auth", "refresh token could not be stored; reconnect account")
	}
	return refreshed, nil
}

func scopeFor(account Account) string {
	switch account.Platform {
	case "instagram":
		return "instagram_business_basic,instagram_business_content_publish"
	case "x":
		return "tweet.read tweet.write users.read media.write offline.access"
	default:
		if strings.HasPrefix(account.RemoteID, "urn:li:organization:") {
			return "w_organization_social rw_organization_admin"
		}
		return "openid profile w_member_social"
	}
}
func (a *Service) redirectURI(provider string) string {
	return a.C.Public + "/oauth/" + provider + "/callback"
}
func tokenHash(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

func (a *Service) oauthLink(w http.ResponseWriter, r *http.Request, p Principal) error {
	account, ok := a.C.Accounts[r.PathValue("account")]
	_, allowed := p.Accounts[account.ID]
	if !p.Admin || !ok || !allowed {
		return ErrForbidden
	}
	if account.Platform == "whatsapp" || !a.providerReady(account) {
		return ErrUnavailable
	}
	grant := id() + id()
	now := time.Now()
	if _, err := a.S.DB.ExecContext(r.Context(), "DELETE FROM oauth_links WHERE expires<=?", now.Unix()); err != nil {
		return err
	}
	if _, err := a.S.DB.ExecContext(r.Context(), "INSERT INTO oauth_links VALUES(?,?,?)", tokenHash(grant), account.ID, now.Add(2*time.Minute).Unix()); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, fields{"url": a.C.Public + "/connect/browser?grant=" + grant})
	return nil
}
func (a *Service) beginOAuth(w http.ResponseWriter, r *http.Request) error {
	var key string
	err := a.S.DB.QueryRowContext(r.Context(), "DELETE FROM oauth_links WHERE hash=? AND expires>? RETURNING account", tokenHash(r.URL.Query().Get("grant")), time.Now().Unix()).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("connection link is invalid, expired or used")
	}
	if err != nil {
		return err
	}
	account, ok := a.C.Accounts[key]
	if !ok || !a.providerReady(account) {
		return ErrUnavailable
	}
	state, browser, verifier := id()+id(), id()+id(), id()+id()
	now := time.Now()
	if _, err := a.S.DB.ExecContext(r.Context(), "DELETE FROM oauth_state WHERE expires<=?", now.Unix()); err != nil {
		return err
	}
	if _, err := a.S.DB.ExecContext(r.Context(), "INSERT INTO oauth_state VALUES(?,?,?,?,?)", tokenHash(state), account.ID, verifier, tokenHash(browser), now.Add(10*time.Minute).Unix()); err != nil {
		return err
	}
	q := url.Values{"client_id": {a.C.Providers[account.Platform].Client}, "response_type": {"code"}, "state": {state}, "redirect_uri": {a.redirectURI(account.Platform)}, "scope": {scopeFor(account)}}
	var uri string
	switch account.Platform {
	case "instagram":
		uri = "https://www.instagram.com/oauth/authorize"
		q.Set("enable_fb_login", "0")
	case "linkedin":
		uri = "https://www.linkedin.com/oauth/v2/authorization"
	case "x":
		uri = "https://x.com/i/oauth2/authorize"
		sum := sha256.Sum256([]byte(verifier))
		q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
		q.Set("code_challenge_method", "S256")
	}
	http.SetCookie(w, &http.Cookie{Name: "__Host-social-oauth", Value: browser, Path: "/", MaxAge: 600, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, uri+"?"+q.Encode(), http.StatusSeeOther)
	return nil
}
func (a *Service) callback(w http.ResponseWriter, r *http.Request) error {
	cookie, err := r.Cookie("__Host-social-oauth")
	if err != nil {
		return invalid("missing OAuth browser session")
	}
	q := r.URL.Query()
	if q.Get("state") == "" || q.Get("code") == "" {
		return invalid("OAuth authorization failed or was denied")
	}
	var key, verifier string
	err = a.S.DB.QueryRowContext(r.Context(), "DELETE FROM oauth_state WHERE hash=? AND browser=? AND expires>? RETURNING account,verifier", tokenHash(q.Get("state")), tokenHash(cookie.Value), time.Now().Unix()).Scan(&key, &verifier)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("OAuth state is invalid, expired or used")
	}
	if err != nil {
		return err
	}
	account, ok := a.C.Accounts[key]
	if !ok || account.Platform != r.PathValue("provider") || !a.providerReady(account) {
		return invalid("account configuration changed")
	}
	// A single rotating token can be refreshed or replaced at a time.
	a.Vault.mu.Lock()
	defer a.Vault.mu.Unlock()
	token, err := a.tokenExchange(r.Context(), account, q.Get("code"), verifier, Token{})
	if err != nil {
		return ErrUnavailable
	}
	if err = a.verifyAccount(r.Context(), account, token.Access); err != nil {
		return invalid("the authorized identity or organization role does not match the configured account")
	}
	if err = a.Vault.save(r.Context(), account, token); err != nil {
		return err
	}
	a.Log.Info("provider account connected", "account", account.ID, "platform", account.Platform)
	http.SetCookie(w, &http.Cookie{Name: "__Host-social-oauth", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("Account connected. This window can be closed.\n"))
	return nil
}
func (a *Service) tokenExchange(ctx context.Context, account Account, code, verifier string, old Token) (Token, error) {
	p := a.C.Providers[account.Platform]
	client := a.Adapters[account.Platform].(*platform).http
	var token Token
	var address string
	q := url.Values{"client_id": {p.Client}, "client_secret": {p.Secret}, "grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {a.redirectURI(account.Platform)}}
	if code == "" {
		q = url.Values{"client_id": {p.Client}, "client_secret": {p.Secret}, "grant_type": {"refresh_token"}, "refresh_token": {old.Refresh}}
	}
	var h http.Header
	switch account.Platform {
	case "x":
		address = "https://api.x.com/2/oauth2/token"
		q.Del("client_secret")
		if p.Secret != "" {
			h = http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(p.Client+":"+p.Secret))}}
		}
		if code != "" {
			q.Set("code_verifier", verifier)
		}
	case "linkedin":
		address = "https://www.linkedin.com/oauth/v2/accessToken"
	case "instagram":
		if code != "" {
			if _, err := exchange(ctx, client, http.MethodPost, "https://api.instagram.com/oauth/access_token", nil, "application/x-www-form-urlencoded", strings.NewReader(q.Encode()), -1, &token, false); err != nil {
				return token, err
			}
			if token.Access == "" {
				return token, unknown(false)
			}
			address = "https://graph.instagram.com/access_token"
			q = url.Values{"grant_type": {"ig_exchange_token"}, "client_secret": {p.Secret}, "access_token": {token.Access}}
		} else {
			address = "https://graph.instagram.com/refresh_access_token"
			q = url.Values{"grant_type": {"ig_refresh_token"}, "access_token": {old.Access}}
		}
		token = Token{}
		_, err := exchange(ctx, client, http.MethodGet, address+"?"+q.Encode(), nil, "", nil, -1, &token, false)
		if err == nil && token.Access == "" {
			err = unknown(false)
		}
		return token, err
	}
	_, err := exchange(ctx, client, http.MethodPost, address, h, "application/x-www-form-urlencoded", strings.NewReader(q.Encode()), -1, &token, false)
	if err == nil && token.Access == "" {
		err = unknown(false)
	}
	return token, err
}
func (a *Service) verifyAccount(ctx context.Context, account Account, token string) error {
	p := a.Adapters[account.Platform].(*platform)
	mismatch := fail("auth", "authorization identity or organization role does not match")
	switch account.Platform {
	case "x":
		var response struct{ Data struct{ ID string } }
		if _, err := p.json(ctx, http.MethodGet, "/2/users/me", token, nil, &response, false); err != nil {
			return err
		}
		if response.Data.ID != account.RemoteID {
			return mismatch
		}
	case "instagram":
		var response struct {
			UserID json.RawMessage `json:"user_id"`
		}
		if _, err := p.json(ctx, http.MethodGet, "/me?fields=user_id", token, nil, &response, false); err != nil {
			return err
		}
		if strings.Trim(string(response.UserID), "\"") != account.RemoteID {
			return mismatch
		}
	case "linkedin":
		if strings.HasPrefix(account.RemoteID, "urn:li:person:") {
			var response struct{ Sub string }
			if _, err := p.json(ctx, http.MethodGet, "/v2/userinfo", token, nil, &response, false); err != nil {
				return err
			}
			if response.Sub == "" || account.RemoteID != "urn:li:person:"+response.Sub {
				return mismatch
			}
			return nil
		}
		var response struct {
			Elements []struct{ Organization, Role, State string }
		}
		if _, err := p.json(ctx, http.MethodGet, "/rest/organizationAcls?q=roleAssignee&state=APPROVED&count=100", token, nil, &response, false); err != nil {
			return err
		}
		for _, row := range response.Elements {
			if row.Organization == account.RemoteID && row.State == "APPROVED" && (row.Role == "ADMINISTRATOR" || row.Role == "CONTENT_ADMIN") {
				return nil
			}
		}
		return mismatch
	}
	return nil
}

func (a *Service) MaintainCredentials(ctx context.Context) {
	timer := time.NewTicker(6 * time.Hour)
	defer timer.Stop()
	for {
		for _, account := range a.C.Accounts {
			if ctx.Err() != nil {
				return
			}
			if account.Platform != "instagram" && account.Platform != "linkedin" {
				continue
			}
			token, err := a.Vault.read(ctx, account)
			if err != nil || token.Deadline.IsZero() || time.Until(token.Deadline) > 24*time.Hour {
				continue
			}
			if _, err = a.access(ctx, account); err != nil {
				a.Log.Warn("provider credential needs attention", "account", account.ID, "platform", account.Platform)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
