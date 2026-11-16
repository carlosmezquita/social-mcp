package app

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Account struct {
	ID       string `json:"id"`
	Platform string `json:"platform"`
	Name     string `json:"name,omitempty"`
	RemoteID string `json:"remote_id,omitempty"`
	Session  string `json:"session,omitempty"`
	Video    bool   `json:"video,omitempty"`
}

func (a Account) binding() string {
	if a.Platform == "whatsapp" {
		return a.Session
	}
	return a.RemoteID
}

type Principal struct {
	ID       string              `json:"id"`
	Subject  string              `json:"oauth_subject,omitempty"`
	TokenEnv string              `json:"token_env"`
	Publish  bool                `json:"publish"`
	Admin    bool                `json:"admin,omitempty"`
	Accounts map[string][]string `json:"accounts"`
	hash     [32]byte
}

func (p Principal) allows(t Target) bool {
	recipients, ok := p.Accounts[t.Account]
	if !ok {
		return false
	}
	if t.Platform != "whatsapp" {
		return true
	}
	for _, r := range recipients {
		if r == t.Recipient {
			return true
		}
	}
	return false
}

type Provider struct{ Client, Secret, Version string }
type Config struct {
	Listen, Public, DB, Media string
	Key                       []byte
	Poll                      time.Duration
	Retries                   int
	Accounts                  map[string]Account
	Principals                []Principal
	Providers                 map[string]Provider
	WAHA, WAHAKey             string
	PreviewHosts              map[string]bool
	Auth                      ResourceAuth
}

