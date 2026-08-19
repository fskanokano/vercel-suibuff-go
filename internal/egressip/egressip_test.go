package egressip

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveJSON returns an httptest server that answers every request with the
// given status and body.
func serveJSON(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// testProvider builds a provider whose parser reads the ip/country/region/
// city fields directly (the common shape across the test fixtures).
func testProvider(name, url string) provider {
	return provider{
		name: name,
		url:  url,
		parse: func(b []byte) (Result, error) {
			var v struct {
				IP      string `json:"ip"`
				Country string `json:"country"`
				Region  string `json:"region"`
				City    string `json:"city"`
			}
			if err := json.Unmarshal(b, &v); err != nil {
				return Result{}, err
			}
			return Result{IP: v.IP, Country: v.Country, Region: v.Region, City: v.City}, nil
		},
	}
}

// TestDefaultParsers feeds each built-in provider a representative sample
// of its real response shape and asserts the IP/country/region are parsed.
func TestDefaultParsers(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		ip      string
		country string
		region  string
	}{
		{
			name:    "ipwho.is",
			body:    `{"ip":"203.0.113.1","success":true,"country":"United States","country_code":"US","region":"California","city":"San Francisco"}`,
			ip:      "203.0.113.1",
			country: "US",
			region:  "California",
		},
		{
			name:    "ipinfo.io",
			body:    `{"ip":"203.0.113.2","country":"US","region":"California","city":"Mountain View"}`,
			ip:      "203.0.113.2",
			country: "US",
			region:  "California",
		},
		{
			name:    "ip-api.com",
			body:    `{"status":"success","query":"203.0.113.3","country":"United States","countryCode":"US","regionName":"California","city":"San Jose"}`,
			ip:      "203.0.113.3",
			country: "US",
			region:  "California",
		},
		{
			name:    "ipapi.co",
			body:    `{"ip":"203.0.113.4","country_name":"United States","country_code":"US","region":"California","city":"Los Angeles"}`,
			ip:      "203.0.113.4",
			country: "US",
			region:  "California",
		},
		{
			name:    "geojs.io",
			body:    `{"ip":"203.0.113.5","country":"United States","country_code":"US","region":"California","city":"San Diego"}`,
			ip:      "203.0.113.5",
			country: "US",
			region:  "California",
		},
	}

	byName := make(map[string]provider, len(defaultProviders))
	for _, p := range defaultProviders {
		byName[p.name] = p
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := byName[tc.name]
			if !ok {
				t.Fatalf("built-in provider %q not found", tc.name)
			}
			r, err := p.parse([]byte(tc.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if r.IP != tc.ip || r.Country != tc.country || r.Region != tc.region {
				t.Errorf("parse = ip %q country %q region %q, want %q/%q/%q",
					r.IP, r.Country, r.Region, tc.ip, tc.country, tc.region)
			}
		})
	}
}

// TestProbeRoundRobinAndFailover guards the two core behaviors: the cursor
// advances across calls (round-robin), and a failing provider is skipped in
// favor of the next one without aborting the whole probe.
func TestProbeRoundRobinAndFailover(t *testing.T) {
	t.Run("failover skips a broken provider", func(t *testing.T) {
		bad := serveJSON(t, http.StatusInternalServerError, `{}`)
		good := serveJSON(t, http.StatusOK, `{"ip":"203.0.113.10","country":"US","region":"California","city":"SF"}`)
		c := NewClient(nil)
		c.providers = []provider{testProvider("bad", bad.URL), testProvider("good", good.URL)}

		r, err := c.Probe(context.Background())
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if r.Provider != "good" || r.IP != "203.0.113.10" {
			t.Errorf("Probe = provider %q ip %q, want good / 203.0.113.10", r.Provider, r.IP)
		}
	})

	t.Run("cursor advances across calls", func(t *testing.T) {
		mk := func(name, ip string) provider {
			ts := serveJSON(t, http.StatusOK, fmt.Sprintf(`{"ip":%q,"country":"US","region":"CA","city":"X"}`, ip))
			return testProvider(name, ts.URL)
		}
		c := NewClient(nil)
		c.providers = []provider{mk("p0", "10.0.0.1"), mk("p1", "10.0.0.2"), mk("p2", "10.0.0.3")}

		// Three calls, three different starting providers; the fourth wraps.
		want := []string{"p0", "p1", "p2", "p0"}
		for i, w := range want {
			r, err := c.Probe(context.Background())
			if err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
			if r.Provider != w {
				t.Errorf("call %d = provider %q, want %q", i, r.Provider, w)
			}
		}
	})
}

// TestProbeAllFail guards the total-failure path: when every provider fails
// the probe returns an error naming the count.
func TestProbeAllFail(t *testing.T) {
	a := serveJSON(t, http.StatusInternalServerError, `{}`)
	b := serveJSON(t, http.StatusInternalServerError, `{}`)
	c := NewClient(nil)
	c.providers = []provider{testProvider("a", a.URL), testProvider("b", b.URL)}

	_, err := c.Probe(context.Background())
	if err == nil {
		t.Fatal("Probe succeeded with all providers failing")
	}
	if !strings.Contains(err.Error(), "2 providers failed") {
		t.Errorf("error %q does not mention the provider count", err)
	}
}

// TestProbeNoProviders guards the empty-list edge.
func TestProbeNoProviders(t *testing.T) {
	c := NewClient(nil)
	c.providers = nil
	if _, err := c.Probe(context.Background()); err == nil {
		t.Fatal("Probe with no providers succeeded")
	}
}

// TestHandler guards the HTTP contract: 200 JSON with ip + location on
// success, 502 JSON on total failure, and 405 for non-GET methods.
func TestHandler(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		ts := serveJSON(t, http.StatusOK, `{"ip":"203.0.113.20","country":"US","region":"California","city":"SF"}`)
		c := NewClient(nil)
		c.providers = []provider{testProvider("test", ts.URL)}

		req := httptest.NewRequest(http.MethodGet, endpointPath, nil)
		rec := httptest.NewRecorder()
		c.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response is not JSON: %v", err)
		}
		if body["ok"] != true || body["ip"] != "203.0.113.20" ||
			body["country"] != "US" || body["region"] != "California" ||
			body["provider"] != "test" {
			t.Errorf("body = %v, want ok/ip/country/region/provider populated", body)
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		c := NewClient(nil)
		req := httptest.NewRequest(http.MethodPost, endpointPath, nil)
		rec := httptest.NewRecorder()
		c.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", rec.Code)
		}
	})

	t.Run("all providers fail -> 502", func(t *testing.T) {
		ts := serveJSON(t, http.StatusInternalServerError, `{}`)
		c := NewClient(nil)
		c.providers = []provider{testProvider("bad", ts.URL)}

		req := httptest.NewRequest(http.MethodGet, endpointPath, nil)
		rec := httptest.NewRecorder()
		c.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", rec.Code)
		}
	})
}

// TestWrap guards the routing wrapper: /egress/ip reaches the egress
// handler while every other path falls through to next.
func TestWrap(t *testing.T) {
	ts := serveJSON(t, http.StatusOK, `{"ip":"203.0.113.30","country":"US","region":"CA","city":"X"}`)
	c := NewClient(nil)
	c.providers = []provider{testProvider("test", ts.URL)}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := Wrap(next, c)

	t.Run("egress path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, endpointPath, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("other path falls through", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusTeapot {
			t.Errorf("status = %d, want 418 from next", rec.Code)
		}
	})
}
