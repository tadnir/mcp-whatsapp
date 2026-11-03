package daemon

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sealjay/mcp-whatsapp/internal/client"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

var (
	pairTmpl     = template.Must(template.ParseFS(templateFS, "templates/pair.html.tmpl"))
	pairedTmpl   = template.Must(template.ParseFS(templateFS, "templates/pair_success.html.tmpl"))
	pairCodeTmpl = template.Must(template.ParseFS(templateFS, "templates/pair_code.html.tmpl"))
)

const (
	qrPNGSize = 256
)

// pairPageData is the template context for pair pages.
type pairPageData struct {
	CSRFToken string
}

// resetter is the dependency `handlePairReset` needs. Production wiring
// satisfies it via *client.Client; tests substitute a fake.
type resetter interface {
	Logout(ctx context.Context) error
}

// pairCoder is the dependency `handlePairCode` needs. Production wiring
// satisfies it via *client.Client; tests substitute a fake.
type pairCoder interface {
	PairPhone(ctx context.Context, phone string) (string, error)
}

// pairHandlers bundles the three pair endpoints against a shared cache and
// a resetter. Wire to an *http.ServeMux via mount.
type pairHandlers struct {
	cache *PairCache
	reset resetter
	// coder may be nil: drivers that do not support phone-number pairing
	// leave it unset and /pair/code returns 501 Not Implemented.
	coder pairCoder

	// One rate limiter per endpoint.
	pairGetLimiter   *Limiter
	pairQRLimiter    *Limiter
	pairResetLimiter *Limiter
	pairCodeLimiter  *Limiter

	// CSRF protection for the POST endpoints: every rendered /pair page
	// gets its own single-use token, valid for csrfTokenTTL. Keeping more
	// than one means a second tab, or the page reloading itself, does not
	// invalidate the form a person is filling in.
	csrfMu     sync.Mutex
	csrfTokens map[string]time.Time // token -> issued at
}

const (
	// csrfTokenTTL is how long a /pair page's form stays submittable.
	csrfTokenTTL = 15 * time.Minute
	// csrfMaxTokens caps the outstanding tokens; the oldest go first.
	csrfMaxTokens = 64
)

// newPairHandlers constructs handlers with default rate limiters.
func newPairHandlers(cache *PairCache, reset resetter, coder pairCoder) *pairHandlers {
	return &pairHandlers{
		cache: cache,
		reset: reset,
		coder: coder,
		// pair.html.tmpl auto-refreshes the whole page (and its embedded QR
		// image) every 5s while unpaired, so both limiters below must refill
		// faster than that, with room for a few open tabs (each reloads on
		// its own), or viewers permanently rate-limit themselves once the
		// burst is spent. The limiters are shared by every client.
		pairGetLimiter:   NewLimiter(60.0/60.0, 20), // 60/min, burst 20
		pairQRLimiter:    NewLimiter(60.0/60.0, 20), // 60/min, burst 20
		pairResetLimiter: NewLimiter(1.0/60.0, 1),   // 1/min, burst 1
		// 3/min, burst 2; WhatsApp limits code requests server-side too.
		// Only requests that pass the CSRF and phone checks are charged.
		pairCodeLimiter: NewLimiter(3.0/60.0, 2),
	}
}

// generateCSRFToken creates a new random CSRF token, stores it in the
// handler, and returns it. Thread-safe.
func (h *pairHandlers) generateCSRFToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback should never be reached in practice.
		panic(fmt.Sprintf("crypto/rand.Read failed: %v", err))
	}
	token := hex.EncodeToString(b)
	now := time.Now()
	h.csrfMu.Lock()
	defer h.csrfMu.Unlock()
	if h.csrfTokens == nil {
		h.csrfTokens = make(map[string]time.Time)
	}
	h.pruneCSRFTokensLocked(now)
	for len(h.csrfTokens) >= csrfMaxTokens {
		var oldest string
		var oldestAt time.Time
		for t, at := range h.csrfTokens {
			if oldest == "" || at.Before(oldestAt) {
				oldest, oldestAt = t, at
			}
		}
		delete(h.csrfTokens, oldest)
	}
	h.csrfTokens[token] = now
	return token
}

// pruneCSRFTokensLocked drops expired tokens. Callers hold csrfMu.
func (h *pairHandlers) pruneCSRFTokensLocked(now time.Time) {
	for t, at := range h.csrfTokens {
		if now.Sub(at) > csrfTokenTTL {
			delete(h.csrfTokens, t)
		}
	}
}