func fallback(name, value string) string {
	if s := os.Getenv(name); s != "" {
		return s
	}
	return value
}
func LoadConfig() (Config, error) {
	c := Config{Listen: fallback("HTTP_LISTEN_ADDRESS", "127.0.0.1:8080"), Public: fallback("PUBLIC_BASE_URL", "https://social.example.com"), DB: fallback("SQLITE_PATH", "/var/lib/social-mcp/social.db"), Media: fallback("MEDIA_DIRECTORY", "/var/lib/social-mcp/media"), WAHA: fallback("WAHA_BASE_URL", "http://127.0.0.1:3000"), WAHAKey: os.Getenv("WAHA_API_KEY"), Accounts: map[string]Account{}, PreviewHosts: map[string]bool{}}
	var err error
	c.Key, err = base64.StdEncoding.DecodeString(os.Getenv("CREDENTIAL_ENCRYPTION_KEY"))
	if err != nil || len(c.Key) != 32 {
		return c, errors.New("CREDENTIAL_ENCRYPTION_KEY requires 32 base64-encoded bytes")
	}
	listen, _, err := net.SplitHostPort(c.Listen)
	if err != nil || net.ParseIP(listen) == nil || !net.ParseIP(listen).IsLoopback() {
		return c, errors.New("HTTP_LISTEN_ADDRESS must use a loopback IP")
	}
	if !validURL(c.Public) {
		return c, errors.New("PUBLIC_BASE_URL must be an HTTPS origin")
	}
	u, _ := url.Parse(c.Public)
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("PUBLIC_BASE_URL must be an HTTPS origin without a trailing slash")
	}
	w, err := url.Parse(c.WAHA)
	if err != nil || w.Scheme != "http" || net.ParseIP(w.Hostname()) == nil || !net.ParseIP(w.Hostname()).IsLoopback() || w.User != nil || w.Path != "" || w.RawQuery != "" || w.Fragment != "" {
		return c, errors.New("WAHA_BASE_URL must be a loopback HTTP origin")
	}
	if !filepath.IsAbs(c.DB) || !filepath.IsAbs(c.Media) {
		return c, errors.New("SQLite and media paths must be absolute")
	}
	c.Poll, err = time.ParseDuration(fallback("SCHEDULER_POLL_INTERVAL", "3s"))
	if err != nil || c.Poll < 100*time.Millisecond || c.Poll > time.Minute {
		return c, errors.New("invalid scheduler poll interval")
	}
	c.Retries, err = strconv.Atoi(fallback("SCHEDULER_MAX_RETRIES", "5"))
	if err != nil || c.Retries < 0 || c.Retries > 12 {
		return c, errors.New("invalid scheduler retry limit")
	}
	c.Providers = map[string]Provider{
		"linkedin":  {os.Getenv("LINKEDIN_CLIENT_ID"), os.Getenv("LINKEDIN_CLIENT_SECRET"), fallback("LINKEDIN_API_VERSION", "202609")},
		"x":         {os.Getenv("X_CLIENT_ID"), os.Getenv("X_CLIENT_SECRET"), ""},
		"instagram": {os.Getenv("META_APP_ID"), os.Getenv("META_APP_SECRET"), fallback("META_API_VERSION", "v26.0")},
	}
	if !regexp.MustCompile(`^[0-9]{6}$`).MatchString(c.Providers["linkedin"].Version) || !regexp.MustCompile(`^v[0-9]+\.[0-9]+$`).MatchString(c.Providers["instagram"].Version) {
		return c, errors.New("invalid provider API version")
	}
	c.Auth, err = resourceAuthConfig()
	if err != nil {
		return c, err
	}
	aliases := map[[2]string]bool{}
	var accounts []Account
	if err = json.Unmarshal([]byte(fallback("SOCIAL_ACCOUNTS", "[]")), &accounts); err != nil {
		return c, errors.New("invalid SOCIAL_ACCOUNTS JSON")
	}
	for _, a := range accounts {
		if !identifier.MatchString(a.ID) || c.Accounts[a.ID].ID != "" {
			return c, errors.New("invalid or duplicate account alias")
		}
		switch a.Platform {
		case "whatsapp":
			if !identifier.MatchString(a.Session) {
				return c, errors.New("invalid WAHA session name")
			}
		case "linkedin":
			if !regexp.MustCompile(`^urn:li:(person:[a-zA-Z0-9_-]+|organization:[0-9]+)$`).MatchString(a.RemoteID) {
				return c, errors.New("invalid LinkedIn author URN")
			}
		case "x", "instagram":
			if !digitID.MatchString(a.RemoteID) {
				return c, errors.New("invalid numeric provider account ID")
			}
		default:
			return c, errors.New("unknown account platform")
		}
		key := [2]string{a.Platform, a.binding()}
		if aliases[key] {
			return c, errors.New("a provider identity can have only one account alias")
		}
		aliases[key] = true
		c.Accounts[a.ID] = a
	}
	if err = json.Unmarshal([]byte(fallback("SOCIAL_CLIENTS", "[]")), &c.Principals); err != nil || len(c.Principals) == 0 {
		return c, errors.New("configure at least one SOCIAL_CLIENTS principal")
	}
	seen := map[string]bool{}
	subjects := map[string]bool{}
	hashes := map[[32]byte]bool{}
	for i := range c.Principals {
		p := &c.Principals[i]
		token := os.Getenv(p.TokenEnv)
		if !identifier.MatchString(p.ID) || seen[p.ID] || (p.TokenEnv != "" && (len(token) < 32 || strings.ContainsAny(token, " \r\n\t"))) || (p.TokenEnv == "" && (p.Subject == "" || c.Auth.Issuer == "")) {
			return c, errors.New("invalid principal or bearer token")
		}
		seen[p.ID] = true
		if p.Subject != "" {
			if c.Auth.Issuer == "" || subjects[p.Subject] {
				return c, errors.New("OAuth subjects require an issuer and must be unique")
			}
			subjects[p.Subject] = true
		}
		if token != "" {
			p.hash = sha256.Sum256([]byte(token))
			if hashes[p.hash] {
				return c, errors.New("bearer tokens must be unique")
			}
			hashes[p.hash] = true
		}
		for account, recipients := range p.Accounts {
			if c.Accounts[account].ID == "" {
				return c, fmt.Errorf("unknown principal account %s", account)
			}
			for _, r := range recipients {
				if !chatID.MatchString(r) {
					return c, errors.New("invalid scoped WhatsApp chat ID")
				}
			}
		}
	}
	for _, host := range strings.Split(os.Getenv("WAHA_PREVIEW_HOSTS"), ",") {
		host = strings.TrimSpace(strings.ToLower(host))
		if host == "" {
			continue
		}
		if !regexp.MustCompile(`^[a-z0-9-]+(?:\.[a-z0-9-]+)+$`).MatchString(host) || net.ParseIP(host) != nil {
			return c, errors.New("preview hosts must be DNS names")
		}
		c.PreviewHosts[host] = true
	}
	return c, nil
}
