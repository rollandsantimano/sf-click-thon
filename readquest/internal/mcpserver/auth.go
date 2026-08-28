package mcpserver

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
)

// requireAPIKey gates the MCP endpoint on a shared secret.
//
// LibreChat's "Add MCP Server" dialog offers API Key auth but does not state
// which header it puts the key in, and that cannot be confirmed until the
// server is registered there for real. So BOTH conventional forms are
// accepted: "X-API-Key: <key>" and "Authorization: Bearer <key>". Accepting
// two headers costs nothing; guessing wrong costs a debugging session on
// hackathon day, when the tunnel, the key and the schema are all suspects at
// once.
func requireAPIKey(want string, next http.Handler) http.Handler {
	if want == "" {
		// Running unauthenticated is a legitimate local-development mode, but
		// it is not safe once ngrok makes the port world-reachable — so it is
		// loud rather than silent.
		slog.Warn("READQUEST_API_KEY is empty — the MCP endpoint is UNAUTHENTICATED. " +
			"Set it before exposing this server through a tunnel.")
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !keyMatches(r, want) {
			slog.Warn("rejected unauthenticated MCP request",
				"remote", r.RemoteAddr, "path", r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// The request is authenticated, so neutralise mcp-go's DNS rebinding
		// guard. That guard rejects any request whose Host header is not a
		// loopback name when the connection itself arrived on loopback —
		// which is exactly the shape of every tunnelled request, since ngrok
		// forwards to localhost while preserving the public Host.
		//
		// The guard exists to stop a malicious web page from rebinding its
		// domain to 127.0.0.1 and driving a victim's browser into a local MCP
		// server. Such a page cannot produce the API key, and this rewrite is
		// unreachable without one — the shared secret is the stronger control,
		// and it has already been checked by this line.
		//
		// Doing it here rather than asking the tunnel to rewrite the header
		// keeps the fix working for any tunnel (ngrok, Tailscale Funnel,
		// Cloudflare) instead of depending on one vendor's flag.
		r.Host = "localhost"

		next.ServeHTTP(w, r)
	})
}

func keyMatches(r *http.Request, want string) bool {
	if got := r.Header.Get("X-API-Key"); got != "" {
		return secureEqual(got, want)
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		// Bearer is the common spelling, but tolerate a bare key too.
		return secureEqual(strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")), want)
	}
	return false
}

// secureEqual compares in constant time. The comparison is cheap and the
// habit is worth keeping even on a one-day build.
func secureEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
