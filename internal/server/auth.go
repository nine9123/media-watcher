package server

// Login hardening: wrong-key throttling, constant-time key checks,
// cross-site request checks and browser security headers.

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxKeyFails = 10               // wrong keys from one address before it is locked out
	keyLockout  = 15 * time.Minute // how long the lockout lasts, and how long a failure counts
	maxClients  = 10_000           // bound on the addresses the guard remembers
)

// guard counts wrong API keys per client address (login form, X-Api-Key and
// ?api_key= alike) and refuses every key from an address that sent too many,
// right ones included, so a key cannot be brute-forced by spreading guesses
// over parallel requests. A valid session cookie still works while locked
// out: it is a 256-bit HMAC, not something worth guessing.
type guard struct {
	mu    sync.Mutex
	fails map[string]*strikes
}

type strikes struct {
	n    int
	last time.Time
}

func newGuard() *guard { return &guard{fails: map[string]*strikes{}} }

// blocked reports whether addr is locked out, and for how much longer.
func (g *guard) blocked(addr string, now time.Time) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.fails[addr]
	if s == nil || s.n < maxKeyFails {
		return 0, false
	}
	left := s.last.Add(keyLockout).Sub(now)
	if left <= 0 {
		delete(g.fails, addr)
		return 0, false
	}
	return left, true
}

// fail records a wrong key from addr; true when that starts a lockout.
func (g *guard) fail(addr string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.fails) >= maxClients {
		for k, s := range g.fails {
			if now.Sub(s.last) >= keyLockout {
				delete(g.fails, k)
			}
		}
		if len(g.fails) >= maxClients {
			g.fails = map[string]*strikes{} // flooded from everywhere: start over rather than grow
		}
	}
	s := g.fails[addr]
	if s == nil || now.Sub(s.last) >= keyLockout {
		s = &strikes{}
		g.fails[addr] = s
	}
	s.n++
	s.last = now
	return s.n == maxKeyFails
}

func (g *guard) succeed(addr string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fails, addr)
}

// clientAddr is the peer's IP. X-Forwarded-For is deliberately ignored: any
// client can set it, which would make the lockout trivial to dodge. Behind a
// reverse proxy every client therefore shares the proxy's lockout.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// equalSecret compares two secrets in constant time, without leaking the
// length of the expected one.
func equalSecret(got, want string) bool {
	g, w := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

// checkKey answers a request that presents key: nil if it is the API key, 401
// if not, 429 while the client is locked out.
func (a *App) checkKey(r *http.Request, key string) *httpError {
	addr, now := clientAddr(r), time.Now()
	if left, ok := a.guard.blocked(addr, now); ok {
		return &httpError{429, "too many wrong keys, try again in " + left.Round(time.Minute).String()}
	}
	if !equalSecret(key, a.Cfg.APIKey) {
		if a.guard.fail(addr, now) {
			log.Warnf("%d wrong API keys from %s: refusing its keys for %s", maxKeyFails, addr, keyLockout)
		}
		return fail(401, "unauthorized")
	}
	a.guard.succeed(addr)
	return nil
}

// sameOrigin reports whether a state-changing request comes from this app's
// own pages. SameSite=Strict keeps the cookie off cross-site requests, but on
// a NAS every other web app on the same host (another port) counts as
// "same site" and would still get it. Browsers send Sec-Fetch-Site; older
// ones only Origin. Requests with neither are not from a browser page
// (curl, scripts with X-Api-Key) and are let through.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "":
	default:
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false // includes Origin: null
	}
	fwd, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ",")
	return strings.EqualFold(u.Host, r.Host) || fwd != "" && strings.EqualFold(u.Host, strings.TrimSpace(fwd))
}

// secureRequest: reached over HTTPS, directly or through a TLS-terminating proxy.
func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func sessionCookie(r *http.Request, value string, maxAge int) string {
	c := cookieName + "=" + value + "; Path=/; HttpOnly; SameSite=Strict; Max-Age=" + strconv.Itoa(maxAge)
	if secureRequest(r) {
		c += "; Secure"
	}
	return c
}

// securityHeaders go on every response. No referrer, so an ?api_key= URL never
// leaks to another site; no framing (clickjacking); no MIME sniffing. The CSP
// leaves scripts alone because the UI uses inline onerror= handlers on images.
func securityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "frame-ancestors 'none'; object-src 'none'; base-uri 'none'; form-action 'self'")
}
