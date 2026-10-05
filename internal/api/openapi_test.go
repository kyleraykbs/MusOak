package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(openAPISpec)
	if err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml does not validate: %v", err)
	}
	return doc
}

func TestOpenAPISpecIsValid(t *testing.T) {
	doc := loadSpec(t)
	if doc.Info == nil || doc.Info.Title == "" {
		t.Error("the specification has no title")
	}
	if len(doc.Paths.Map()) < 20 {
		t.Errorf("only %d paths documented", len(doc.Paths.Map()))
	}
}

// TestSpecAndRoutesAgree is the drift guard: every route the server serves must
// be documented, and the specification must not promise anything it does not
// serve.
func TestSpecAndRoutesAgree(t *testing.T) {
	doc := loadSpec(t)
	server := newTestServer(t)

	documented := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		documented[path] = true
		_ = item
	}
	registered := map[string]bool{}

	for _, route := range server.routeTable() {
		method, path, ok := strings.Cut(route.pattern, " ")
		if !ok {
			t.Fatalf("route %q carries no method", route.pattern)
		}
		registered[path] = true

		item := doc.Paths.Find(path)
		if item == nil {
			t.Errorf("route %s is not in the specification", route.pattern)
			continue
		}
		if item.GetOperation(strings.ToLower(method)) == nil && item.GetOperation(method) == nil {
			t.Errorf("route %s has no %s operation in the specification", path, method)
		}
	}

	for path := range documented {
		if !registered[path] {
			t.Errorf("the specification documents %s, which the server does not serve", path)
		}
	}
}

func TestOpenAPISpecIsServed(t *testing.T) {
	server := newTestServer(t)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.yaml", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "yaml") {
		t.Errorf("content type = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "openapi: 3.0.3") {
		t.Error("the served document does not look like the specification")
	}
}

// TestDocumentedEndpointsExist spot-checks that the specification names the
// endpoints a client author actually needs.
func TestDocumentedEndpointsExist(t *testing.T) {
	doc := loadSpec(t)
	for _, path := range []string{
		"/api/v1/search",
		"/api/v1/tracks/{trackId}",
		"/api/v1/tracks/{trackId}/variants",
		"/api/v1/tracks/{trackId}/resolve",
		"/api/v1/media/{variantId}",
		"/api/v1/media/{variantId}/status",
		"/api/v1/media/{variantId}/download",
		"/api/v1/library/import",
		"/api/v1/auth/register",
		"/api/v1/auth/login",
		"/api/v1/me/favorites",
		"/api/v1/me/providers/ranking",
		"/api/v1/providers",
		"/api/v1/clock",
		"/api/v1/ws",
		"/api/v1/rooms",
		"/api/v1/rooms/{roomId}/join",
		"/api/v1/rooms/{roomId}/vote",
		"/api/v1/rooms/{roomId}/started",
		"/api/v1/rooms/{roomId}/ended",
	} {
		if doc.Paths.Find(path) == nil {
			t.Errorf("the specification is missing %s", path)
		}
	}
}
