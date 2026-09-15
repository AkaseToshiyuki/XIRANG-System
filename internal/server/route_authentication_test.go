package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// registeredRoutePattern extracts every route literal handed to mux.HandleFunc
// in this package so the authentication contract below is checked against the
// routes that actually exist rather than a hand-maintained copy of them.
var registeredRoutePattern = regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) ([^"]+)"`)

var routeParameterPattern = regexp.MustCompile(`\{[^}]*\}`)

func registeredRoutes(t *testing.T) [][2]string {
	t.Helper()
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[[2]string]bool{}
	routes := [][2]string{}
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		content, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range registeredRoutePattern.FindAllStringSubmatch(string(content), -1) {
			route := [2]string{match[1], match[2]}
			if !seen[route] {
				seen[route] = true
				routes = append(routes, route)
			}
		}
	}
	if len(routes) < 50 {
		t.Fatalf("route extraction found only %d routes; the HandleFunc pattern likely changed", len(routes))
	}
	return routes
}

// TestEveryRegisteredRouteRejectsAnonymousRequests is the structural guarantee
// that authentication cannot be lost by omission. A route registered without an
// authentication wrapper must still be refused by accessMiddleware unless it is
// explicitly declared public or carries its own non-bearer credential.
func TestEveryRegisteredRouteRejectsAnonymousRequests(t *testing.T) {
	handler := New(Config{AdminToken: "bootstrap"}).HTTPHandler()
	for index, route := range registeredRoutes(t) {
		method, pattern := route[0], route[1]
		path := routeParameterPattern.ReplaceAllString(pattern, "x")
		exempt := publicIdentityRoute(method, path) || credentialOptionalRoute(method, path)
		request := httptest.NewRequest(method, path, nil)
		// A distinct peer per route keeps the shared rate limiter from
		// answering 429 before the authentication decision is reached.
		request.RemoteAddr = netAddressForIndex(index)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		switch {
		case exempt && recorder.Code == http.StatusUnauthorized:
			t.Errorf("%s %s is declared credential-optional but was rejected as anonymous", method, pattern)
		case !exempt && recorder.Code != http.StatusUnauthorized:
			t.Errorf("%s %s served an anonymous request with %d; it must fail closed or be declared public",
				method, pattern, recorder.Code)
		}
	}
}

// TestCredentialOptionalRoutesAreTheExpectedSet pins the exemption list so that
// widening it is a deliberate, reviewed edit rather than a side effect.
func TestCredentialOptionalRoutesAreTheExpectedSet(t *testing.T) {
	expected := map[[2]string]bool{
		{http.MethodGet, "/healthz"}:                                 true,
		{http.MethodGet, "/readyz"}:                                  true,
		{http.MethodGet, "/"}:                                        true,
		{http.MethodGet, "/assets/index.js"}:                         true,
		{http.MethodGet, "/api/v1/version"}:                          true,
		{http.MethodGet, "/api/v1/capabilities"}:                     true,
		{http.MethodGet, "/api/v1/enrollment/ca"}:                    true,
		{http.MethodGet, "/api/v1/terminal/connect"}:                 true,
		{http.MethodGet, "/artifacts/amd64/task"}:                    true,
		{http.MethodPost, "/api/v1/enrollment/register"}:             true,
		{http.MethodPost, "/api/v1/nodes/node-1/heartbeat"}:          true,
		{http.MethodPost, "/api/v1/nodes/node-1/certificate/rotate"}: true,

		// Deliberate near misses that must stay protected.
		{http.MethodPost, "/api/v1/nodes/node-1/certificate/rotate-command"}: false,
		{http.MethodPost, "/api/v1/nodes/node-1/approve"}:                    false,
		{http.MethodDelete, "/api/v1/nodes/node-1"}:                          false,
		{http.MethodGet, "/api/v1/nodes"}:                                    false,
		{http.MethodGet, "/api/v1/jobs"}:                                     false,
		{http.MethodGet, "/api/v1/audit"}:                                    false,
		{http.MethodGet, "/api/v1/access/users"}:                             false,
		{http.MethodPost, "/api/v1/terminal/tickets"}:                        false,
		{http.MethodGet, "/api/v1/enrollment/join-token"}:                    false,
		{http.MethodPost, "/api/v1/enrollment/tokens"}:                       false,
	}
	for route, want := range expected {
		if got := credentialOptionalRoute(route[0], route[1]); got != want {
			t.Errorf("credentialOptionalRoute(%s, %s) = %v, want %v", route[0], route[1], got, want)
		}
	}
}

func netAddressForIndex(index int) string {
	return "10." + strconv.Itoa((index/65536)%256) + "." + strconv.Itoa((index/256)%256) +
		"." + strconv.Itoa(index%256) + ":1024"
}
