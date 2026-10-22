package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/MacroXie04/Berth/internal/auth"
	"github.com/MacroXie04/Berth/internal/store"
)

var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

const testTTL = 24 * time.Hour

// clock is a settable fake time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	t     *testing.T
	h     http.Handler
	clock *clock
	store store.Store
}

// newEnv wires a Server to a real SQLite database in a temp dir and a
// minimum-cost bcrypt hasher so the suite stays fast.
func newEnv(t *testing.T, mutate ...func(*Deps)) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "berth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hasher, err := auth.NewHasher(bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: t0}
	d := Deps{
		Store:             st,
		Hasher:            hasher,
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:               clk.Now,
		SessionTTL:        testTTL,
		DefaultMemLimitMB: 2048,
		DefaultCPULimit:   2.0,
	}
	for _, m := range mutate {
		m(&d)
	}
	return &env{t: t, h: New(d).Handler(), clock: clk, store: d.Store}
}

type resp struct {
	*httptest.ResponseRecorder
}

func (r resp) json() map[string]any {
	var m map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &m); err != nil {
		r.Result().Body.Close()
		panic("response is not a JSON object: " + r.Body.String())
	}
	return m
}

func (r resp) sessionCookie() *http.Cookie {
	for _, c := range r.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	return nil
}

// do issues a request. body "" means no body; JSON content type is set for
// non-empty bodies unless the caller overrides it.
func (e *env) do(method, path, body string, hdr map[string]string, cookies ...*http.Cookie) resp {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return resp{rec}
}

func creds(user, pw string) string {
	b, _ := json.Marshal(map[string]string{"username": user, "password": pw})
	return string(b)
}

func (e *env) register(user, pw string) resp {
	e.t.Helper()
	return e.do("POST", "/api/auth/register", creds(user, pw), nil)
}

// login registers nothing; it logs in and returns the session cookie.
func (e *env) login(user, pw string) *http.Cookie {
	e.t.Helper()
	r := e.do("POST", "/api/auth/login", creds(user, pw), nil)
	if r.Code != 200 {
		e.t.Fatalf("login %s: %d %s", user, r.Code, r.Body)
	}
	c := r.sessionCookie()
	if c == nil {
		e.t.Fatal("login set no session cookie")
	}
	return c
}

func (e *env) registerAndLogin(user, pw string) *http.Cookie {
	e.t.Helper()
	if r := e.register(user, pw); r.Code != 201 {
		e.t.Fatalf("register %s: %d %s", user, r.Code, r.Body)
	}
	return e.login(user, pw)
}

func TestHealthz(t *testing.T) {
	r := newEnv(t).do("GET", "/healthz", "", nil)
	if r.Code != 200 {
		t.Fatalf("status %d", r.Code)
	}
}

func TestRegister(t *testing.T) {
	e := newEnv(t)

	r := e.register("alice", "correct horse")
	if r.Code != 201 {
		t.Fatalf("status %d: %s", r.Code, r.Body)
	}
	body := r.json()
	user := body["user"].(map[string]any)
	if user["username"] != "alice" || user["is_admin"] != true {
		t.Errorf("first user should be admin: %v", user)
	}
	ws := body["workspace"].(map[string]any)
	if ws["state"] != "offline" || ws["mem_limit_mb"] != float64(2048) || ws["cpu_limit"] != float64(2) {
		t.Errorf("unexpected workspace: %v", ws)
	}
	if r.sessionCookie() != nil {
		t.Error("register must not log the user in")
	}
	for _, leak := range []string{"password", "hash", "$2a$", "$2b$"} {
		if strings.Contains(r.Body.String(), leak) {
			t.Errorf("response leaks %q: %s", leak, r.Body)
		}
	}

	r = e.register("bob", "battery staple")
	if r.Code != 201 || r.json()["user"].(map[string]any)["is_admin"] != false {
		t.Errorf("second user must not be admin: %d %s", r.Code, r.Body)
	}

	// The workspace row must carry the volume name from the naming scheme.
	u, _ := e.store.UserByUsername(context.Background(), "alice")
	w, err := e.store.WorkspaceByUserID(context.Background(), u.ID)
	if err != nil || w.VolumeName != "berth-data-alice" || w.State != store.StateOffline {
		t.Errorf("workspace = %+v, %v", w, err)
	}
	// The stored hash is bcrypt, never the plaintext.
	if u.PasswordHash == "correct horse" || !strings.HasPrefix(u.PasswordHash, "$2") {
		t.Errorf("stored hash = %q", u.PasswordHash)
	}
}

