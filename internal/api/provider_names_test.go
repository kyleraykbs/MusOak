package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProviderNamesReader covers the optional providers= filter: absent or
// empty means every enabled provider, a comma-separated list names a subset,
// and an unknown name is an error naming it.
func TestProviderNamesReader(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()

	cases := []struct {
		name    string
		url     string
		want    []string
		wantErr string
	}{
		{"absent means all", "/api/v1/search?q=x", nil, ""},
		{"empty means all", "/api/v1/search?q=x&providers=", nil, ""},
		{"blank list means all", "/api/v1/search?q=x&providers=,", nil, ""},
		{"one name", "/api/v1/search?q=x&providers=ytmusic", []string{"ytmusic"}, ""},
		{"whitespace is trimmed", "/api/v1/search?q=x&providers=%20ytmusic%20", []string{"ytmusic"}, ""},
		{"unknown name errors", "/api/v1/search?q=x&providers=nope", nil, "nope"},
		{"unknown name among known errors", "/api/v1/search?q=x&providers=ytmusic,nope", nil, "nope"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			names, err := s.providerNames(httptest.NewRequest(http.MethodGet, tc.url, nil))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("err = nil, want one naming %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to name %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want none", err)
			}
			if len(names) != len(tc.want) {
				t.Fatalf("names = %v, want %v", names, tc.want)
			}
			for i := range tc.want {
				if names[i] != tc.want[i] {
					t.Fatalf("names = %v, want %v", names, tc.want)
				}
			}
		})
	}
}

// TestSearchUnknownProviderIsBadRequest proves the parameter is validated at
// the endpoint and names the offending provider.
func TestSearchUnknownProviderIsBadRequest(t *testing.T) {
	c := newHTTPTestServer(t, nil)

	rec := c.do(http.MethodGet, "/api/v1/search?q=x&providers=nope", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "nope") {
		t.Errorf("body = %q, want it to name the provider", body)
	}
}
