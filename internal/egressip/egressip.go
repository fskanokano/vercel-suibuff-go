// Package egressip reports the proxy's outbound (egress) public IP address
// and its geographic location to upstream callers over HTTP.
//
// The feature is deliberately isolated in this single file so the upstream
// sync (whole-tree overwrite) never touches it: the file does not exist in
// upstream, and it is wired from the Vercel-only root main.go rather than
// the upstream-synced server route table. It therefore survives syncing
// with no manual reconciliation.
//
// On each request it probes a rotating list of free, keyless IP-geolocation
// services round-robin: every call starts at the provider after the one the
// previous call started at, and walks the list once until a provider
// answers. A single outage or rate limit only costs one attempt, so the
// endpoint keeps working as long as any provider is up.
package egressip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// probeTimeout bounds one provider request end to end (dial, TLS, body).
const probeTimeout = 8 * time.Second

// maxBodyBytes caps a provider response read. Real responses are a few KB;
// this only guards against a misbehaving endpoint.
const maxBodyBytes = 1 << 20

// endpointPath is the route the egress-IP endpoint is served at.
const endpointPath = "/egress/ip"

// Result is one successful probe: the public IP and its location as
// reported by the provider. Country is the ISO 3166-1 alpha-2 code (e.g.
// "US"); CountryName is the full English name (may be empty when a provider
// reports only the code, e.g. ipinfo.io).
type Result struct {
	IP          string
	Country     string
	CountryName string
	Region      string
	City        string
	Provider    string
}

// provider is one IP-geolocation endpoint and its response parser.
type provider struct {
	name  string
	url   string
	parse func(body []byte) (Result, error)
}

// defaultProviders is the rotating list of free, keyless, US-reachable
// IP-geolocation services. Order matters: Probe starts each request at the
// index after the previous one (round-robin), spreading load across the
// providers so a single outage or rate limit only ever costs one attempt.
var defaultProviders = []provider{
	{
		name: "ipwho.is",
		url:  "https://ipwho.is/",
		parse: func(b []byte) (Result, error) {
			var v struct {
				IP          string `json:"ip"`
				Success     bool   `json:"success"`
				Country     string `json:"country"`
				CountryCode string `json:"country_code"`
				Region      string `json:"region"`
				City        string `json:"city"`
				Message     string `json:"message"`
			}
			if err := json.Unmarshal(b, &v); err != nil {
				return Result{}, err
			}
			if !v.Success {
				return Result{}, fmt.Errorf("unsuccessful: %s", orDefault(v.Message, "no message"))
			}
			if v.IP == "" {
				return Result{}, errors.New("missing ip field")
			}
			return Result{IP: v.IP, Country: v.CountryCode, CountryName: v.Country, Region: v.Region, City: v.City}, nil
		},
	},
	{
		name: "ipinfo.io",
		url:  "https://ipinfo.io/json",
		parse: func(b []byte) (Result, error) {
			var v struct {
				IP      string `json:"ip"`
				Country string `json:"country"` // ISO code only, no full name
				Region  string `json:"region"`
				City    string `json:"city"`
			}
			if err := json.Unmarshal(b, &v); err != nil {
				return Result{}, err
			}
			if v.IP == "" {
				return Result{}, errors.New("missing ip field")
			}
			return Result{IP: v.IP, Country: v.Country, Region: v.Region, City: v.City}, nil
		},
	},
	{
		name: "ip-api.com",
		url:  "http://ip-api.com/json?fields=status,message,query,country,countryCode,regionName,city",
		parse: func(b []byte) (Result, error) {
			var v struct {
				Status      string `json:"status"`
				Message     string `json:"message"`
				Query       string `json:"query"`
				Country     string `json:"country"`
				CountryCode string `json:"countryCode"`
				RegionName  string `json:"regionName"`
				City        string `json:"city"`
			}
			if err := json.Unmarshal(b, &v); err != nil {
				return Result{}, err
			}
			if v.Status != "success" {
				return Result{}, fmt.Errorf("unsuccessful: %s", orDefault(v.Message, "status "+v.Status))
			}
			if v.Query == "" {
				return Result{}, errors.New("missing query field")
			}
			return Result{IP: v.Query, Country: v.CountryCode, CountryName: v.Country, Region: v.RegionName, City: v.City}, nil
		},
	},
	{
		name: "ipapi.co",
		url:  "https://ipapi.co/json/",
		parse: func(b []byte) (Result, error) {
			var v struct {
				IP          string `json:"ip"`
				CountryName string `json:"country_name"`
				CountryCode string `json:"country_code"`
				Region      string `json:"region"`
				City        string `json:"city"`
			}
			if err := json.Unmarshal(b, &v); err != nil {
				return Result{}, err
			}
			if v.IP == "" {
				return Result{}, errors.New("missing ip field")
			}
			return Result{IP: v.IP, Country: v.CountryCode, CountryName: v.CountryName, Region: v.Region, City: v.City}, nil
		},
	},
	{
		name: "geojs.io",
		url:  "https://get.geojs.io/v1/ip/geo.json",
		parse: func(b []byte) (Result, error) {
			var v struct {
				IP          string `json:"ip"`
				Country     string `json:"country"`
				CountryCode string `json:"country_code"`
				Region      string `json:"region"`
				City        string `json:"city"`
			}
			if err := json.Unmarshal(b, &v); err != nil {
				return Result{}, err
			}
			if v.IP == "" {
				return Result{}, errors.New("missing ip field")
			}
			return Result{IP: v.IP, Country: v.CountryCode, CountryName: v.Country, Region: v.Region, City: v.City}, nil
		},
	},
}

