package engine

// Sandbox DNS adapter (additive, opt-in).
//
// The reviewed engine resolves hostnames through the system resolver and
// fail-closes when answers are unusable. In sandboxed runtimes where local
// DNS is sinkholed (every name resolves to an internal/test address), the
// native transport cannot reach any public host even though an HTTP(S)
// egress proxy is available.
//
// Setting INSANE_DOH_URL (e.g. https://cloudflare-dns.com/dns-query) swaps
// the guard's resolver for DNS-over-HTTPS performed through the proxy taken
// from the standard HTTPS_PROXY/https_proxy environment variables
// (credential-free, as the transport already requires).
//
// Security properties are preserved:
//   - DNS answers are used only for routing. The peer is still authenticated
//     by TLS certificate verification against the original hostname (SNI),
//     so a lying or poisoned DoH answer fails closed at the handshake.
//   - The SSRF BlockedIP filtering in URLGuard.Resolve still applies to
//     every returned address.
//   - When INSANE_DOH_URL is unset, behavior is exactly the reviewed
//     default (system resolver, fail closed).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
)

type dohResolver struct {
	endpoint string
	client   *http.Client
	// configErr is set when the endpoint is misconfigured (non-HTTPS,
	// userinfo present, empty host). Lookups fail closed in that case.
	configErr error
}

type dohAnswer struct {
	Type int    `json:"type"`
	Data string `json:"data"`
}

type dohResponse struct {
	Status int         `json:"Status"`
	TC     bool        `json:"TC"`
	Answer []dohAnswer `json:"Answer"`
}

// maxDoHBodyBytes caps a single DNS-over-HTTPS JSON response. Legitimate
// answers are a few KB; anything larger is rejected.
const maxDoHBodyBytes = 1 << 20

func newDOHResolver(endpoint string) *dohResolver {
	r := &dohResolver{endpoint: endpoint}
	if u, err := url.Parse(endpoint); err != nil {
		r.configErr = fmt.Errorf("doh: bad endpoint: %w", err)
	} else if u.Scheme != "https" {
		r.configErr = fmt.Errorf("doh: endpoint must be https, got %q", u.Scheme)
	} else if u.User != nil {
		r.configErr = fmt.Errorf("doh: endpoint must not carry userinfo")
	} else if u.Hostname() == "" {
		r.configErr = fmt.Errorf("doh: endpoint has empty host")
	}
	r.client = &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			TLSHandshakeTimeout: 15 * time.Second,
		},
		// Never follow redirects: a compromised resolver must not be able
		// to steer the client to an attacker-chosen address before the
		// SSRF BlockedIP filter sees the answer IPs.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return r
}

// dohEnabled reports whether the sandbox DNS adapter is active.
func dohEnabled() bool {
	return strings.TrimSpace(os.Getenv("INSANE_DOH_URL")) != ""
}

func (r *dohResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if r.configErr != nil {
		return nil, r.configErr
	}
	var out []netip.Addr
	for _, qtype := range []string{"A", "AAAA"} {
		addrs, err := r.lookup(ctx, host, qtype)
		if err != nil {
			return nil, err
		}
		out = append(out, addrs...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("doh: no addresses for %s", host)
	}
	return out, nil
}

func (r *dohResolver) lookup(ctx context.Context, host, qtype string) ([]netip.Addr, error) {
	u, err := url.Parse(r.endpoint)
	if err != nil {
		return nil, fmt.Errorf("doh: bad endpoint: %w", err)
	}
	q := u.Query()
	q.Set("name", host)
	q.Set("type", qtype)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/dns-json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("doh query failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("doh: status %d", resp.StatusCode)
	}
	var dr dohResponse
	// Bound memory first: read at most maxDoHBodyBytes+1 and reject anything
	// that reaches the cap, so an oversized answer can never decode as a
	// partial success.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDoHBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("doh: read: %w", err)
	}
	if len(raw) > maxDoHBodyBytes {
		return nil, fmt.Errorf("doh: response exceeds %d bytes", maxDoHBodyBytes)
	}
	if err := json.Unmarshal(raw, &dr); err != nil {
		return nil, fmt.Errorf("doh: decode: %w", err)
	}
	// A non-NOERROR DNS status (e.g. SERVFAIL) or a truncated response is a
	// lookup failure, not an empty success: fail closed instead of silently
	// proceeding with partial answers.
	if dr.Status != 0 {
		return nil, fmt.Errorf("doh: dns status %d for %s", dr.Status, host)
	}
	if dr.TC {
		return nil, fmt.Errorf("doh: truncated response for %s", host)
	}
	var out []netip.Addr
	for _, a := range dr.Answer {
		if (qtype == "A" && a.Type != 1) || (qtype == "AAAA" && a.Type != 28) {
			continue
		}
		addr, err := netip.ParseAddr(a.Data)
		if err != nil || !addr.IsValid() {
			continue
		}
		addr = addr.Unmap()
		// The record type must match the address family actually parsed;
		// reject IPv6 zones, which never belong in a DNS answer here.
		if qtype == "A" && !addr.Is4() {
			continue
		}
		if qtype == "AAAA" && !addr.Is6() {
			continue
		}
		if addr.Zone() != "" {
			continue
		}
		out = append(out, addr)
	}
	return out, nil
}

// Ensure the guard only needs LookupNetIP.
var _ Resolver = (*dohResolver)(nil)
