package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
)

func accessMiddleware(service *identity.AccessService, bootstrapToken string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if publicIdentityRoute(request.Method, request.URL.Path) {
			next.ServeHTTP(writer, request)
			return
		}
		header := request.Header.Get("Authorization")
		if header == "" {
			// Fail closed. Only routes that present their own non-bearer
			// credential further down the chain — node mTLS, a single-use
			// terminal ticket, an enrollment token — or that serve public
			// content may continue without an Authorization header. A route
			// added without an authentication wrapper is rejected here
			// instead of becoming silently reachable.
			if !credentialOptionalRoute(request.Method, request.URL.Path) {
				writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
				return
			}
			next.ServeHTTP(writer, request)
			return
		}
		if service == nil || (bootstrapToken != "" && identity.AuthenticateBearer(header, bootstrapToken)) {
			next.ServeHTTP(writer, request)
			return
		}
		bearer, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || bearer == "" {
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "invalid bearer credential"})
			return
		}
		principal, err := service.Authenticate(request.Context(), bearer)
		if err != nil {
			status := http.StatusUnauthorized
			if errors.Is(err, identity.ErrForbidden) {
				status = http.StatusForbidden
			}
			writeJSON(writer, status, map[string]string{"error": err.Error()})
			return
		}
		if err := service.Authorize(principal, requiredPermission(request.Method, request.URL.Path)); err != nil {
			writeJSON(writer, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		next.ServeHTTP(writer, request.WithContext(identity.WithPrincipal(request.Context(), principal)))
	})
}

func publicIdentityRoute(method, path string) bool {
	return (method == http.MethodGet && path == "/api/v1/setup/status") ||
		(method == http.MethodPost && (path == "/api/v1/setup/admin" || path == "/api/v1/auth/register" || path == "/api/v1/auth/login"))
}

// credentialOptionalRoute reports whether a request carrying no Authorization
// header may still reach its handler. Every entry either serves unauthenticated
// content or is guarded by a credential the access middleware cannot read: an
// enrollment token in the request body, a client certificate verified by
// authenticatedNodeCertificate, or a single-use terminal ticket. Any route
// outside this set is rejected before it is dispatched.

func credentialOptionalRoute(method, path string) bool {
	// Console assets and health probes are served without a credential.
	if !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/artifacts/") {
		return true
	}
	switch method {
	case http.MethodGet:
		switch path {
		case "/api/v1/version", "/api/v1/capabilities", "/api/v1/enrollment/ca":
			return true
		case "/api/v1/terminal/connect":
			// Authorized by a single-use ticket in the query string because a
			// browser WebSocket handshake cannot carry a bearer header.
			return true
		}
		// Agents fetch build artifacts with their enrolled client certificate.
		return strings.HasPrefix(path, "/artifacts/")
	case http.MethodPost:
		if path == "/api/v1/enrollment/register" {
			return true
		}
		// Node certificate rotation and heartbeat authenticate with the
		// existing client certificate, never with a bearer token. The
		// "/certificate/rotate-command" administrator route is deliberately
		// excluded by the exact suffix match.
		return strings.HasPrefix(path, "/api/v1/nodes/") &&
			(strings.HasSuffix(path, "/heartbeat") || strings.HasSuffix(path, "/certificate/rotate"))
	}
	return false
}

func requiredPermission(method, path string) string {
	if strings.HasPrefix(path, "/artifacts/") {
		return identity.PermissionRead
	}
	if strings.HasPrefix(path, "/api/v1/shared-spaces/") && strings.Contains(path, "/files") {
		if method != http.MethodGet && method != http.MethodHead {
			return identity.PermissionFilesWrite
		}
		return identity.PermissionRead
	}
	if strings.HasPrefix(path, "/api/v1/terminal") {
		return identity.PermissionAccessManage
	}
	if strings.HasPrefix(path, "/api/v1/job-templates") {
		if method == http.MethodGet {
			return identity.PermissionRead
		}
		return identity.PermissionJobTemplates
	}
	if strings.HasPrefix(path, "/api/v1/access/") {
		if path == "/api/v1/access/me" {
			return identity.PermissionRead
		}
		return identity.PermissionAccessManage
	}
	if strings.HasPrefix(path, "/api/v1/system/") {
		if method == http.MethodGet && path == "/api/v1/system/telemetry-interval" {
			return identity.PermissionRead
		}
		return identity.PermissionAccessManage
	}
	if strings.HasPrefix(path, "/api/v1/build-signing-key") {
		if method == http.MethodGet {
			return identity.PermissionRead
		}
		return identity.PermissionAccessManage
	}
	if path == "/api/v1/build-retention" || path == "/api/v1/builds/cleanup" {
		return identity.PermissionAccessManage
	}
	if strings.HasPrefix(path, "/api/v1/audit") {
		return identity.PermissionAuditRead
	}
	if strings.HasPrefix(path, "/api/v1/accounting") {
		if method == http.MethodGet {
			return identity.PermissionRead
		}
		return identity.PermissionAccessManage
	}
	if strings.HasPrefix(path, "/api/v1/enrollment/join-token") {
		return identity.PermissionAccessManage
	}
	if strings.HasPrefix(path, "/api/v1/discovery") || path == "/api/v1/assignments" {
		return identity.PermissionNodesManage
	}
	if strings.HasPrefix(path, "/api/v1/build") {
		return identity.PermissionBuildsManage
	}
	if strings.HasPrefix(path, "/api/v1/queues") || path == "/api/v1/elasticity" {
		return identity.PermissionQueuesManage
	}
	if method == http.MethodGet {
		return identity.PermissionRead
	}
	switch {
	case strings.HasPrefix(path, "/api/v1/build"):
		return identity.PermissionBuildsManage
	case strings.HasPrefix(path, "/api/v1/jobs"):
		return identity.PermissionJobsManage
	case strings.HasPrefix(path, "/api/v1/queues"):
		return identity.PermissionQueuesManage
	default:
		return identity.PermissionNodesManage
	}
}