func TestRegisterAppliesConfiguredWorkspaceDefaults(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.DefaultMemLimitMB = 512; d.DefaultCPULimit = 0.5 })
	if r := e.register("alice", "correct horse"); r.Code != 201 {
		t.Fatal(r.Body)
	}
	u, _ := e.store.UserByUsername(context.Background(), "alice")
	w, _ := e.store.WorkspaceByUserID(context.Background(), u.ID)
	if w.MemLimitMB != 512 || w.CPULimit != 0.5 {
		t.Errorf("limits = %d MB / %v CPUs, want 512 / 0.5", w.MemLimitMB, w.CPULimit)
	}
}

func TestRegisterRejections(t *testing.T) {
	e := newEnv(t)
	e.register("alice", "correct horse")

	cases := []struct {
		name   string
		user   string
		pw     string
		status int
	}{
		{"duplicate", "alice", "another pass", 409},
		{"uppercase username", "Alice2", "correct horse", 400},
		{"path chars in username", "al/ice", "correct horse", 400},
		{"dot in username", "al.ice", "correct horse", 400},
		{"too short username", "ab", "correct horse", 400},
		{"empty username", "", "correct horse", 400},
		{"short password", "carol", "short", 400},
		{"empty password", "carol", "", 400},
		{"73-byte password", "carol", strings.Repeat("a", 73), 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := e.register(tc.user, tc.pw)
			if r.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", r.Code, tc.status, r.Body)
			}
			if _, ok := r.json()["error"]; !ok {
				t.Errorf("no error field: %s", r.Body)
			}
		})
	}
	if r := e.register("dave", strings.Repeat("a", 72)); r.Code != 201 {
		t.Errorf("72-byte password should be accepted: %d %s", r.Code, r.Body)
	}
}

func TestRequestBodyHandling(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name   string
		body   string
		ctype  string
		status int
	}{
		{"wrong content type", creds("carol", "correct horse"), "application/x-www-form-urlencoded", 415},
		{"text/plain (CSRF-able)", creds("carol", "correct horse"), "text/plain", 415},
		{"missing content type", creds("carol", "correct horse"), "", 415},
		{"charset param ok", creds("carol", "correct horse"), "application/json; charset=utf-8", 201},
		{"malformed json", `{"username":`, "application/json", 400},
		{"not an object", `["a"]`, "application/json", 400},
		{"unknown field", `{"username":"dave","password":"correct horse","is_admin":true}`, "application/json", 400},
		{"trailing data", creds("erin", "correct horse") + `{"x":1}`, "application/json", 400},
		{"oversized", `{"username":"frank","password":"` + strings.Repeat("a", 10_000) + `"}`, "application/json", 413},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/auth/register", strings.NewReader(tc.body))
			if tc.ctype != "" {
				req.Header.Set("Content-Type", tc.ctype)
			}
			rec := httptest.NewRecorder()
			e.h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
		})
	}
	// The mass-assignment attempt above must not have created an admin.
	if _, err := e.store.UserByUsername(context.Background(), "dave"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("user created from a rejected request: %v", err)
	}
}

