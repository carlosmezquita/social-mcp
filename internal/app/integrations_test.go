package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func mockReply(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: header}
}

func TestOAuthBrowserStateAndPKCE(t *testing.T) {
	a, _ := setup(t)
	completed := false
	a.Adapters["x"].(*platform).http = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/2/oauth2/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.PostForm.Get("code") != "code" || len(r.PostForm.Get("code_verifier")) < 43 || r.PostForm.Get("redirect_uri") != a.C.Public+"/oauth/x/callback" {
				t.Error(r.PostForm)
			}
			return mockReply(200, `{"access_token":"oauth-verified-access","refresh_token":"refresh","expires_in":3600}`, nil), nil
		case "/2/users/me":
			if r.Header.Get("Authorization") != "Bearer oauth-verified-access" {
				t.Error("wrong token used to verify identity")
			}
			completed = true
			return mockReply(200, `{"data":{"id":"123"}}`, nil), nil
		default:
			return mockReply(404, "", nil), nil
		}
	})}
	handler := a.HTTP()
	req := requestAs(httptest.NewRequest("POST", a.C.Public+"/connect/x", nil))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var login struct{ URL string }
	if err := json.Unmarshal(w.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", login.URL, nil))
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil || len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || loc.Query().Get("code_challenge_method") != "S256" {
		t.Fatal(cookies, loc, err)
	}
	callback := a.C.Public + "/oauth/x/callback?code=code&state=" + url.QueryEscape(loc.Query().Get("state"))
	without := httptest.NewRecorder()
	handler.ServeHTTP(without, httptest.NewRequest("GET", callback, nil))
	if without.Code != 400 {
		t.Fatal(without.Code)
	}
	finish := func() int {
		r := httptest.NewRequest("GET", callback, nil)
		r.AddCookie(cookies[0])
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if finish() != 200 || !completed {
		t.Fatal("callback failed")
	}
	if finish() != 400 {
		t.Fatal("OAuth state was replayable")
	}
	token, err := a.Vault.read(t.Context(), a.C.Accounts["x"])
	if err != nil || token.Access != "oauth-verified-access" || token.Refresh != "refresh" {
		t.Fatalf("bad token: %v", err)
	}
}

func TestMCPIntrospection(t *testing.T) {
	a, p := setup(t)
	a.C.Principals[0].Subject = "bot"
	a.C.Auth = ResourceAuth{Issuer: "https://identity.example.com", Introspection: "https://identity.example.com/introspection", Client: "id", Secret: "secret"}
	for _, audience := range []string{a.C.Public + "/mcp", "https://another.example.com/mcp"} {
		a.authClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != a.C.Auth.Introspection {
				t.Fatal(r.URL)
			}
			user, pass, ok := r.BasicAuth()
			if !ok || user != "id" || pass != "secret" {
				t.Fatal("wrong credentials for introspection")
			}
			return mockReply(200, fmt.Sprintf(`{"active":true,"iss":"https://identity.example.com","sub":"bot","scope":"social:mcp","aud":[%q],"exp":%d}`, audience, time.Now().Add(time.Hour).Unix()), nil), nil
		})}
		req := httptest.NewRequest("GET", a.C.Public+"/api/accounts", nil)
		req.Header.Set("Authorization", "Bearer external-token-of-sufficient-length")
		principal, ok := a.authenticate(req)
		if ok != (audience == a.C.Public+"/mcp") || ok && principal.ID != p.ID {
			t.Fatal(audience, principal, ok)
		}
	}
}