// Client probes the egress IP across the rotating provider list. A single
// shared client keeps idle connections warm and — critically — never applies
// environment proxies (Proxy: nil), so the reported IP is the true direct
// egress, not whatever upstream SOCKS5/HTTP proxy is configured.
type Client struct {
	logger    *slog.Logger
	client    *http.Client
	providers []provider
	next      atomic.Uint64
}

// NewClient returns a Client over the default provider list. A nil logger
// falls back to slog.Default().
func NewClient(logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	tr := &http.Transport{
		Proxy:               nil, // direct egress: ignore HTTP(S)_PROXY
		MaxIdleConns:        4,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: probeTimeout,
	}
	return &Client{
		logger:    logger,
		client:    &http.Client{Transport: tr, Timeout: probeTimeout},
		providers: defaultProviders,
	}
}

// Probe returns the first successful result across the provider list,
// starting at the round-robin cursor. The cursor advances on every call and
// each provider is tried once in order until one answers; only when all
// providers fail does Probe return an error.
func (c *Client) Probe(ctx context.Context) (Result, error) {
	if len(c.providers) == 0 {
		return Result{}, errors.New("egressip: no providers configured")
	}
	start := int(c.next.Add(1)-1) % len(c.providers)
	var lastErr error
	for i := 0; i < len(c.providers); i++ {
		p := c.providers[(start+i)%len(c.providers)]
		r, err := c.probeOne(ctx, p)
		if err == nil {
			r.Provider = p.name
			return r, nil
		}
		lastErr = err
		c.logger.Warn("egressip probe failed", "provider", p.name, "err", err)
	}
	return Result{}, fmt.Errorf("egressip: all %d providers failed: %w", len(c.providers), lastErr)
}

// probeOne GETs a single provider and parses its response. Any failure —
// dial, TLS, non-200 status, unreadable body, parse error — is returned as
// an error so the round-robin can move on.
func (c *Client) probeOne(ctx context.Context, p provider) (Result, error) {
	reqCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, p.url, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("User-Agent", "freebuff-proxy/egressip")
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("%s: unexpected status %s", p.name, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return Result{}, err
	}
	r, err := p.parse(body)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", p.name, err)
	}
	if r.IP == "" {
		return Result{}, fmt.Errorf("%s: empty ip", p.name)
	}
	return r, nil
}

// Handler returns the JSON handler for GET /egress/ip. On success it
// reports the IP plus its country/region/city and the provider that
// answered; on total failure it reports 502 with a JSON error body.
func (c *Client) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		res, err := c.Probe(r.Context())
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		c.logger.Debug("egressip probe ok", "provider", res.Provider, "ip", res.IP, "country", res.Country)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":           true,
			"ip":           res.IP,
			"country":      res.Country,
			"country_name": res.CountryName,
			"region":       res.Region,
			"city":         res.City,
			"provider":     res.Provider,
		})
	})
}

// Wrap routes GET /egress/ip in front of next: the egress endpoint is served
// here, and every other request falls through to next unchanged. Routing in
// front of the server (instead of editing its route table) keeps this
// feature fully isolated from upstream-synced code.
func Wrap(next http.Handler, c *Client) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+endpointPath, c.Handler())
	mux.Handle("/", next)
	return mux
}

// orDefault returns s when non-empty, else def.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