func TestLogin(t *testing.T) {
	e := newEnv(t)
	e.register("alice", "correct horse")

	r := e.do("POST", "/api/auth/login", creds("alice", "correct horse"), nil)
	if r.Code != 200 {
		t.Fatalf("status %d: %s", r.Code, r.Body)
	}
	c := r.sessionCookie()
	if c == nil || len(c.Value) != 43 {
		t.Fatalf("session cookie = %+v", c)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Secure {
		t.Errorf("cookie attributes wrong: %+v", c)
	}
	if c.MaxAge != int(testTTL.Seconds()) || !c.Expires.Equal(t0.Add(testTTL)) {
		t.Errorf("cookie lifetime: MaxAge=%d Expires=%v", c.MaxAge, c.Expires)
	}
	if u := r.json()["user"].(map[string]any); u["username"] != "alice" {
		t.Errorf("body: %s", r.Body)
	}
	// The raw token is never stored: only its hash.
	u, _ := e.store.SessionUser(context.Background(), auth.HashToken(c.Value), t0)
	if u.Username != "alice" {
		t.Errorf("session not stored under hash of token: %+v", u)
	}
	if _, err := e.store.SessionUser(context.Background(), c.Value, t0); !errors.Is(err, store.ErrNotFound) {
		t.Error("session is retrievable by the raw token: token stored unhashed?")
	}
}

func TestLoginSecureCookieWhenConfigured(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.CookieSecure = true })
	e.register("alice", "correct horse")
	c := e.login("alice", "correct horse")
	if !c.Secure {
		t.Error("cookie should be Secure when BERTH_COOKIE_SECURE=true")
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	e := newEnv(t)
	e.register("alice", "correct horse")

	wrongPW := e.do("POST", "/api/auth/login", creds("alice", "wrong password"), nil)
	noUser := e.do("POST", "/api/auth/login", creds("nobody", "wrong password"), nil)
	badName := e.do("POST", "/api/auth/login", creds("Not/A/Name", "wrong password"), nil)
	empty := e.do("POST", "/api/auth/login", creds("", ""), nil)

	for name, r := range map[string]resp{"wrong pw": wrongPW, "no user": noUser, "bad name": badName, "empty": empty} {
		if r.Code != 401 {
			t.Errorf("%s: status %d, want 401", name, r.Code)
		}
		if r.sessionCookie() != nil {
			t.Errorf("%s: set a session cookie", name)
		}
		if r.Body.String() != wrongPW.Body.String() {
			t.Errorf("%s: body differs from wrong-password body (user enumeration):\n%s\nvs\n%s", name, r.Body, wrongPW.Body)
		}
	}
}

// bcrypt truncates at 72 bytes; a login that appends junk to the real
// password must not authenticate.
func TestLoginRejectsPasswordWithMatchingPrefix(t *testing.T) {
	e := newEnv(t)
	pw := strings.Repeat("a", 72)
	e.register("alice", pw)
	if r := e.do("POST", "/api/auth/login", creds("alice", pw+"junk"), nil); r.Code != 401 {
		t.Fatalf("status %d, want 401", r.Code)
	}
	if r := e.do("POST", "/api/auth/login", creds("alice", pw), nil); r.Code != 200 {
		t.Fatalf("status %d, want 200", r.Code)
	}
}

func TestLoginRotatesSession(t *testing.T) {
	e := newEnv(t)
	e.register("alice", "correct horse")
	first := e.login("alice", "correct horse")

	r := e.do("POST", "/api/auth/login", creds("alice", "correct horse"), nil, first)
	second := r.sessionCookie()
	if second == nil || second.Value == first.Value {
		t.Fatal("login did not issue a fresh token")
	}
	if e.do("GET", "/api/me", "", nil, first).Code != 401 {
		t.Error("the session presented at login was not retired")
	}
	if e.do("GET", "/api/me", "", nil, second).Code != 200 {
		t.Error("new session does not work")
	}
}

func TestMultipleSessionsCoexist(t *testing.T) {
	e := newEnv(t)
	e.register("alice", "correct horse")
	laptop := e.login("alice", "correct horse")
	phone := e.login("alice", "correct horse")
	e.do("POST", "/api/auth/logout", "", nil, laptop)
	if e.do("GET", "/api/me", "", nil, laptop).Code != 401 {
		t.Error("logged-out session still valid")
	}
	if e.do("GET", "/api/me", "", nil, phone).Code != 200 {
		t.Error("logging out one device logged out the other")
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t)
	c := e.registerAndLogin("alice", "correct horse")

	r := e.do("POST", "/api/auth/logout", "", nil, c)
	if r.Code != 204 {
		t.Fatalf("status %d", r.Code)
	}
	cleared := r.sessionCookie()
	if cleared == nil || cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Errorf("cookie not cleared: %+v", cleared)
	}
	// The old token must be dead server-side, not merely forgotten by the browser.
	if e.do("GET", "/api/me", "", nil, c).Code != 401 {
		t.Error("session still valid after logout (replayed cookie)")
	}
	if code := e.do("GET", "/api/auth/check", "", map[string]string{"X-Forwarded-Uri": "/ws/alice/terminal"}, c).Code; code != 401 {
		t.Errorf("ForwardAuth accepted a logged-out session: %d", code)
	}
	// Logging out with no session is harmless.
	if r := e.do("POST", "/api/auth/logout", "", nil); r.Code != 204 {
		t.Errorf("logout without cookie: %d", r.Code)
	}
}

