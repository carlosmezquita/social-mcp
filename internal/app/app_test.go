package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testToken = "0123456789abcdef0123456789abcdef"
const testRecipient = "44123456789@c.us"
const groupRecipient = "1234-5678@g.us"

type mockAdapter func(context.Context, Delivery, Account, Media, Media) (Result, error)

func (m mockAdapter) Validate(Target, Media) error { return nil }
func (m mockAdapter) Publish(ctx context.Context, d Delivery, a Account, upload, preview Media) (Result, error) {
	return m(ctx, d, a, upload, preview)
}

func setup(t *testing.T) (*Service, Principal) {
	t.Helper()
	directory := t.TempDir()
	c := Config{DB: filepath.Join(directory, "db.sqlite"), Media: filepath.Join(directory, "media"), Public: "https://social.example.com", Listen: "127.0.0.1:8080", Key: bytes.Repeat([]byte{65}, 32), Poll: 100 * time.Millisecond, Retries: 2, WAHAKey: "test-key", WAHA: "http://127.0.0.1:3000",
		Accounts:  map[string]Account{"wa": {ID: "wa", Platform: "whatsapp", Session: "default", Video: true}, "x": {ID: "x", Platform: "x", RemoteID: "123", Video: true}, "ig": {ID: "ig", Platform: "instagram", RemoteID: "123"}, "li": {ID: "li", Platform: "linkedin", RemoteID: "urn:li:person:abc", Video: true}},
		Providers: map[string]Provider{"x": {Client: "test"}, "instagram": {Client: "test", Secret: "secret", Version: "v26.0"}, "linkedin": {Client: "test", Secret: "secret", Version: "202609"}}, PreviewHosts: map[string]bool{"example.com": true}}
	p := Principal{ID: "test-client", Publish: true, Admin: true, Accounts: map[string][]string{"wa": {testRecipient, groupRecipient}, "x": nil, "ig": nil, "li": nil}, hash: sha256.Sum256([]byte(testToken))}
	c.Principals = []Principal{p}
	s, err := Open(t.Context(), c.DB, c.Media)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Close() })
	a := New(c, s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, key := range []string{"x", "li", "ig"} {
		if err = a.Vault.save(t.Context(), c.Accounts[key], Token{Access: "test-provider-token"}); err != nil {
			t.Fatal(err)
		}
	}
	return a, p
}
func target(recipient string) Target {
	return Target{Platform: "whatsapp", Account: "wa", Recipient: recipient, Text: "Schedule me"}
}
func assertState(t *testing.T, a *Service, p Principal, key, state string) Delivery {
	t.Helper()
	ds, err := a.S.get(t.Context(), p, key)
	if err != nil || len(ds) != 1 || ds[0].State != state {
		t.Fatalf("wanted %s, got %+v (%v)", state, ds, err)
	}
	return ds[0]
}
func TestScheduleRestartDedupAndCancel(t *testing.T) {
	a, p := setup(t)
	var published atomic.Int64
	adapter := mockAdapter(func(context.Context, Delivery, Account, Media, Media) (Result, error) {
		published.Add(1)
		return Result{ID: "message-1"}, nil
	})
	a.Adapters["whatsapp"] = adapter
	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	request := Request{Key: "future", At: future, Targets: []Target{target(testRecipient)}}
	ds, err := a.submit(t.Context(), p, request, true)
	if err != nil {
		t.Fatal(err)
	}
	d := ds[0]
	earlier, err := a.S.claim(t.Context(), time.Now(), time.Minute)
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("future delivery claimed: %+v %v", earlier, err)
	}
	if ds, err = a.submit(t.Context(), p, request, true); err != nil || ds[0].ID != d.ID {
		t.Fatalf("dedup failed: %v", err)
	}
	request.Targets[0].Text = "Change"
	if _, err = a.submit(t.Context(), p, request, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict: %v", err)
	}
	if err = a.S.DB.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), a.C.DB, a.C.Media)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	a = New(a.C, reopened, a.Log)
	a.Adapters["whatsapp"] = adapter
	persisted := assertState(t, a, p, d.ID, "scheduled")
	changed := persisted.Target
	changed.Text = "Edited"
	if _, err = a.change(t.Context(), p, Edit{ID: d.ID, Target: &changed}, false); err != nil {
		t.Fatal(err)
	}
	assertState(t, a, p, d.ID, "scheduled")
	if _, err = a.change(t.Context(), p, Edit{ID: d.ID}, true); err != nil {
		t.Fatal(err)
	}
	assertState(t, a, p, d.ID, "cancelled")
	ds, err = a.submit(t.Context(), p, Request{Key: "now", Targets: []Target{target(testRecipient)}}, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go a.Run(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := a.S.get(t.Context(), p, ds[0].ID)
		if err == nil && rows[0].State == "published" {
			if published.Load() != 1 {
				t.Fatal("duplicate")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("immediate job did not publish")
}
func TestConcurrentLeaseAndAmbiguousRecovery(t *testing.T) {
	a, p := setup(t)
	also, err := Open(t.Context(), a.C.DB, a.C.Media)
	if err != nil {
		t.Fatal(err)
	}
	defer also.DB.Close()
	ds, err := a.submit(t.Context(), p, Request{Key: "lease", Targets: []Target{target(testRecipient)}}, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	var claimed atomic.Int64
	var group sync.WaitGroup
	for _, store := range []*Store{a.S, also, a.S, also} {
		group.Go(func() {
			_, err := store.claim(ctx, time.Now(), time.Minute)
			if err == nil {
				claimed.Add(1)
			} else if !errors.Is(err, ErrMissing) {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if claimed.Load() != 1 {
		t.Fatalf("claims=%d", claimed.Load())
	}
	if _, err = also.claim(ctx, time.Now().Add(2*time.Minute), time.Minute); !errors.Is(err, ErrMissing) {
		t.Fatal(err)
	}
	assertState(t, a, p, ds[0].ID, "uncertain")
	attempts, err := a.S.attempts(ctx, ds[0].ID)
	if err != nil || len(attempts) != 1 || attempts[0].Outcome != "uncertain" || attempts[0].Finish == nil {
		t.Fatalf("%+v %v", attempts, err)
	}
}

func TestRetryAndPartialCampaign(t *testing.T) {
	a, p := setup(t)
	a.Adapters["whatsapp"] = mockAdapter(func(_ context.Context, d Delivery, _ Account, _, _ Media) (Result, error) {
		if d.Target.Recipient == testRecipient {
			return Result{}, fail("ambiguous", "response lost")
		}
		if d.Tries == 1 {
			return Result{}, &Failure{Kind: "rate_limited", Message: "wait", After: 70 * time.Second}
		}
		return Result{ID: "123"}, nil
	})
	a.Adapters["x"] = mockAdapter(func(context.Context, Delivery, Account, Media, Media) (Result, error) { return Result{ID: "456"}, nil })
	ds, err := a.submit(t.Context(), p, Request{Key: "partial", Targets: []Target{target(testRecipient), target(groupRecipient), {Platform: "x", Account: "x", Text: "Same campaign"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	for range ds {
		d, err := a.S.claim(t.Context(), time.Now(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		a.execute(t.Context(), d)
	}
	for _, d := range ds {
		switch {
		case d.Target.Platform == "x":
			assertState(t, a, p, d.ID, "published")
		case d.Target.Recipient == testRecipient:
			assertState(t, a, p, d.ID, "uncertain")
		default:
			retry := assertState(t, a, p, d.ID, "retry_wait")
			if time.Until(retry.Due) < time.Minute {
				t.Fatal("retry-after ignored")
			}
		}
	}
	next, err := a.S.claim(t.Context(), time.Now().Add(2*time.Minute), time.Minute)
	if err != nil || next.Target.Recipient != groupRecipient {
		t.Fatal(next, err)
	}
	a.execute(t.Context(), next)
	assertState(t, a, p, next.ID, "published")
	if _, err = a.S.claim(t.Context(), time.Now().Add(time.Hour), time.Minute); !errors.Is(err, ErrMissing) {
		t.Fatal(err)
	}
}
func TestTimezones(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, input := range []string{"2026-10-25T01:30:00", "2026-03-29T01:30:00+01:00", "2026-07-01T09:00:00+02:00"} {
		if _, _, err := schedule(input, "Europe/London", now); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted %s", input)
		}
	}
	for _, input := range []string{"2026-10-25T01:30:00+00:00", "2026-10-25T01:30:00+01:00", "2026-10-25T10:30:00Z"} {
		if _, _, err := schedule(input, "Europe/London", now); err != nil {
			t.Error(err)
		}
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func requestAs(r *http.Request) *http.Request {
	r.Host = "social.example.com"
	r.Header.Set("Authorization", "Bearer "+testToken)
	return r
}
func TestMCPAndREST(t *testing.T) {
	a, p := setup(t)
	server := httptest.NewServer(a.HTTP())
	defer server.Close()
	h := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) { return http.DefaultTransport.RoundTrip(requestAs(r)) })}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: h}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) < 12 {
		t.Fatalf("tool discovery: %v %v", tools, err)
	}
	for recipient, wantError := range map[string]bool{testRecipient: false, "100@c.us": true} {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "publish_now", Arguments: map[string]any{"idempotency_key": id(), "targets": []Target{target(recipient)}}})
		if err != nil || result.IsError != wantError {
			t.Fatalf("tool result %+v %v", result, err)
		}
	}
	if rows, err := a.S.list(t.Context(), p, Filter{}); err != nil || len(rows) != 1 {
		t.Fatalf("durable tool call: %v %v", rows, err)
	}
	for _, item := range []struct {
		path, host, origin, token, method, body string
		status                                  int
	}{
		{"/mcp", "social.example.com", "", "", "POST", "", 401},
		{"/mcp", "evil.net", "", testToken, "POST", "", 421},
		{"/mcp", "social.example.com", "https://evil.net", testToken, "POST", "", 403},
		{"/api/publish", "social.example.com", "", testToken, "POST", strings.Repeat(" ", 270000), 400},
		{"/api/publish", "social.example.com", "", testToken, "POST", `{"bogus":1}`, 400},
		{"/readyz", "social.example.com", "", "", "GET", "", 404},
		{"/readyz", a.C.Listen, "", "", "GET", "", 200},
	} {
		r := httptest.NewRequest(item.method, item.path, strings.NewReader(item.body))
		r.Host = item.host
		if item.origin != "" {
			r.Header.Set("Origin", item.origin)
		}
		if item.token != "" {
			r.Header.Set("Authorization", "Bearer "+item.token)
		}
		w := httptest.NewRecorder()
		a.HTTP().ServeHTTP(w, r)
		if w.Code != item.status {
			t.Errorf("%s: %d want %d (%s)", r.URL, w.Code, item.status, w.Body)
		}
	}
}
func TestBackupAndEncryption(t *testing.T) {
	a, p := setup(t)
	media, err := a.S.upload(t.Context(), p, strings.NewReader("\xff\xd8\xffphotobytes"))
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err = a.S.DB.QueryRow("SELECT secret FROM credentials WHERE account='x'").Scan(&raw); err != nil || bytes.Contains(raw, []byte("test-provider-token")) {
		t.Fatal("credential encryption", err)
	}
	d, err := a.submit(t.Context(), p, Request{Key: "snapshot", Targets: []Target{{Platform: "instagram", Account: "ig", MediaID: media.ID}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	copyFile := filepath.Join(t.TempDir(), "copy.db")
	if err = a.Backup(t.Context(), copyFile); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), copyFile, a.C.Media)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	restored := New(a.C, s, a.Log)
	if err = restored.Verify(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertState(t, restored, p, d[0].ID, "scheduled")
}
func TestWAHAPayloads(t *testing.T) {
	a, p := setup(t)
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" {
			t.Error("expected WAHA API key")
		}
		switch r.URL.Path {
		case "/api/sessions/default":
			fmt.Fprint(w, `{"status":"WORKING"}`)
		case "/api/default/chats/overview":
			fmt.Fprintf(w, `[{"id":%q,"name":"Contact"},{"id":%q,"name":"Group"},{"id":"999@c.us","name":"Private"}]`, testRecipient, groupRecipient)
		case "/api/sendText", "/api/send/link-custom-preview", "/api/sendImage":
			calls = append(calls, r.URL.Path)
			var body fields
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid payload")
			}
			if r.URL.Path == "/api/sendText" && (body["linkPreview"] != true || body["linkPreviewHighQuality"] != true || !strings.Contains(body["text"].(string), "https://example.com")) {
				t.Error(body)
			}
			if r.URL.Path == "/api/send/link-custom-preview" && (body["preview"].(map[string]any)["title"] != "Expected headline" || body["chatId"] != groupRecipient) {
				t.Error(body)
			}
			if r.URL.Path == "/api/sendImage" && body["file"].(map[string]any)["data"] == "" {
				t.Error(body)
			}
			fmt.Fprint(w, `{"id":"sent-message"}`)
		case "/api/default/chats/" + testRecipient + "/messages/sent-message":
			fmt.Fprint(w, `{"ack":3,"ackName":"READ"}`)
		default:
			t.Error(r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	wa := a.Adapters["whatsapp"].(*platform)
	wa.base = server.URL
	chats, err := a.chats(t.Context(), p, "wa", 0)
	if err != nil || len(chats) != 2 || !chats[1].Group {
		t.Fatal(chats, err)
	}
	text := target(testRecipient)
	text.URL = "https://example.com/"
	custom := target(groupRecipient)
	custom.URL = "https://example.com/"
	custom.Custom = &Preview{Title: "Expected headline"}
	m, err := a.S.upload(t.Context(), p, strings.NewReader("\xff\xd8\xffphotobytes"))
	if err != nil {
		t.Fatal(err)
	}
	for i, item := range []Target{text, custom, target(groupRecipient)} {
		var media Media
		if i == 2 {
			media = m
		}
		result, err := wa.whatsapp(t.Context(), Delivery{Target: item}, a.C.Accounts["wa"], media, Media{})
		if err != nil || result.ID != "sent-message" {
			t.Fatal(result, err)
		}
	}
	if len(calls) != 3 {
		t.Fatal(calls)
	}
	ack, err := wa.ack(t.Context(), a.C.Accounts["wa"], Delivery{Target: text, ExternalID: "sent-message"})
	if err != nil || ack["ack_name"] != "READ" {
		t.Fatal(ack, err)
	}
}
func TestProviders(t *testing.T) {
	a, _ := setup(t)
	for _, name := range []string{"linkedin", "instagram", "x"} {
		t.Run(name, func(t *testing.T) {
			hit := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-provider-token" {
					t.Error("bearer missing")
				}
				switch r.URL.Path {
				case "/rest/posts":
					if r.Header.Get("LinkedIn-Version") != "202609" || r.Header.Get("X-Restli-Protocol-Version") != "2.0.0" {
						t.Error(r.Header)
					}
					var payload fields
					_ = json.NewDecoder(r.Body).Decode(&payload)
					if payload["author"] != "urn:li:person:abc" {
						t.Error(payload)
					}
					w.Header().Set("X-RestLi-Id", "urn:li:share:123")
					w.WriteHeader(201)
					hit = true
				case "/2/tweets":
					fmt.Fprint(w, `{"data":{"id":"234"}}`)
					hit = true
				case "/123/media":
					_ = r.ParseForm()
					if !strings.HasPrefix(r.Form.Get("image_url"), "https://social.example.com/public/media/") {
						t.Error("signed image URL missing")
					}
					fmt.Fprint(w, `{"id":"345"}`)
				case "/345":
					fmt.Fprint(w, `{"status_code":"FINISHED"}`)
				case "/123/media_publish":
					_ = r.ParseForm()
					if r.Form.Get("creation_id") != "345" {
						t.Error(r.Form)
					}
					fmt.Fprint(w, `{"id":"456"}`)
					hit = true
				default:
					t.Error(r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			adapter := a.Adapters[name].(*platform)
			adapter.base = server.URL
			key := map[string]string{"linkedin": "li", "instagram": "ig", "x": "x"}[name]
			m := Media{}
			if name == "instagram" {
				m = Media{ID: "test-media", MIME: "image/jpeg", Size: 20}
			}
			result, err := adapter.Publish(t.Context(), Delivery{Target: Target{Platform: name, Account: key, Text: "A test"}}, a.C.Accounts[key], m, Media{})
			if err != nil || result.ID == "" || !hit {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
}
func TestProviderFailureClassification(t *testing.T) {
	for _, test := range []struct {
		status  int
		publish bool
		kind    string
	}{
		{429, true, "rate_limited"}, {401, true, "auth"}, {403, true, "auth"}, {402, true, "permanent"}, {422, true, "permanent"}, {503, true, "ambiguous"}, {503, false, "transient"}, {200, true, "ambiguous"},
	} {
		client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: test.status, Header: http.Header{"Retry-After": {"62"}}, Body: io.NopCloser(strings.NewReader("bad body"))}, nil
		})}
		var out fields
		_, err := exchange(t.Context(), client, "POST", "https://example.com", nil, "", nil, -1, &out, test.publish)
		var f *Failure
		if !errors.As(err, &f) || f.Kind != test.kind {
			t.Errorf("%d got %v", test.status, err)
			continue
		}
		if test.status == 429 && f.After != 62*time.Second {
			t.Error(f.After)
		}
	}
}
func TestMediaSigningAndValidation(t *testing.T) {
	a, p := setup(t)
	if _, err := a.S.upload(t.Context(), p, strings.NewReader("html")); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	m, err := a.S.upload(t.Context(), p, strings.NewReader("\xff\xd8\xffimage"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(m.Path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	for _, test := range []struct {
		url    string
		status int
	}{{a.mediaURL(m.ID), 200}, {a.mediaURL(m.ID) + "bad", 404}} {
		r := httptest.NewRequest("GET", test.url, nil)
		w := httptest.NewRecorder()
		a.HTTP().ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatal(w.Code, w.Body)
		}
	}
}
