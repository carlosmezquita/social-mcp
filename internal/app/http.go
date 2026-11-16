package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type actorKey struct{}

func actor(ctx context.Context) Principal { p, _ := ctx.Value(actorKey{}).(Principal); return p }

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func publicError(err error) (int, string) {
	switch {
	case errors.Is(err, ErrInvalid):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden, ErrForbidden.Error()
	case errors.Is(err, ErrMissing):
		return http.StatusNotFound, ErrMissing.Error()
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, ErrConflict.Error()
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable, ErrUnavailable.Error()
	default:
		return http.StatusInternalServerError, "internal service error"
	}
}
func respond(w http.ResponseWriter, status int, value any, err error) {
	if err != nil {
		code, message := publicError(err)
		writeJSON(w, code, fields{"error": message})
		return
	}
	writeJSON(w, status, value)
}
func decode(r *http.Request, dst any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(dst) != nil || d.Decode(new(any)) != io.EOF {
		return invalid("invalid or oversized JSON request")
	}
	return nil
}

type rateWindow struct {
	At    time.Time
	Count int
}
type limiter struct {
	mu      sync.Mutex
	windows map[string]rateWindow
}

func (l *limiter) allow(key string, budget int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	current := l.windows[key]
	if now.Sub(current.At) >= time.Minute {
		current = rateWindow{At: now}
		for k, w := range l.windows {
			if now.Sub(w.At) >= time.Minute {
				delete(l.windows, k)
			}
		}
	}
	if current.Count >= budget {
		return false
	}
	current.Count++
	l.windows[key] = current
	return true
}
func (a *Service) authenticate(r *http.Request) (Principal, bool) {
	scheme, raw, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || raw == "" || len(raw) > 8192 {
		return Principal{}, false
	}
	hash := sha256.Sum256([]byte(raw))
	for _, p := range a.C.Principals {
		if subtle.ConstantTimeCompare(hash[:], p.hash[:]) == 1 {
			return p, true
		}
	}
	return a.introspect(r.Context(), raw)
}
func (a *Service) httpAccounts(ctx context.Context, p Principal, check bool) []fields {
	out := []fields{}
	for _, key := range sortedAccounts(a.C.Accounts) {
		account := a.C.Accounts[key]
		if _, allowed := p.Accounts[key]; !allowed {
			continue
		}
		var credential Token
		if account.Platform != "whatsapp" {
			credential, _ = a.Vault.read(ctx, account)
		}
		row := fields{"id": key, "platform": account.Platform, "name": account.Name, "status": a.connection(ctx, account, check)}
		if credential.Scope != "" {
			row["granted_scope"] = credential.Scope
		}
		if !credential.Deadline.IsZero() {
			row["expires_at"] = credential.Deadline
		}
		out = append(out, row)
	}
	return out
}
func (a *Service) detail(ctx context.Context, p Principal, key string, history bool) ([]fields, error) {
	ds, err := a.S.get(ctx, p, key)
	if err != nil {
		return nil, err
	}
	out := make([]fields, 0, len(ds))
	for _, d := range ds {
		row := fields{"delivery": d}
		if history {
			attempts, err := a.S.attempts(ctx, d.ID)
			if err != nil {
				return nil, err
			}
			row["history"] = attempts
		}
		out = append(out, row)
	}
	return out, nil
}

