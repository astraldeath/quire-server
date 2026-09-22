package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestOPDSProxyPolicy(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1/a", "http://169.254.169.254/", "http://[::1]/", "http://100.100.100.200/", "http://10.0.0.2/", "file:///secret", "http://user:pass@example.com/"} {
		if _, err := newCatalogTransport(nil).fetch(context.Background(), raw, "https://example.com", nil); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, ip := range []string{"169.254.169.254", "fe80::1", "100.100.100.200", "0.0.0.0", "224.0.0.1"} {
		if catalogIPAllowed(net.ParseIP(ip), true) {
			t.Fatal(ip)
		}
	}
	if !catalogIPAllowed(net.ParseIP("10.0.0.1"), true) || catalogIPAllowed(net.ParseIP("10.0.0.1"), false) {
		t.Fatal("LAN policy")
	}
}
func TestOPDSProxyRedirectAndDNSPinning(t *testing.T) {
	var leaked string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		io.WriteString(w, "feed")
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("missing source auth")
		}
		http.Redirect(w, r, target.URL, 302)
	}))
	defer source.Close()
	transport := newCatalogTransport([]string{source.URL, target.URL})
	response, err := transport.fetch(context.Background(), source.URL, source.URL, &catalogCredentials{"reader", "secret"})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if leaked != "" {
		t.Fatal("credentials crossed origins")
	}
	// A DNS name resolving to loopback must be rejected before connecting unless its exact origin is configured.
	u, _ := url.Parse(source.URL)
	fake := "http://catalog.invalid:" + u.Port()
	transport = newCatalogTransport(nil)
	transport.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	if _, err = transport.fetch(context.Background(), fake, fake, nil); err == nil {
		t.Fatal("DNS private address accepted")
	}
	transport = newCatalogTransport([]string{fake})
	calls := 0
	transport.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	// Allowed source still cannot redirect to an unconfigured private target.
	if _, err = transport.fetch(context.Background(), fake, fake, &catalogCredentials{"reader", "secret"}); err == nil {
		t.Fatal("private redirect accepted")
	}
	if calls != 2 {
		t.Fatal("DNS address was not pinned", calls)
	}
}
func TestOPDSProxyOwnershipAndFeedLimits(t *testing.T) {
	s, h := fixture(t)
	alice, bob := login(t, h, "alice"), login(t, h, "bob")
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, strings.Repeat("x", (4<<20)+1)) }))
	defer source.Close()
	t.Setenv("QUIRE_OPDS_ALLOWED_ORIGINS", source.URL)
	h = NewHandler(s, "https://books.example", "Test")
	request(t, h, "PUT", "/v1/catalog-sources/source", alice, map[string]any{"name": "Source", "url": source.URL, "baseRevision": 0, "operationId": "create"}, 200)
	request(t, h, "POST", "/v1/catalog-sources/source/fetch", bob, map[string]any{"url": source.URL, "kind": "feed"}, 404)
	request(t, h, "POST", "/v1/catalog-sources/source/fetch", alice, map[string]any{"url": source.URL, "kind": "feed"}, 502)
}