func TestMe(t *testing.T) {
	e := newEnv(t)
	c := e.registerAndLogin("alice", "correct horse")

	r := e.do("GET", "/api/me", "", nil, c)
	if r.Code != 200 {
		t.Fatalf("status %d: %s", r.Code, r.Body)
	}
	body := r.json()
	if body["user"].(map[string]any)["username"] != "alice" || body["workspace"].(map[string]any)["state"] != "offline" {
		t.Errorf("body: %s", r.Body)
	}

	for name, cs := range map[string][]*http.Cookie{
		"no cookie":       nil,
		"garbage cookie":  {{Name: sessionCookieName, Value: "garbage"}},
		"empty cookie":    {{Name: sessionCookieName, Value: ""}},
		"other cookie":    {{Name: "session", Value: c.Value}},
		"truncated token": {{Name: sessionCookieName, Value: c.Value[:20]}},
	} {
		if code := e.do("GET", "/api/me", "", nil, cs...).Code; code != 401 {
			t.Errorf("%s: status %d, want 401", name, code)
		}
	}
}

func TestSessionExpiry(t *testing.T) {
	e := newEnv(t)
	c := e.registerAndLogin("alice", "correct horse")
	fwd := map[string]string{"X-Forwarded-Uri": "/ws/alice/terminal"}

	e.clock.Advance(testTTL - time.Second)
	if e.do("GET", "/api/me", "", nil, c).Code != 200 || e.do("GET", "/api/auth/check", "", fwd, c).Code != 200 {
		t.Fatal("session should still be valid just before expiry")
	}
	e.clock.Advance(time.Second)
	if e.do("GET", "/api/me", "", nil, c).Code != 401 || e.do("GET", "/api/auth/check", "", fwd, c).Code != 401 {
		t.Fatal("session should be expired at its expiry instant")
	}
}

func TestLoginPurgesExpiredSessions(t *testing.T) {
	e := newEnv(t)
	e.register("alice", "correct horse")
	e.login("alice", "correct horse")
	e.clock.Advance(testTTL + time.Hour)
	e.login("alice", "correct horse") // the first session is now expired

	// The expired row is gone, not merely ignored: nothing is left to purge.
	n, err := e.store.DeleteExpiredSessions(context.Background(), e.clock.Now())
	if err != nil || n != 0 {
		t.Errorf("login left %d expired session row(s) behind (err %v)", n, err)
	}
}