// validCSRFToken checks whether the supplied token is one this server
// issued and has not expired. A match consumes the token (single-use).
// Thread-safe.
func (h *pairHandlers) validCSRFToken(token string) bool {
	h.csrfMu.Lock()
	defer h.csrfMu.Unlock()
	if token == "" || h.csrfTokens == nil {
		return false
	}
	h.pruneCSRFTokensLocked(time.Now())
	if _, ok := h.csrfTokens[token]; !ok {
		return false
	}
	delete(h.csrfTokens, token) // single-use: consume after validation
	return true
}

func (h *pairHandlers) mount(mux *http.ServeMux) {
	mux.HandleFunc("/pair", h.handlePairPage)
	mux.HandleFunc("/pair/qr.png", h.handlePairQR)
	mux.HandleFunc("/pair/reset", h.handlePairReset)
	mux.HandleFunc("/pair/code", h.handlePairCode)
}

func (h *pairHandlers) handlePairPage(w http.ResponseWriter, r *http.Request) {
	if !h.pairGetLimiter.Allow() {
		w.Header().Set("Retry-After", "4")
		// Browsers follow Refresh on any response, so an open page
		// recovers by itself instead of stopping at this error.
		w.Header().Set("Refresh", "4")
		http.Error(w, "rate limit exceeded, retrying in a few seconds", http.StatusTooManyRequests)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := pairPageData{
		CSRFToken: h.generateCSRFToken(),
	}
	if h.cache.Paired() {
		if err := pairedTmpl.Execute(w, data); err != nil {
			http.Error(w, "template error", http.StatusInternalServerError)
		}
		return
	}
	if err := pairTmpl.Execute(w, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (h *pairHandlers) handlePairQR(w http.ResponseWriter, r *http.Request) {
	if !h.pairQRLimiter.Allow() {
		w.Header().Set("Retry-After", "4")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	qr := h.cache.QR()
	if qr == "" {
		http.NotFound(w, r)
		return
	}
	png, err := renderQRPNG(qr, qrPNGSize)
	if err != nil {
		http.Error(w, "qr render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (h *pairHandlers) handlePairReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !h.pairResetLimiter.Allow() {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	// Validate the CSRF token from the form submission.
	if !h.validCSRFToken(r.FormValue("csrf_token")) {
		http.Error(w, "invalid or missing CSRF token", http.StatusForbidden)
		return
	}
	if err := h.reset.Logout(r.Context()); err != nil {
		http.Error(w, fmt.Sprintf("logout failed: %v", err), http.StatusInternalServerError)
		return
	}
	h.cache.Reset()
	http.Redirect(w, r, "/pair", http.StatusSeeOther)
}

// pairCodePageData is the template context for the pairing-code result page.
type pairCodePageData struct {
	Code    string // formatted code, empty on error
	Phone   string
	Message string // non-empty on failure
}

// normalizePairingPhone strips formatting characters and sanity-checks the
// remaining digits. Returns the cleaned international number (digits only,
// no leading plus) or an empty string when the input is unusable.
func normalizePairingPhone(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	digits := b.String()
	// whatsmeow itself rejects short numbers and leading-zero (non-
	// international) numbers; mirror that here with a friendly check.
	if len(digits) < 7 || len(digits) > 15 || strings.HasPrefix(digits, "0") {
		return ""
	}
	return digits
}

func (h *pairHandlers) handlePairCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if h.cache.Paired() {
		http.Redirect(w, r, "/pair", http.StatusSeeOther)
		return
	}
	if h.coder == nil {
		http.Error(w, "phone-number pairing not supported by this driver", http.StatusNotImplemented)
		return
	}
	// Reuse the CSRF token minted on the /pair page so a drive-by POST
	// cannot request codes on the user's behalf.
	if !h.validCSRFToken(r.FormValue("csrf_token")) {
		http.Error(w, "invalid or missing CSRF token", http.StatusForbidden)
		return
	}
	phone := normalizePairingPhone(r.FormValue("phone"))
	// Charged only now, so a stale form or a mistyped number does not
	// spend the few code requests a minute WhatsApp itself allows.
	if phone != "" && !h.pairCodeLimiter.Allow() {
		w.Header().Set("Retry-After", "20")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = pairCodeTmpl.Execute(w, pairCodePageData{Message: "Too many pairing-code requests. Wait 20 seconds, then go back and try again."})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := pairCodePageData{Phone: phone}
	if phone == "" {
		data.Phone = ""
		data.Message = "Enter a valid international phone number (country code first, digits only)."
	} else if code, err := h.coder.PairPhone(r.Context(), phone); err != nil {
		data.Message = fmt.Sprintf("Pairing-code request failed: %v", err)
	} else {
		data.Code = client.FormatPairingCode(code)
	}
	if err := pairCodeTmpl.Execute(w, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}
