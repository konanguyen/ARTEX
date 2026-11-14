package llmnet

import (
	"context"
	"fmt"
	"golang.org/x/net/dns/dnsmessage"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLocalAddresses(t *testing.T) {
	for _, tc := range []struct {
		ip    string
		local bool
	}{{"127.0.0.1", true}, {"10.2.3.4", true}, {"172.16.0.1", true}, {"192.168.1.2", true}, {"::1", true}, {"fd12::1", true}, {"::ffff:127.0.0.1", true}, {"8.8.8.8", false}, {"169.254.169.254", false}, {"100.64.1.1", false}, {"fe80::1", false}, {"2001:4860:4860::8888", false}} {
		if got := IsLocalAddress(netip.MustParseAddr(tc.ip)); got != tc.local {
			t.Errorf("%s=%v", tc.ip, got)
		}
	}
}
func TestStrictPolicy(t *testing.T) {
	for _, proxy := range []string{"http://127.0.0.1:8080", "socks5://localhost:1080"} {
		if _, err := NewClient(proxy, true, nil); err == nil {
			t.Fatal("strict proxy accepted")
		}
	}
	if _, err := NewClient("", true, []string{"broken"}); err == nil {
		t.Fatal("bad CIDR accepted")
	}
	client, _ := NewClient("", true, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://203.0.113.4:81", nil)
	if _, err := client.Do(req); err == nil || !strings.Contains(err.Error(), "not loopback or allowlisted") {
		t.Fatalf("public=%v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://203.0.113.4:81", http.StatusFound)
			return
		}
		fmt.Fprint(w, "local")
	}))
	defer srv.Close()
	if !IsLocalEndpoint(srv.URL) || IsLocalEndpoint("") || IsLocalEndpoint("https://203.0.113.4") {
		t.Fatal("endpoint classification")
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := client.Get(srv.URL + "/redirect"); err == nil || !strings.Contains(err.Error(), "local_llm_only") {
		t.Fatalf("redirect=%v", err)
	}
}
func TestAllowedCIDRReachesDialer(t *testing.T) {
	// A public documentation IP with a closed local test connection verifies policy
	// acceptance without sending packets: context is canceled before the actual dial.
	client, _ := NewClient("", true, []string{"203.0.113.0/24"})
	tr := client.Transport.(*http.Transport)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tr.DialContext(ctx, "tcp", net.JoinHostPort("203.0.113.4", "81"))
	if err == nil || strings.Contains(err.Error(), "not loopback or allowlisted") {
		t.Fatalf("allowlist=%v", err)
	}
}
func TestDefaultAllowsConfiguredProxy(t *testing.T) {
	client, err := NewClient("http://localhost:8080", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := client.Transport.(*http.Transport)
	req, _ := http.NewRequest("GET", "https://example.test", nil)
	u, err := tr.Proxy(req)
	if err != nil || u.Host != "localhost:8080" {
		t.Fatalf("proxy=%v %v", u, err)
	}
	client, _ = NewClient("", false, nil)
	if client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("environment proxy inherited")
	}
}

func TestLivePolicyDoesNotReuseUnrestrictedTransport(t *testing.T) {
	strict := false
	client, err := NewClientWithPolicy("http://127.0.0.1:9999", func() (bool, []string) { return strict, nil })
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*policyTransport)
	calls := 0
	transport.normal = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})
	req, _ := http.NewRequest("GET", "http://203.0.113.1/", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	strict = true
	if _, err := client.Do(req); err == nil || !strings.Contains(err.Error(), "proxies are not allowed") {
		t.Fatalf("live strict proxy error=%v", err)
	}
	if calls != 1 {
		t.Fatalf("unrestricted transport reused after strict toggle: %d", calls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDNSRebindingChecksTheSocketDestination(t *testing.T) {
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	var rebound atomic.Bool
	go func() {
		buffer := make([]byte, 1500)
		for {
			n, peer, err := socket.ReadFrom(buffer)
			if err != nil {
				return
			}
			var query dnsmessage.Message
			if query.Unpack(buffer[:n]) != nil {
				continue
			}
			reply := dnsmessage.Message{Header: dnsmessage.Header{ID: query.ID, Response: true, RecursionAvailable: true}, Questions: query.Questions}
			for _, q := range query.Questions {
				if q.Type == dnsmessage.TypeA {
					address := [4]byte{127, 0, 0, 1}
					if rebound.Load() {
						address = [4]byte{203, 0, 113, 1}
					}
					reply.Answers = append(reply.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 0}, Body: &dnsmessage.AResource{A: address}})
				}
			}
			data, err := reply.Pack()
			if err == nil {
				socket.WriteTo(data, peer)
			}
		}
	}()
	previous := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", socket.LocalAddr().String())
	}}
	defer func() { net.DefaultResolver = previous }()
	if !IsLocalEndpoint("http://dns-rebind.test:11434") {
		t.Fatal("initial private DNS rejected")
	}
	rebound.Store(true)
	client, _ := NewClient("", true, nil)
	_, err = client.Get("http://dns-rebind.test:11434")
	if err == nil || !strings.Contains(err.Error(), "not loopback or allowlisted") {
		t.Fatalf("rebound DNS destination not rejected: %v", err)
	}
}