func TestCheck(t *testing.T) {
	e := newEnv(t)
	alice := e.registerAndLogin("alice", "correct horse")
	bob := e.registerAndLogin("bob", "battery staple")

	cases := []struct {
		name    string
		uri     string
		cookies []*http.Cookie
		want    int
	}{
		{"owner terminal", "/ws/alice/terminal", []*http.Cookie{alice}, 200},
		{"owner terminal deep path", "/ws/alice/terminal/ws?token=1", []*http.Cookie{alice}, 200},
		{"owner browser", "/ws/alice/browser/", []*http.Cookie{alice}, 200},
		{"other user's terminal", "/ws/bob/terminal", []*http.Cookie{alice}, 403},
		{"other user's browser", "/ws/bob/browser", []*http.Cookie{alice}, 403},
		{"bob owns bob's", "/ws/bob/terminal", []*http.Cookie{bob}, 200},
		{"no cookie", "/ws/alice/terminal", nil, 401},
		{"garbage cookie", "/ws/alice/terminal", []*http.Cookie{{Name: sessionCookieName, Value: "x"}}, 401},
		{"no cookie and bad path is still 401", "/nonsense", nil, 401},
		{"traversal to other user", "/ws/alice/../bob/terminal", []*http.Cookie{alice}, 403},
		{"encoded traversal", "/ws/alice/%2e%2e/bob/terminal", []*http.Cookie{alice}, 403},
		{"encoded slash traversal", "/ws/alice%2F..%2Fbob/terminal", []*http.Cookie{alice}, 403},
		{"prefix-only match", "/ws/alice/terminalx", []*http.Cookie{alice}, 403},
		{"unknown section", "/ws/alice/admin", []*http.Cookie{alice}, 403},
		{"not a workspace path", "/api/me", []*http.Cookie{alice}, 403},
		{"empty uri", "", []*http.Cookie{alice}, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hdr := map[string]string{}
			if tc.uri != "" {
				hdr["X-Forwarded-Uri"] = tc.uri
			}
			r := e.do("GET", "/api/auth/check", "", hdr, tc.cookies...)
			if r.Code != tc.want {
				t.Fatalf("status %d, want %d (body %s)", r.Code, tc.want, r.Body)
			}
		})
	}
}

// The check endpoint must not be steerable through other forwarded headers:
// only X-Forwarded-Uri (which Traefik overwrites) counts.
func TestCheckIgnoresOtherHeaders(t *testing.T) {
	e := newEnv(t)
	alice := e.registerAndLogin("alice", "correct horse")
	e.registerAndLogin("bob", "battery staple")
	r := e.do("GET", "/api/auth/check?user=alice", "", map[string]string{
		"X-Forwarded-Uri": "/ws/bob/terminal",
		"X-Original-URL":  "/ws/alice/terminal",
		"X-Forwarded-For": "alice",
	}, alice)
	if r.Code != 403 {
		t.Fatalf("status %d, want 403", r.Code)
	}
}

// failingStore makes session lookups error, as a locked or broken database would.
type failingStore struct{ store.Store }

func (failingStore) SessionUser(context.Context, string, time.Time) (store.User, error) {
	return store.User{}, errors.New("database is on fire")
}

// An infrastructure error must be a 5xx, never a 401: a 401 would make the UI
// believe the user logged out; and ForwardAuth must still fail closed.
func TestStoreErrorsAreServerErrorsAndFailClosed(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Store = failingStore{d.Store} })
	c := &http.Cookie{Name: sessionCookieName, Value: "whatever"}
	if code := e.do("GET", "/api/me", "", nil, c).Code; code != 500 {
		t.Errorf("/api/me status %d, want 500", code)
	}
	r := e.do("GET", "/api/auth/check", "", map[string]string{"X-Forwarded-Uri": "/ws/alice/terminal"}, c)
	if r.Code != 500 {
		t.Errorf("/api/auth/check status %d, want 500", r.Code)
	}
}

func TestCheckRequiresGET(t *testing.T) {
	e := newEnv(t)
	if code := e.do("POST", "/api/auth/check", "", nil).Code; code != 405 {
		t.Errorf("POST /check status %d, want 405", code)
	}
}

func TestAPIResponseHeaders(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/me"}, {"POST", "/api/auth/login"}, {"GET", "/api/auth/check"}, {"GET", "/api/nope"},
	} {
		h := e.do(tc.method, tc.path, "", nil).Header()
		if h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s: headers %v", tc.method, tc.path, h)
		}
	}
}

func TestNotFoundAndMethodNotAllowedAreJSON(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/api/nope", 404}, {"GET", "/nope", 404}, {"GET", "/api/auth/login", 405}, {"DELETE", "/api/me", 405},
	} {
		r := e.do(tc.method, tc.path, "", nil)
		if r.Code != tc.status || !strings.HasPrefix(r.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s %s: %d %q", tc.method, tc.path, r.Code, r.Header().Get("Content-Type"))
		}
	}
}

func TestPanicBecomesJSON500(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Store = panicStore{d.Store} })
	r := e.do("POST", "/api/auth/login", creds("alice", "correct horse"), nil)
	if r.Code != 500 || r.json()["error"] != "internal error" {
		t.Fatalf("status %d body %s", r.Code, r.Body)
	}
}

