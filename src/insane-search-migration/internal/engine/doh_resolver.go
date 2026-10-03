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
}

type dohAnswer struct {
	Type int    `json:"type"`
	Data string `json:"data"`
}

type dohResponse struct {
	Answer []dohAnswer `json:"Answer"`
}

func newDOHResolver(endpoint string) *dohResolver {
	return &dohResolver{
		endpoint: endpoint,
		client: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				TLSHandshakeTimeout: 15 * time.Second,
			},
		},
	}
}

// dohEnabled reports whether the sandbox DNS adapter is active.
func dohEnabled() bool {
	return strings.TrimSpace(os.Getenv("INSANE_DOH_URL")) != ""
}

func (r *dohResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
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
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return nil, fmt.Errorf("doh: decode: %w", err)
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
		out = append(out, addr.Unmap())
	}
	return out, nil
}

// Ensure the guard only needs LookupNetIP.
var _ Resolver = (*dohResolver)(nil)
