package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type catalogTransport struct {
	allowed map[string]bool
	lookup  func(context.Context, string) ([]net.IPAddr, error)
}

func catalogOrigin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}
func newCatalogTransport(origins []string) *catalogTransport {
	t := &catalogTransport{allowed: map[string]bool{}, lookup: net.DefaultResolver.LookupIPAddr}
	for _, raw := range origins {
		u, e := catalogURL(strings.TrimSpace(raw))
		if e == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" {
			t.allowed[catalogOrigin(u)] = true
		}
	}
	return t
}
func catalogIPAllowed(ip net.IP, lan bool) bool {
	if ip != nil && ip.IsLoopback() {
		return lan
	}
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	// Carrier-grade NAT contains cloud metadata addresses; never opt it in.
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "168.63.129.16/32", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "fd00:ec2::254/128", "64:ff9b::/96", "64:ff9b:1::/48", "2001:db8::/32", "2001::/32", "2002::/16"} {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return false
		}
	}
	if ip.IsPrivate() || ip.IsLoopback() {
		return lan
	}
	return true
}
func (t *catalogTransport) fetch(ctx context.Context, raw, source string, credentials *catalogCredentials) (*http.Response, error) {
	origin, e := catalogURL(source)
	if e != nil {
		return nil, e
	}
	transport := &http.Transport{Proxy: nil, ResponseHeaderTimeout: 30 * time.Second, MaxResponseHeaderBytes: 64 << 10, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	// The validated DNS answer is the literal address handed to DialContext. The
	// request's hostname is preserved for TLS verification and SNI.
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		// Permission is passed in request context so nonstandard TLS ports work too.
		allowed, _ := ctx.Value(catalogLANKey{}).(bool)
		ips, e := t.lookup(ctx, host)
		if e != nil {
			return nil, e
		}
		if len(ips) == 0 {
			return nil, ErrInvalid
		}
		for _, ip := range ips {
			if !catalogIPAllowed(ip.IP, allowed) {
				return nil, ErrInvalid
			}
		}
		d := net.Dialer{Timeout: 10 * time.Second}
		var last error
		for _, ip := range ips {
			conn, e := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if e == nil {
				return conn, nil
			}
			last = e
		}
		return nil, last
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	crossed := false
	for redirects := 0; redirects <= 5; redirects++ {
		u, e := catalogURL(raw)
		if e != nil {
			return nil, e
		}
		allowed := t.allowed[catalogOrigin(u)]
		if origin.Scheme == "https" && u.Scheme != "https" {
			return nil, ErrInvalid
		}
		if catalogOrigin(u) != catalogOrigin(origin) {
			crossed = true
		}
		req, e := http.NewRequestWithContext(context.WithValue(ctx, catalogLANKey{}, allowed), "GET", u.String(), nil)
		if e != nil {
			return nil, e
		}
		req.Header.Set("Accept", "application/opds+json, application/atom+xml, application/xml, */*")
		if credentials != nil && !crossed {
			if u.Scheme != "https" && !allowed {
				return nil, ErrInvalid
			}
			req.SetBasicAuth(credentials.Username, credentials.Password)
		}
		response, e := client.Do(req)
		if e != nil {
			return nil, e
		}
		if response.StatusCode >= 300 && response.StatusCode <= 399 {
			next, e := response.Location()
			response.Body.Close()
			if e != nil {
				return nil, e
			}
			raw = next.String()
			continue
		}
		return response, nil
	}
	return nil, errors.New("too many catalog redirects")
}

type catalogLANKey struct{}

func (s *Store) catalogSourceSecret(user, id string) (string, *catalogCredentials, error) {
	var raw string
	var encrypted []byte
	if e := s.db.QueryRow("SELECT url,secret FROM catalog_sources WHERE user_id=? AND id=? AND deleted=0", user, id).Scan(&raw, &encrypted); e != nil {
		return "", nil, e
	}
	if len(encrypted) == 0 {
		return raw, nil, nil
	}
	aead, e := s.trackingCipher()
	if e != nil {
		return "", nil, e
	}
	if len(encrypted) < aead.NonceSize() {
		return "", nil, ErrInvalid
	}
	plain, e := aead.Open(nil, encrypted[:aead.NonceSize()], encrypted[aead.NonceSize():], []byte("opds:"+user+":"+id))
	if e != nil {
		return "", nil, e
	}
	var creds catalogCredentials
	if e = json.Unmarshal(plain, &creds); e != nil {
		return "", nil, e
	}
	return raw, &creds, nil
}
func (a *api) catalogProxyRoutes(mux *http.ServeMux) {
	transport := newCatalogTransport(strings.Split(os.Getenv("QUIRE_OPDS_ALLOWED_ORIGINS"), ","))
	mux.HandleFunc("POST /v1/catalog-sources/{id}/fetch", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		source, credentials, e := a.store.catalogSourceSecret(user, r.PathValue("id"))
		if e != nil {
			failure(w, e)
			return
		}
		var in struct {
			URL  string `json:"url"`
			Kind string `json:"kind"`
		}
		if !decode(w, r, &in, 8192) {
			return
		}
		limit := int64(4 << 20)
		timeout := 30 * time.Second
		switch in.Kind {
		case "feed":
		case "cover":
			limit = 8 << 20
		case "book":
			limit = MaxStoredBookBytes
			timeout = 30 * time.Minute
		default:
			failure(w, ErrInvalid)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		response, e := transport.fetch(ctx, in.URL, source, credentials)
		if e != nil {
			respond(w, 502, map[string]string{"error": "catalog destination unavailable or prohibited"})
			return
		}
		defer response.Body.Close()
		if response.StatusCode == 401 || response.StatusCode == 403 {
			respond(w, 401, map[string]string{"error": "catalog authentication required"})
			return
		}
		if response.StatusCode != 200 || response.ContentLength > limit {
			respond(w, 502, map[string]string{"error": "catalog response unavailable or too large"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if in.Kind == "feed" {
			body, e := io.ReadAll(io.LimitReader(response.Body, limit+1))
			if e != nil || int64(len(body)) > limit {
				respond(w, 502, map[string]string{"error": "catalog feed incomplete or too large"})
				return
			}
			respond(w, 200, map[string]string{"body": string(body), "contentType": response.Header.Get("Content-Type"), "url": response.Request.URL.String()})
			return
		}
		contentType := response.Header.Get("Content-Type")
		if in.Kind == "cover" && contentType != "image/jpeg" && contentType != "image/png" && contentType != "image/webp" && contentType != "image/gif" {
			respond(w, 502, map[string]string{"error": "unsupported cover type"})
			return
		}
		// Never render an untrusted book response as active content on the server origin.
		if in.Kind == "book" {
			contentType = "application/octet-stream"
			w.Header().Set("Content-Disposition", `attachment; filename="catalog-book"`)
		}
		w.Header().Set("Content-Type", contentType)
		if response.ContentLength >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(response.ContentLength, 10))
		}
		count, e := io.Copy(w, io.LimitReader(response.Body, limit))
		if e != nil {
			panic(http.ErrAbortHandler)
		}
		if count == limit {
			var extra [1]byte
			if n, _ := response.Body.Read(extra[:]); n > 0 {
				panic(http.ErrAbortHandler)
			}
		}
	})
}