type panicStore struct{ store.Store }

func (panicStore) UserByUsername(context.Context, string) (store.User, error) { panic("boom") }

// The cookie that clears a session must carry the same Path as the one that set
// it, or the browser keeps the old cookie. The other attributes are pinned too.
func TestLogoutCookieAttributes(t *testing.T) {
	for _, secure := range []bool{false, true} {
		e := newEnv(t, func(d *Deps) { d.CookieSecure = secure })
		c := e.registerAndLogin("alice", "correct horse")
		got := e.do("POST", "/api/auth/logout", "", nil, c).sessionCookie()
		if got == nil {
			t.Fatal("no clearing cookie")
		}
		if got.Name != sessionCookieName || got.Path != "/" || !got.HttpOnly ||
			got.SameSite != http.SameSiteLaxMode || got.Secure != secure || got.MaxAge >= 0 {
			t.Errorf("secure=%v: clearing cookie = %+v", secure, got)
		}
	}
}

// Login must cost about the same whether or not the account exists and whether
// or not the password is overlong; otherwise response time reveals usernames.
// The minimum over a few trials is compared, which ignores scheduler noise.
func TestLoginTimingDoesNotRevealUsernames(t *testing.T) {
	// Cost 7 (~8ms per compare) is plenty: the test compares ratios of per-path minima,
	// and a path that skips bcrypt answers in well under 1ms.
	h, err := auth.NewHasher(7)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(d *Deps) { d.Hasher = h })
	e.register("alice", "correct horse")

	timeLogin := func(user, pw string) time.Duration {
		best := time.Hour
		for i := 0; i < 3; i++ {
			start := time.Now()
			if r := e.do("POST", "/api/auth/login", creds(user, pw), nil); r.Code != 401 {
				t.Fatalf("login(%s, %d bytes) = %d, want 401", user, len(pw), r.Code)
			}
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}
	long := strings.Repeat("x", 200)
	reference := timeLogin("alice", "wrong password") // existing user, normal length
	for name, d := range map[string]time.Duration{
		"unknown user":               timeLogin("nobody", "wrong password"),
		"existing user, overlong pw": timeLogin("alice", long),
		"unknown user, overlong pw":  timeLogin("nobody", long),
	} {
		if d < reference/2 {
			t.Errorf("%s answered in %v, under half of the %v a real compare takes: timing reveals account existence", name, d, reference)
		}
	}
}

func TestRequestIDIsServerGenerated(t *testing.T) {
	var logs bytes.Buffer
	e := newEnv(t, func(d *Deps) { d.Log = slog.New(slog.NewTextHandler(&logs, nil)) })

	forged := strings.Repeat("A", 8000)
	r := e.do("GET", "/api/nope", "", map[string]string{"X-Request-Id": forged})
	id := r.Header().Get("X-Request-Id")
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("X-Request-Id = %q, want 12 hex chars generated by the server", id)
	}
	if strings.Contains(logs.String(), "AAAA") {
		t.Fatal("client-supplied X-Request-Id reached the log")
	}
	if !strings.Contains(logs.String(), "request_id="+id) {
		t.Fatalf("access log does not carry the server-generated id %q:\n%s", id, logs.String())
	}
	other := e.do("GET", "/api/nope", "", nil).Header().Get("X-Request-Id")
	if other == id {
		t.Fatal("two requests got the same id")
	}
}

func TestWriteJSONEncodingFailureIsA500NotAnEmptySuccess(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusCreated, map[string]any{"cpu_limit": math.Inf(1)}) // not encodable as JSON
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "internal error") {
		t.Fatalf("status %d body %q, want a 500 error body", rec.Code, rec.Body)
	}
}

func TestNewHTTPServerLimits(t *testing.T) {
	srv := NewHTTPServer(":0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Errorf("a server timeout is unset (slowloris protection): %+v", srv)
	}
	if srv.MaxHeaderBytes <= 0 || srv.MaxHeaderBytes > 64<<10 {
		t.Errorf("MaxHeaderBytes = %d, want a small explicit cap", srv.MaxHeaderBytes)
	}
}
