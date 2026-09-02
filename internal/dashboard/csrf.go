package dashboard

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
)

// CSRF: a browser sends Basic auth without a consent step, so every POST
// under /dashboard/action/ additionally requires the X-CSRF-Token header to
// equal the elpulpo_csrf cookie (double-submit marker, SameSite=Strict).
// Pages embed the token in <body hx-headers> so every htmx request carries
// it; GETs never mutate and /api/* is read-only.

const csrfCookie = "elpulpo_csrf"

func validCSRFToken(v string) bool {
	if len(v) != 32 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

// ensureCSRF returns the request's CSRF token, issuing a fresh 32-hex token
// via cookie when absent. Called on every dashboard GET.
func (h *Handler) ensureCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && validCSRFToken(c.Value) {
		return c.Value
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand cannot realistically fail; render without a marker
		// rather than failing the page — mutations will 403 until retried.
		h.d.Log.Error("csrf token generation failed", "err", err)
		return ""
	}
	tok := hex.EncodeToString(b[:])
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	return tok
}

// checkCSRF verifies the double-submit marker with a constant-time compare.
func (h *Handler) checkCSRF(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || !validCSRFToken(c.Value) {
		return false
	}
	got := r.Header.Get("X-CSRF-Token")
	if !validCSRFToken(got) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(got)) == 1
}

// csrfBodyAttrs is the hx-headers JSON carried on <body> so every htmx
// request in the page sends the marker.
func csrfBodyAttrs(token string) string {
	return `{"X-CSRF-Token":"` + token + `"}`
}

// newImportID is a random id for a staged import (in memory only).
func newImportID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