func TestStrictDefaultsRejectUntrustedPrivateAddresses(t *testing.T) {
	client, err := NewClient("", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, address := range []string{"10.2.3.4", "172.16.0.1", "192.168.1.2", "fd12::1", "::ffff:192.168.1.2"} {
		t.Run(address, func(t *testing.T) {
			_, err := client.Transport.(*http.Transport).DialContext(ctx, "tcp", net.JoinHostPort(address, "11434"))
			if err == nil || !strings.Contains(err.Error(), "local_llm_only: LLM destination") {
				t.Fatalf("untrusted private destination was not rejected by policy: %v", err)
			}
			if !IsLocalAddress(netip.MustParseAddr(address)) || !IsLocalEndpoint("http://"+net.JoinHostPort(address, "11434")) {
				t.Fatal("strict policy changed keyless/auth local classification")
			}
		})
	}
}

func TestStrictAllowlistCannotTrustForbiddenAddresses(t *testing.T) {
	client, err := NewClient("", true, []string{"0.0.0.0/0", "::/0"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, address := range []string{"169.254.169.254", "::ffff:169.254.169.254", "fe80::1", "224.0.0.1", "::ffff:224.0.0.1", "ff02::1", "0.0.0.0", "::ffff:0.0.0.0", "::"} {
		t.Run(address, func(t *testing.T) {
			_, err := client.Transport.(*http.Transport).DialContext(ctx, "tcp", net.JoinHostPort(address, "11434"))
			if err == nil || !strings.Contains(err.Error(), "local_llm_only: LLM destination") {
				t.Fatalf("forbidden destination escaped allowlist policy: %v", err)
			}
		})
	}
}

func TestStrictRejectsLocalRedirectsWithoutForwardingBody(t *testing.T) {
	for _, live := range []bool{false, true} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			t.Run(fmt.Sprintf("live=%v/status=%d", live, status), func(t *testing.T) {
				var sinkCalls atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/start" {
						w.Header().Set("Location", "/sink")
						w.WriteHeader(status)
						return
					}
					sinkCalls.Add(1)
				}))
				defer srv.Close()
				client, err := NewClient("", true, nil)
				if live {
					client, err = NewClientWithPolicy("", func() (bool, []string) { return true, nil })
				}
				if err != nil {
					t.Fatal(err)
				}
				resp, err := client.Post(srv.URL+"/start", "application/json", strings.NewReader(`{"secret":"prompt"}`))
				if resp != nil {
					resp.Body.Close()
				}
				if err == nil || !strings.Contains(err.Error(), "redirects are not allowed") || sinkCalls.Load() != 0 {
					t.Fatalf("redirect error=%v, sink calls=%d", err, sinkCalls.Load())
				}
			})
		}
	}
}

func TestLiveMalformedPolicyCannotReuseValidPool(t *testing.T) {
	var cidrs []string
	client, err := NewClientWithPolicy("", func() (bool, []string) { return true, cidrs })
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cidrs = []string{""}
	if resp, err = client.Get(srv.URL); err == nil || !strings.Contains(err.Error(), "invalid allowed CIDR") {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatalf("malformed policy reused a valid transport: %v", err)
	}
}