func (a *Service) HTTP() http.Handler {
	mux := http.NewServeMux()
	protect := func(f func(http.ResponseWriter, *http.Request, Principal)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { f(w, r, actor(r.Context())) }
	}
	check := func(f func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if err := f(w, r); err != nil {
				respond(w, 0, nil, err)
			}
		}
	}
	mcpHandler := a.mcp()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		// The outer handler has checked the original Host and Origin. The SDK
		// sees the upstream loopback authority and keeps its rebinding check.
		upstream := r.Clone(r.Context())
		upstream.Host = a.C.Listen
		mcpHandler.ServeHTTP(w, upstream)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, fields{"healthy": true}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		status := http.StatusOK
		if a.S.DB.PingContext(ctx) != nil {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, fields{"ready": status == http.StatusOK})
	})
	mux.HandleFunc("GET /public/media/{id}", a.serveMedia)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", a.resourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", a.resourceMetadata)
	mux.HandleFunc("POST /connect/{account}", protect(func(w http.ResponseWriter, r *http.Request, p Principal) {
		if err := a.oauthLink(w, r, p); err != nil {
			respond(w, 0, nil, err)
		}
	}))
	mux.HandleFunc("GET /connect/browser", check(a.beginOAuth))
	mux.HandleFunc("GET /oauth/{provider}/callback", check(a.callback))
	for route, scheduled := range map[string]bool{"POST /api/publish": false, "POST /api/schedule": true} {
		mux.HandleFunc(route, protect(func(w http.ResponseWriter, r *http.Request, p Principal) {
			var req Request
			if err := decode(r, &req); err != nil {
				respond(w, 0, nil, err)
				return
			}
			ds, err := a.submit(r.Context(), p, req, scheduled)
			respond(w, http.StatusAccepted, ds, err)
		}))
	}
	list := protect(func(w http.ResponseWriter, r *http.Request, p Principal) {
		q := r.URL.Query()
		f := Filter{State: q.Get("state"), Platform: q.Get("platform"), Account: q.Get("account_id"), BeforeID: q.Get("before_id")}
		var err error
		if q.Has("limit") {
			f.Limit, err = strconv.Atoi(q.Get("limit"))
		}
		if err == nil && q.Has("from") {
			f.From, err = time.Parse(time.RFC3339, q.Get("from"))
		}
		if err == nil && q.Has("to") {
			f.To, err = time.Parse(time.RFC3339, q.Get("to"))
		}
		if err != nil {
			respond(w, 0, nil, invalid("invalid limit or RFC3339 date filter"))
			return
		}
		ds, err := a.S.list(r.Context(), p, f)
		respond(w, http.StatusOK, ds, err)
	})
	mux.HandleFunc("GET /api/deliveries", list)
	mux.HandleFunc("GET /api/campaigns", list)
	details := protect(func(w http.ResponseWriter, r *http.Request, p Principal) {
		ds, err := a.detail(r.Context(), p, r.PathValue("id"), true)
		respond(w, http.StatusOK, ds, err)
	})
	mux.HandleFunc("GET /api/deliveries/{id}", details)
	mux.HandleFunc("GET /api/campaigns/{id}", details)
	mux.HandleFunc("PATCH /api/deliveries/{id}", protect(func(w http.ResponseWriter, r *http.Request, p Principal) {
		var req Edit
		if err := decode(r, &req); err != nil {
			respond(w, 0, nil, err)
			return
		}
		req.ID = r.PathValue("id")
		ds, err := a.change(r.Context(), p, req, false)
		respond(w, http.StatusOK, ds, err)
	}))
	mux.HandleFunc("DELETE /api/deliveries/{id}", protect(func(w http.ResponseWriter, r *http.Request, p Principal) {
		ds, err := a.change(r.Context(), p, Edit{ID: r.PathValue("id")}, true)
		respond(w, http.StatusOK, ds, err)
	}))
	mux.HandleFunc("GET /api/accounts", protect(func(w http.ResponseWriter, r *http.Request, p Principal) {
		writeJSON(w, http.StatusOK, a.httpAccounts(r.Context(), p, r.URL.Query().Get("check") == "1"))
	}))
	mux.HandleFunc("GET /api/capabilities", protect(func(w http.ResponseWriter, r *http.Request, p Principal) {
		writeJSON(w, http.StatusOK, a.capabilities(r.Context(), p))
	}))
	mux.HandleFunc("GET /api/whatsapp/chats/{id}", protect(func(w http.ResponseWriter, r *http.Request, p Principal) {
		offset := 0
		if r.URL.Query().Has("offset") {
			var err error
			offset, err = strconv.Atoi(r.URL.Query().Get("offset"))
			if err != nil {
				respond(w, 0, nil, invalid("invalid offset"))
				return
			}
		}
		out, err := a.chats(r.Context(), p, r.PathValue("id"), offset)
		respond(w, http.StatusOK, out, err)
	}))
	mux.HandleFunc("GET /api/deliveries/{id}/ack", protect(func(w http.ResponseWriter, r *http.Request, p Principal) {
		ds, err := a.S.get(r.Context(), p, r.PathValue("id"))
		if err != nil {
			respond(w, 0, nil, err)
			return
		}
		d := ds[0]
		if len(ds) != 1 || d.ID != r.PathValue("id") || d.Target.Platform != "whatsapp" || d.ExternalID == "" {
			respond(w, 0, nil, ErrMissing)
			return
		}
		data, err := a.whatsAppAck(r.Context(), d)
		respond(w, http.StatusOK, data, err)
	}))
	mux.HandleFunc("POST /api/media", protect(func(w http.ResponseWriter, r *http.Request, p Principal) {
		if !p.Publish || len(p.Accounts) == 0 {
			respond(w, 0, nil, ErrForbidden)
			return
		}
		if r.ContentLength > MaxMedia {
			respond(w, 0, nil, invalid("media exceeds 64 MiB"))
			return
		}
		m, err := a.S.upload(r.Context(), p, r.Body)
		respond(w, http.StatusCreated, m, err)
	}))
	u, _ := url.Parse(a.C.Public)
	limits := &limiter{windows: map[string]rateWindow{}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		host := r.Host
		local := host == a.C.Listen
		if !strings.EqualFold(host, u.Host) && !local {
			http.Error(w, "invalid host", http.StatusMisdirectedRequest)
			return
		}
		if origins := r.Header.Values("Origin"); len(origins) > 0 && (len(origins) != 1 || origins[0] != a.C.Public) {
			http.Error(w, "invalid origin", http.StatusForbidden)
			return
		}
		path := r.URL.Path
		if (path == "/healthz" || path == "/readyz") && !local {
			http.NotFound(w, r)
			return
		}
		capBytes := int64(256 << 10)
		if path == "/api/media" && r.Method == http.MethodPost {
			capBytes = MaxMedia + 1
		}
		r.Body = http.MaxBytesReader(w, r.Body, capBytes)
		public := path == "/healthz" || path == "/readyz" || path == "/connect/browser" || strings.HasPrefix(path, "/public/media/") || strings.HasPrefix(path, "/oauth/") || path == "/.well-known/oauth-protected-resource/mcp" || path == "/.well-known/oauth-protected-resource"
		if !public {
			// The transport peer is trusted only as loopback. Ignore caller-supplied forwarding headers.
			remote, _, _ := net.SplitHostPort(r.RemoteAddr)
			if !limits.allow("peer:"+remote, 1200) {
				w.Header().Set("Retry-After", "60")
				http.Error(w, "rate limit", http.StatusTooManyRequests)
				return
			}
			p, ok := a.authenticate(r)
			if !ok {
				challenge := "Bearer"
				if a.C.Auth.Issuer != "" && path == "/mcp" {
					challenge += ` resource_metadata="` + a.C.Public + `/.well-known/oauth-protected-resource/mcp"`
				}
				w.Header().Set("WWW-Authenticate", challenge)
				http.Error(w, "authorization required", http.StatusUnauthorized)
				return
			}
			budget := 120
			if path == "/api/media" {
				budget = 12
			}
			if !limits.allow(p.ID+":"+strconv.Itoa(budget), budget) {
				w.Header().Set("Retry-After", "60")
				http.Error(w, "rate limit", http.StatusTooManyRequests)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), actorKey{}, p))
		}
		mux.ServeHTTP(w, r)
	})
}
func (a *Service) Backup(ctx context.Context, dest string) error { return a.S.backup(ctx, dest) }
func (a *Service) Verify(ctx context.Context) error {
	var check string
	if err := a.S.DB.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return errors.New("SQLite integrity check failed")
	}
	rows, err := a.S.DB.QueryContext(ctx, "SELECT id,mime,size,sha256 FROM media")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var m Media
		if err = rows.Scan(&m.ID, &m.MIME, &m.Size, &m.SHA); err != nil {
			return err
		}
		f, err := os.Open(a.S.mediaDir + "/" + m.ID)
		if err != nil {
			return err
		}
		hash := sha256.New()
		n, err := io.Copy(hash, f)
		f.Close()
		if err != nil || n != m.Size || m.SHA != hex.EncodeToString(hash.Sum(nil)) {
			return errors.New("media integrity check failed")
		}
	}
	return rows.Err()
}
