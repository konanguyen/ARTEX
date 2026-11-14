// Package llmnet supplies the shared LLM HTTP transport and optional local-only policy.
package llmnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// IsLocalAddress accepts loopback, RFC1918, and IPv6 unique-local addresses.
func IsLocalAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate()
}

// IsLocalEndpoint only accepts explicit endpoints whose current destinations are all local.
func IsLocalEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !IsLocalAddress(ip) {
			return false
		}
	}
	return true
}

// strictDestinationAllowed is separate from keyless/auth endpoint classification.
func strictDestinationAllowed(ip netip.Addr, prefixes []netip.Prefix) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func strictDestinationError(host string, ip netip.Addr) error {
	return fmt.Errorf("local_llm_only: LLM destination %s (%s) is not loopback or allowlisted", host, ip)
}

func strictDialer(prefixes []netip.Prefix) *net.Dialer {
	return &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			if !strictDestinationAllowed(ip, prefixes) {
				return strictDestinationError(host, ip)
			}
			return nil
		},
	}
}

func strictRedirectError() error {
	return fmt.Errorf("local_llm_only: redirects are not allowed")
}

// NewClient preserves direct-by-default networking; strict mode rejects proxies
// and redirects, and checks the resolved IP and actual socket destination.
func NewClient(proxy string, strict bool, allowedCIDRs []string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	proxy = strings.TrimSpace(proxy)
	if strict && proxy != "" {
		return nil, fmt.Errorf("local_llm_only: proxies are not allowed")
	}
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("llm: invalid proxy: %w", err)
		}
		if u.Hostname() == "" {
			return nil, fmt.Errorf("llm: proxy requires a host")
		}
		switch u.Scheme {
		case "http", "https", "socks5":
		default:
			return nil, fmt.Errorf("llm: unsupported proxy scheme %q (use http, https or socks5)", u.Scheme)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	var prefixes []netip.Prefix
	for _, cidr := range allowedCIDRs {
		p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			return nil, fmt.Errorf("local_llm_only: invalid allowed CIDR %q: %w", cidr, err)
		}
		prefixes = append(prefixes, p)
	}
	if strict {
		dialer := strictDialer(prefixes)
		tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("local_llm_only: no addresses for %s", host)
			}
			// Reject any untrusted DNS answer, then dial the exact checked IP.
			for _, ip := range ips {
				if !strictDestinationAllowed(ip, prefixes) {
					return nil, strictDestinationError(host, ip)
				}
			}
			var last error
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				last = err
			}
			return nil, last
		}
	}
	client := &http.Client{Transport: tr}
	if strict {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return strictRedirectError() }
	}
	return client, nil
}

// policyTransport re-evaluates policy before every request, including connection
// reuse. Strict transports have separate pools, so a live toggle cannot reuse an
// external connection established before local-only mode was enabled.
type policyTransport struct {
	proxy   string
	resolve func() (bool, []string)
	normal  http.RoundTripper
	mu      sync.Mutex
	guarded map[string]http.RoundTripper
}

func (t *policyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	strict, cidrs := t.resolve()
	if !strict {
		return t.normal.RoundTrip(req)
	}
	// A toggle can occur after CheckRedirect but before this round trip.
	if req.Response != nil {
		return nil, strictRedirectError()
	}
	// Quote every element so malformed policies cannot alias a valid cached pool.
	key := fmt.Sprintf("%q", cidrs)
	t.mu.Lock()
	tr := t.guarded[key]
	if tr == nil {
		client, err := NewClient(t.proxy, true, cidrs)
		if err != nil {
			t.mu.Unlock()
			return nil, err
		}
		tr = client.Transport
		t.guarded[key] = tr
	}
	t.mu.Unlock()
	return tr.RoundTrip(req)
}

// NewClientWithPolicy applies the current policy to each request. The resolver
// must be safe for concurrent calls and returns owned or immutable CIDR slices.
func NewClientWithPolicy(proxy string, resolve func() (bool, []string)) (*http.Client, error) {
	normal, err := NewClient(proxy, false, nil)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: &policyTransport{proxy: proxy, resolve: resolve, normal: normal.Transport, guarded: map[string]http.RoundTripper{}},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			strict, _ := resolve()
			if strict {
				return strictRedirectError()
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}, nil
}
