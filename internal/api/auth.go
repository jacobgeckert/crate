package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Optional single-user login. Nothing is enforced until a username+password is
// set via PUT /api/auth/credentials — LAN installs stay auth-free. Sessions
// are random tokens in cache.db (30d); a rotatable secret is stored as the
// session value so credential changes invalidate every session at once.
//
// Lidarr-compat tools (/api/v1) can't do cookie logins, so every authed path
// also accepts a matching ?apikey= / X-Api-Key — the Lidarr convention.

const (
	SettingAuthUsername = "auth_username"
	SettingAuthHash     = "auth_password_hash" // bcrypt — in hiddenSettings, managed here only
	SettingAuthSecret   = "auth_session_secret" // #nosec G101 -- settings key name, not a credential
	SettingAPIKey       = "api_key" // minted when auth is first enabled

	sessionCookie = "crate_session"
	sessionTTL    = 30 * 24 * time.Hour
)

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) authEnabled() bool {
	u, _ := s.queries.GetSetting(SettingAuthUsername)
	h, _ := s.queries.GetSetting(SettingAuthHash)
	return u != "" && h != ""
}

func (s *Server) sessionSecret() string {
	sec, _ := s.queries.GetSetting(SettingAuthSecret)
	if sec == "" {
		sec = randHex(16)
		_ = s.queries.SetSetting(SettingAuthSecret, sec)
	}
	return sec
}

// authenticated reports whether the request carries a valid session cookie or
// a matching API key. API key auth exists for Lidarr-compat tooling, but works
// on every route for convenience.
func (s *Server) authenticated(r *http.Request) bool {
	if c, err := r.Cookie(sessionCookie); err == nil && s.cache != nil {
		if v, ok := s.cache.Get("auth:session:" + c.Value); ok && string(v) == s.sessionSecret() {
			return true
		}
	}
	key := r.Header.Get("X-Api-Key")
	if key == "" {
		key = r.URL.Query().Get("apikey")
	}
	if key != "" {
		stored, _ := s.queries.GetSetting(SettingAPIKey)
		if stored != "" && subtle.ConstantTimeCompare([]byte(key), []byte(stored)) == 1 {
			return true
		}
	}
	return false
}

// requireAuth gates /api/* except the auth endpoints themselves — /api/v1 is
// covered too (Lidarr tools authenticate via ?apikey=). No-op until
// credentials exist — enabling auth is the switch, not a config flag. SPA
// assets stay public; the app renders its own login gate.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/api/auth/") ||
			!s.authEnabled() || s.authenticated(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "authentication required")
	})
}

func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	enabled := s.authEnabled()
	out := map[string]any{"enabled": enabled, "authenticated": !enabled}
	if enabled && s.authenticated(r) {
		out["authenticated"] = true
		if u, _ := s.queries.GetSetting(SettingAuthUsername); u != "" {
			out["username"] = u
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !s.authEnabled() {
		writeError(w, http.StatusBadRequest, "login not configured")
		return
	}
	storedUser, _ := s.queries.GetSetting(SettingAuthUsername)
	hash, _ := s.queries.GetSetting(SettingAuthHash)
	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(storedUser)) == 1
	passOK := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) == nil
	if !userOK || !passOK {
		time.Sleep(500 * time.Millisecond) // pace brute-force attempts
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	token := randHex(32)
	if s.cache != nil {
		s.cache.Set("auth:session:"+token, []byte(s.sessionSecret()), sessionTTL)
	}
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure is conditional on purpose: plain-http LAN installs must still log in
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge:   int(sessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": storedUser})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && s.cache != nil {
		s.cache.Delete("auth:session:" + c.Value)
	}
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- clearing cookie must expire under whatever scheme issued it
		Name:   sessionCookie,
		Path:   "/",
		MaxAge: -1,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAuthCredentials sets or clears the login. When auth is already on,
// the current password is required (the session middleware has already
// authenticated the caller — this stops a stolen session locking the owner
// out). Clearing both fields disables auth entirely.
func (s *Server) handleAuthCredentials(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentPassword string `json:"current_password"`
		Username        string `json:"username"`
		Password        string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if s.authEnabled() {
		hash, _ := s.queries.GetSetting(SettingAuthHash)
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.CurrentPassword)) != nil {
			time.Sleep(500 * time.Millisecond) // same brute-force pacing as login
			writeError(w, http.StatusForbidden, "current password is incorrect")
			return
		}
	}
	if req.Username == "" && req.Password == "" {
		_ = s.queries.SetSetting(SettingAuthUsername, "")
		_ = s.queries.SetSetting(SettingAuthHash, "")
		s.rotateSessionSecret()
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are both required")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "hash password: "+err.Error())
		return
	}
	if err := s.queries.SetSetting(SettingAuthUsername, req.Username); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.queries.SetSetting(SettingAuthHash, string(hash)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Mint an API key once — lidarr-compat tools need a credential once auth
	// is on. Rotating it is a deliberate user action via the settings field.
	if k, _ := s.queries.GetSetting(SettingAPIKey); k == "" {
		_ = s.queries.SetSetting(SettingAPIKey, randHex(16))
	}
	s.rotateSessionSecret()
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true})
}

func (s *Server) rotateSessionSecret() {
	_ = s.queries.SetSetting(SettingAuthSecret, randHex(16))
}
