package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type platform struct {
	kind string
	app  *Service
	http *http.Client
	base string
}
type fields map[string]any

func providerClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.ResponseHeaderTimeout = 35 * time.Second
	return &http.Client{Transport: t, Timeout: 75 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func providerBase(name string, c Config) string {
	switch name {
	case "whatsapp":
		return c.WAHA
	case "linkedin":
		return "https://api.linkedin.com"
	case "instagram":
		return "https://graph.instagram.com/" + c.Providers[name].Version
	case "x":
		return "https://api.x.com"
	}
	return ""
}
func bearer(token string) http.Header { return http.Header{"Authorization": {"Bearer " + token}} }
func (p *platform) headers(token string) http.Header {
	if p.kind == "whatsapp" {
		return http.Header{"X-Api-Key": {p.app.C.WAHAKey}}
	}
	h := bearer(token)
	if p.kind == "linkedin" {
		h.Set("LinkedIn-Version", p.app.C.Providers["linkedin"].Version)
		h.Set("X-Restli-Protocol-Version", "2.0.0")
	}
	return h
}
func (p *platform) json(ctx context.Context, method, path, token string, in, out any, publish bool) (http.Header, error) {
	var r io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, fail("permanent", "failed to assemble provider payload")
		}
		r = bytes.NewReader(b)
	}
	return exchange(ctx, p.http, method, p.base+path, p.headers(token), "application/json", r, -1, out, publish)
}
func exchange(ctx context.Context, client *http.Client, method, address string, headers http.Header, contentType string, body io.Reader, length int64, out any, publish bool) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return nil, fail("permanent", "invalid provider request")
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if length >= 0 {
		req.ContentLength = length
	}
	// The transport may replay a request with GetBody after a connection failure.
	if publish {
		req.GetBody = nil
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, unknown(publish)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		switch {
		case res.StatusCode == 429:
			return nil, &Failure{Kind: "rate_limited", Message: "provider rate limit", After: retryAfter(res.Header)}
		case res.StatusCode == 401 || res.StatusCode == 403:
			return nil, fail("auth", "provider denied authorization or account permissions")
		case res.StatusCode == 402:
			return nil, fail("permanent", "provider requires billing or API credits")
		case res.StatusCode == 408 || res.StatusCode >= 500:
			return nil, unknown(publish)
		default:
			return nil, fail("permanent", fmt.Sprintf("provider rejected request (HTTP %d)", res.StatusCode))
		}
	}
	if out != nil {
		data, err := io.ReadAll(io.LimitReader(res.Body, 2<<20+1))
		if err != nil || len(data) > 2<<20 || json.Unmarshal(data, out) != nil {
			return nil, unknown(publish)
		}
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 8192))
	}
	return res.Header, nil
}
func unknown(publish bool) *Failure {
	if publish {
		return fail("ambiguous", "provider response did not confirm whether publication was accepted")
	}
	return fail("transient", "provider unavailable or returned an incomplete response")
}
func retryAfter(h http.Header) time.Duration {
	if n, err := strconv.ParseInt(h.Get("Retry-After"), 10, 64); err == nil && n > 0 && n < 1<<30 {
		return time.Second * time.Duration(n)
	}
	if at, err := http.ParseTime(h.Get("Retry-After")); err == nil {
		return max(0, time.Until(at))
	}
	if n, err := strconv.ParseInt(h.Get("X-Rate-Limit-Reset"), 10, 64); err == nil {
		return max(0, time.Until(time.Unix(n, 0)))
	}
	return 0
}
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return unknown(false)
	case <-timer.C:
		return nil
	}
}

var urlInText = regexp.MustCompile(`(?i)https?://[^\s<>]+`)

func xLength(text string) int {
	weight := func(s string) int {
		n := 0
		for _, r := range s {
			if r <= 0x10ff || (r >= 0x2000 && r <= 0x200d) || (r >= 0x2010 && r <= 0x201f) || (r >= 0x2032 && r <= 0x2037) {
				n++
			} else {
				n += 2
			}
		}
		return n
	}
	offset, total := 0, 0
	for _, m := range urlInText.FindAllStringIndex(text, -1) {
		link := strings.TrimRight(text[m[0]:m[1]], ".,!?;:)]}")
		total += weight(text[offset:m[0]]) + 23
		offset = m[0] + len(link)
	}
	return total + weight(text[offset:])
}
func (p *platform) Validate(t Target, m Media) error {
	text := bodyText(t)
	limit := 0
	size := int64(MaxMedia)
	switch p.kind {
	case "instagram":
		limit = 2200
		if m.MIME != "image/jpeg" && m.MIME != "video/mp4" {
			return invalid("Instagram requires one JPEG or MP4 Reel")
		}
		if m.MIME == "image/jpeg" {
			size = 8 << 20
		}
	case "x":
		if xLength(text) > 280 {
			return invalid("X text exceeds 280 weighted characters")
		}
		if strings.HasPrefix(m.MIME, "image/") {
			size = 5 << 20
		}
	case "linkedin":
		limit = 3000
		if strings.HasPrefix(m.MIME, "image/") {
			size = 8 << 20
		}
	case "whatsapp":
		limit = 4096
		if strings.HasPrefix(m.MIME, "image/") {
			size = 5 << 20
		}
		if m.MIME == "video/mp4" {
			size = 16 << 20
		}
		if t.Custom == nil && m.ID == "" && (t.Preview == nil || *t.Preview) {
			for _, raw := range urlInText.FindAllString(text, -1) {
				raw = strings.TrimRight(raw, ".,!?;:)]}")
				u, err := url.Parse(raw)
				if err != nil || !validURL(raw) || !p.app.C.PreviewHosts[strings.ToLower(u.Hostname())] {
					return invalid("automatic WhatsApp previews require an approved HTTPS hostname; use custom metadata or disable previews for this link")
				}
			}
		}
	}
	if limit > 0 && utf8.RuneCountInString(text) > limit {
		return invalid("platform text length limit exceeded")
	}
	if m.Size > size {
		return invalid("media exceeds configured platform size limit")
	}
	if m.ID == "" && strings.TrimSpace(text) == "" {
		return invalid("text, link or media is required")
	}
	return nil
}
func (p *platform) Publish(ctx context.Context, d Delivery, a Account, m, preview Media) (Result, error) {
	if p.kind == "whatsapp" {
		return p.whatsapp(ctx, d, a, m, preview)
	}
	credential, err := p.app.access(ctx, a)
	if err != nil {
		return Result{}, err
	}
	t := d.Target
	switch p.kind {
	case "linkedin":
		return p.linkedin(ctx, t, a, m, credential.Access)
	case "x":
		return p.x(ctx, t, m, credential.Access)
	case "instagram":
		return p.instagram(ctx, t, a, m, credential.Access)
	default:
		return Result{}, errors.New("unsupported adapter")
	}
}

func confirmed(remote string, err error) (Result, error) {
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(remote) == "" || len(remote) > 512 {
		return Result{}, unknown(true)
	}
	return Result{ID: remote}, nil
}