func TestStrictSocketControlUsesDestinationPolicy(t *testing.T) {
	dialer := strictDialer([]netip.Prefix{netip.MustParsePrefix("192.168.1.0/24"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("ff00::/8")})
	if dialer.Control == nil {
		t.Fatal("strict dialer has no socket control")
	}
	for _, tc := range []struct {
		address string
		allowed bool
	}{
		{"127.0.0.1", true}, {"::1", true}, {"::ffff:127.0.0.1", true},
		{"192.168.1.2", true}, {"::ffff:192.168.1.2", true},
		{"192.168.2.2", false}, {"fd12::1", false}, {"203.0.113.1", false},
		{"169.254.169.254", false}, {"::ffff:169.254.169.254", false},
		{"ff02::1", false}, {"fe80::1", false}, {"::", false}, {"0.0.0.0", false},
	} {
		t.Run(tc.address, func(t *testing.T) {
			err := dialer.Control("tcp", net.JoinHostPort(tc.address, "11434"), nil)
			if (err == nil) != tc.allowed {
				t.Fatalf("socket destination allowed=%v, error=%v", tc.allowed, err)
			}
		})
	}
	for _, address := range []string{"invalid", "dns.test:11434"} {
		if err := dialer.Control("tcp", address, nil); err == nil {
			t.Fatalf("nonliteral or malformed socket destination %q accepted", address)
		}
	}
}

func TestExplicitPrivateCIDRsReachDialer(t *testing.T) {
	client, err := NewClient("", true, []string{"192.168.1.2/32", "fd12::1/128"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, address := range []string{"192.168.1.2", "::ffff:192.168.1.2", "fd12::1"} {
		_, err := client.Transport.(*http.Transport).DialContext(ctx, "tcp", net.JoinHostPort(address, "11434"))
		if err == nil || !strings.Contains(err.Error(), "operation was canceled") {
			t.Fatalf("explicitly trusted %s did not reach dialer: %v", address, err)
		}
	}
}

func TestLiveCIDRChangeUsesSeparateConnectionPool(t *testing.T) {
	cidrs := []string{"0.0.0.0/0"}
	client, err := NewClientWithPolicy("", func() (bool, []string) { return true, cidrs })
	if err != nil {
		t.Fatal(err)
	}
	peers := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { peers <- r.RemoteAddr }))
	defer srv.Close()
	for _, policy := range [][]string{{"0.0.0.0/0"}, {"127.0.0.1/32"}} {
		cidrs = policy
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if first, second := <-peers, <-peers; first == second {
		t.Fatalf("CIDR change reused connection %s", first)
	}
}

func TestLiveRedirectRejectsPolicyToggle(t *testing.T) {
	var strict atomic.Bool
	var sinkCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			strict.Store(true)
			w.Header().Set("Location", "/sink")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		sinkCalls.Add(1)
	}))
	defer srv.Close()
	client, err := NewClientWithPolicy("", func() (bool, []string) { return strict.Load(), nil })
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Post(srv.URL+"/start", "text/plain", strings.NewReader("private prompt"))
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "redirects are not allowed") || sinkCalls.Load() != 0 {
		t.Fatalf("toggle redirect error=%v, sink calls=%d", err, sinkCalls.Load())
	}
}

func TestLiveRedirectRechecksAfterCheckRedirect(t *testing.T) {
	var calls atomic.Int32
	var sinkCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "/sink")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		sinkCalls.Add(1)
	}))
	defer srv.Close()
	// Initial round trip and CheckRedirect admit the request; the next round trip
	// sees strict mode, covering the toggle window between those two hooks.
	client, err := NewClientWithPolicy("", func() (bool, []string) { return calls.Add(1) >= 3, nil })
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(srv.URL + "/start")
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "redirects are not allowed") || sinkCalls.Load() != 0 {
		t.Fatalf("late toggle redirect error=%v, sink calls=%d", err, sinkCalls.Load())
	}
}

func TestNormalRedirectBehaviorAndLimit(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("live=%v", live), func(t *testing.T) {
			var loopCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/start":
					http.Redirect(w, r, "/sink", http.StatusTemporaryRedirect)
				case "/loop":
					loopCalls.Add(1)
					http.Redirect(w, r, "/loop", http.StatusFound)
				case "/sink":
					if r.Method != http.MethodPost || r.ContentLength != int64(len("prompt")) {
						t.Errorf("normal redirect changed method/body: %s %d", r.Method, r.ContentLength)
					}
				}
			}))
			defer srv.Close()
			client, err := NewClient("", false, nil)
			if live {
				client, err = NewClientWithPolicy("", func() (bool, []string) { return false, nil })
			}
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Post(srv.URL+"/start", "text/plain", strings.NewReader("prompt"))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			resp, err = client.Get(srv.URL + "/loop")
			if resp != nil {
				resp.Body.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") || loopCalls.Load() != 10 {
				t.Fatalf("redirect limit error=%v, requests=%d", err, loopCalls.Load())
			}
		})
	}
}