// streamLimitMiddleware bounds long-lived browser event streams separately
// from the short-request rate limiter. Browsers reconnect automatically when
// the finite lifetime expires, preventing stale tabs from retaining resources
// forever while preserving the live-update contract.

func streamLimitMiddleware(globalLimit, perPrincipal int, lifetime time.Duration, next http.Handler) http.Handler {
	if globalLimit < 1 || perPrincipal < 1 || lifetime <= 0 {
		return next
	}
	var mu sync.Mutex
	global := 0
	active := make(map[string]int)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !eventStreamRoute(request.Method, request.URL.Path) {
			next.ServeHTTP(writer, request)
			return
		}
		key := request.RemoteAddr
		if principal, ok := identity.PrincipalFromContext(request.Context()); ok {
			key = principalIdentity(principal)
		}
		if host, _, err := net.SplitHostPort(key); err == nil {
			key = host
		}
		mu.Lock()
		if global >= globalLimit || active[key] >= perPrincipal {
			mu.Unlock()
			writeJSON(writer, http.StatusTooManyRequests, map[string]string{"error": "event stream limit reached"})
			return
		}
		global++
		active[key]++
		mu.Unlock()
		defer func() {
			mu.Lock()
			global--
			active[key]--
			if active[key] == 0 {
				delete(active, key)
			}
			mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(request.Context(), lifetime)
		defer cancel()
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func eventStreamRoute(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	return path == "/api/v1/events/nodes" || path == "/api/v1/events/topology" || path == "/api/v1/events/alerts" ||
		(strings.HasPrefix(path, "/api/v1/jobs/") && strings.HasSuffix(path, "/logs/stream"))
}

func requestPrincipal(request *http.Request) domain.Principal {
	if principal, ok := identity.PrincipalFromContext(request.Context()); ok {
		return principal
	}
	return domain.Principal{
		Subject: "bootstrap-admin", DisplayName: "Bootstrap administrator", Role: "admin",
		Permissions: []string{"*"}, AuthMethod: "bootstrap",
	}
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestID := request.Header.Get("X-Request-ID")
		if requestID == "" {
			buffer := make([]byte, 16)
			if _, err := rand.Read(buffer); err == nil {
				requestID = hex.EncodeToString(buffer)
			}
		}
		if requestID != "" {
			writer.Header().Set("X-Request-ID", requestID)
		}
		next.ServeHTTP(writer, request)
	})
}

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		writer.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		writer.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'; img-src 'self' data:; font-src 'self'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; connect-src 'self' ws: wss:")
		if request.TLS != nil {
			writer.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(writer, request)
	})
}

func rateLimitMiddleware(rate, burst float64, next http.Handler) http.Handler {
	return throttleMiddleware(rate, burst, "management API rate limit exceeded", nil, next)
}

// credentialThrottleRoute selects the endpoints that verify a password. They
// are throttled far harder than the management API because each one costs an
// Argon2 hash and because they are the endpoints worth guessing against.

func credentialThrottleRoute(method, path string) bool {
	return method == http.MethodPost &&
		(path == "/api/v1/auth/login" || path == "/api/v1/auth/register" || path == "/api/v1/setup/admin")
}

// throttleMiddleware applies a per-peer token bucket. When applies is nil every
// request is metered; otherwise only the selected routes are, leaving a second
// bucket free to meter a narrower set at a different rate.

func throttleMiddleware(rate, burst float64, message string, applies func(method, path string) bool, next http.Handler) http.Handler {
	type bucket struct {
		tokens  float64
		updated time.Time
	}
	const sweepInterval = time.Second
	var mu sync.Mutex
	buckets := map[string]bucket{}
	var lastSweep time.Time
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if applies != nil && !applies(request.Method, request.URL.Path) {
			next.ServeHTTP(writer, request)
			return
		}
		host, _, err := net.SplitHostPort(request.RemoteAddr)
		if err != nil || host == "" {
			host = request.RemoteAddr
		}
		if host == "" {
			host = "local"
		}
		now := time.Now()
		mu.Lock()
		current := buckets[host]
		if current.updated.IsZero() {
			current.tokens = burst
		} else {
			current.tokens += now.Sub(current.updated).Seconds() * rate
		}
		if current.tokens > burst {
			current.tokens = burst
		}
		current.updated = now
		allowed := current.tokens >= 1
		if allowed {
			current.tokens--
		}
		buckets[host] = current
		// A bucket that has refilled to capacity carries no state a new bucket
		// would not start with, so dropping it changes no future decision.
		// Sweeping on a timer bounds the map by the set of peers currently
		// spending tokens. Sweeping only once the map grew past a size, as this
		// did before, collects nothing while every entry happens to be recent.
		if now.Sub(lastSweep) >= sweepInterval {
			lastSweep = now
			for key, value := range buckets {
				if value.tokens+now.Sub(value.updated).Seconds()*rate >= burst {
					delete(buckets, key)
				}
			}
		}
		mu.Unlock()
		if !allowed {
			writer.Header().Set("Retry-After", "1")
			writeJSON(writer, http.StatusTooManyRequests, map[string]string{"error": message})
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func loggingMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		next.ServeHTTP(writer, request)
		logger.Info("http request", "method", request.Method, "path", request.URL.Path, "duration", time.Since(started))
	})
}

func requireAuthenticated(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		_, hasPrincipal := identity.PrincipalFromContext(request.Context())
		if !hasPrincipal && (token == "" || !identity.AuthenticateBearer(request.Header.Get("Authorization"), token)) {
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		next(writer, request)
	}
}
