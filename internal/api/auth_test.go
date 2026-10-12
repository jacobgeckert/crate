package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func authedDo(t *testing.T, e *testEnv, method, path, cookie string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	w := httptest.NewRecorder()
	e.server.ServeHTTP(w, req)
	e.server.WaitBackground()
	return w.Code
}

func TestAuthFlow(t *testing.T) {
	e := newTestEnv(t)

	// Open by default.
	if code := authedDo(t, e, http.MethodGet, "/api/status", ""); code != http.StatusOK {
		t.Fatalf("no-auth /api/status = %d", code)
	}

	// Enable login.
	w := e.do(http.MethodPut, "/api/auth/credentials", `{"username":"me","password":"hunter2"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("enable credentials = %d: %s", w.Code, w.Body)
	}

	// Everything but /api/auth/* is now gated.
	if code := authedDo(t, e, http.MethodGet, "/api/status", ""); code != http.StatusUnauthorized {
		t.Fatalf("gated /api/status = %d", code)
	}
	if code := authedDo(t, e, http.MethodGet, "/api/v1/system/status", ""); code != http.StatusUnauthorized {
		t.Fatalf("gated /api/v1 = %d", code)
	}

	// API key path — minted on first enable; lidarr tools use ?apikey=.
	settings, _ := e.queries.AllSettings()
	apiKey := settings["api_key"]
	if apiKey == "" {
		t.Fatal("no api_key minted on enable")
	}
	if code := authedDo(t, e, http.MethodGet, "/api/status?apikey="+apiKey, ""); code != http.StatusOK {
		t.Fatalf("apikey /api/status = %d", code)
	}

	// Bad login → 401; good login → session cookie.
	if w := e.do(http.MethodPost, "/api/auth/login", `{"username":"me","password":"wrong"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad login = %d", w.Code)
	}
	w = e.do(http.MethodPost, "/api/auth/login", `{"username":"me","password":"hunter2"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", w.Code, w.Body)
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "crate_session" {
			session = c
		}
	}
	if session == nil || session.Value == "" {
		t.Fatal("no crate_session cookie on login")
	}
	if code := authedDo(t, e, http.MethodGet, "/api/status", "crate_session="+session.Value); code != http.StatusOK {
		t.Fatalf("session /api/status = %d", code)
	}

	// Credential change requires the current password — and rotates sessions.
	if w := e.do(http.MethodPut, "/api/auth/credentials", `{"current_password":"nope","username":"me","password":"new"}`); w.Code != http.StatusForbidden {
		t.Fatalf("cred change w/o current pw = %d", w.Code)
	}
	if w := e.do(http.MethodPut, "/api/auth/credentials", `{"current_password":"hunter2","username":"me","password":"new"}`); w.Code != http.StatusOK {
		t.Fatalf("cred change = %d: %s", w.Code, w.Body)
	}
	if code := authedDo(t, e, http.MethodGet, "/api/status", "crate_session="+session.Value); code != http.StatusUnauthorized {
		t.Fatalf("rotated session should be dead, got %d", code)
	}

	// Disable (requires current password while enabled) → open again.
	if w := e.do(http.MethodPut, "/api/auth/credentials", `{"current_password":"new"}`); w.Code != http.StatusOK {
		t.Fatalf("disable = %d: %s", w.Code, w.Body)
	}
	if code := authedDo(t, e, http.MethodGet, "/api/status", ""); code != http.StatusOK {
		t.Fatalf("post-disable /api/status = %d", code)
	}
}