func TestProviderMediaFlows(t *testing.T) {
	a, p := setup(t)
	waPath := a.C.Media + "/video"
	if err := os.WriteFile(waPath, []byte("small mocked asset"), 0600); err != nil {
		t.Fatal(err)
	}
	media := Media{ID: "video", MIME: "video/mp4", Size: 18, Path: waPath}
	tests := []struct {
		platform, key string
		paths         []string
	}{
		{"x", "x", []string{"/2/media/upload/initialize", "/2/media/upload/789/append", "/2/media/upload/789/finalize", "/2/tweets"}},
		{"linkedin", "li", []string{"/rest/videos", "/part", "/rest/videos", "/rest/videos/urn:li:video:ABC", "/rest/posts"}},
	}
	for _, tc := range tests {
		t.Run(tc.platform, func(t *testing.T) {
			var paths []string
			adapter := a.Adapters[tc.platform].(*platform)
			adapter.http = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				paths = append(paths, r.URL.Path)
				switch r.URL.Path {
				case "/2/media/upload/initialize":
					data, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(data), `"media_category":"tweet_video"`) {
						t.Error(string(data))
					}
					return mockReply(200, `{"data":{"id":"789"}}`, nil), nil
				case "/2/media/upload/789/append":
					if err := r.ParseMultipartForm(1 << 20); err != nil || r.FormValue("segment_index") != "0" {
						t.Error(err)
					}
					return mockReply(204, "", nil), nil
				case "/2/media/upload/789/finalize":
					return mockReply(200, `{"data":{"id":"789","processing_info":{"state":"succeeded"}}}`, nil), nil
				case "/2/tweets":
					return mockReply(201, `{"data":{"id":"567"}}`, nil), nil
				case "/rest/videos":
					switch r.URL.Query().Get("action") {
					case "initializeUpload":
						return mockReply(200, `{"value":{"video":"urn:li:video:ABC","uploadToken":"tok","uploadInstructions":[{"uploadUrl":"https://www.linkedin.com/part","firstByte":0,"lastByte":17}]}}`, nil), nil
					case "finalizeUpload":
						body, _ := io.ReadAll(r.Body)
						if !strings.Contains(string(body), `"uploadedPartIds":["etag"]`) {
							t.Error(string(body))
						}
						return mockReply(200, "{}", nil), nil
					}
				case "/part":
					data, _ := io.ReadAll(r.Body)
					if r.Method != "PUT" || r.ContentLength != media.Size || len(data) != int(media.Size) {
						t.Error("bad video part")
					}
					return mockReply(200, "", http.Header{"Etag": {`"etag"`}}), nil
				case "/rest/videos/urn:li:video:ABC":
					return mockReply(200, `{"status":"AVAILABLE"}`, nil), nil
				case "/rest/posts":
					return mockReply(201, "", http.Header{"X-Restli-Id": {"urn:li:share:123"}}), nil
				}
				return mockReply(404, "", nil), nil
			})}
			target := Target{Platform: tc.platform, Account: tc.key, Text: "Video test"}
			if _, _, err := a.validate(t.Context(), p, target); err != nil {
				t.Fatal(err)
			}
			result, err := adapter.Publish(t.Context(), Delivery{Target: target}, a.C.Accounts[tc.key], media, Media{})
			if err != nil || result.ID == "" || !slices.Equal(paths, tc.paths) {
				t.Fatal(result, err, paths)
			}
		})
	}
}
func TestFailedUploadStopsPublish(t *testing.T) {
	a, _ := setup(t)
	adapter := a.Adapters["instagram"].(*platform)
	var count int
	adapter.http = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		count++
		if r.URL.Path != "/v26.0/123/media" {
			t.Error(r.URL)
		}
		return mockReply(422, "", nil), nil
	})}
	_, err := adapter.Publish(t.Context(), Delivery{Target: Target{Platform: "instagram", Account: "ig"}}, a.C.Accounts["ig"], Media{ID: "image", MIME: "image/jpeg"}, Media{})
	if err == nil || count != 1 {
		t.Fatal(err, count)
	}
}
func TestDoNotPublishAfterAccountRebind(t *testing.T) {
	a, p := setup(t)
	rows, err := a.submit(t.Context(), p, Request{Key: "binding-test", Targets: []Target{target(testRecipient)}}, false)
	if err != nil {
		t.Fatal(err)
	}
	a.C.Accounts["wa"] = Account{ID: "wa", Platform: "whatsapp", Session: "changed"}
	a.Adapters["whatsapp"] = mockAdapter(func(context.Context, Delivery, Account, Media, Media) (Result, error) {
		t.Fatal("rebound account was allowed to execute existing delivery")
		return Result{}, nil
	})
	d, err := a.S.claim(t.Context(), time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	a.execute(t.Context(), d)
	assertState(t, a, p, rows[0].ID, "failed")
}
func TestConcurrentIdempotencyAndRestoreQuarantine(t *testing.T) {
	a, p := setup(t)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if _, err := a.submit(t.Context(), p, Request{Key: "retry-together", Targets: []Target{target(testRecipient)}}, false); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	ds, err := a.S.list(t.Context(), p, Filter{})
	if err != nil || len(ds) != 1 {
		t.Fatal(ds, err)
	}
	if _, err = a.S.claim(t.Context(), time.Now(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = a.S.QuarantineRestored(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertState(t, a, p, ds[0].ID, "uncertain")
}
