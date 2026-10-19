package webtool

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNormalizeRawURL(t *testing.T) {
	overlongURL := "https://example.test/" + strings.Repeat("a", hardURLLength)

	tests := []struct {
		name                 string
		rawURL               string
		allowPrivateNetwork  bool
		wantNormalized       string
		wantErr              bool
		wantErrContains      string
		wantParsedHost       string
		wantParsedPath       string
		wantParsedRawQuery   string
		wantParsedNoFragment bool
	}{
		{
			name:                 "trims and strips fragment",
			rawURL:               "  https://example.test/a/b?x=1#frag  ",
			wantNormalized:       "https://example.test/a/b?x=1",
			wantParsedHost:       "example.test",
			wantParsedPath:       "/a/b",
			wantParsedRawQuery:   "x=1",
			wantParsedNoFragment: true,
		},
		{
			name:            "empty",
			rawURL:          " ",
			wantErr:         true,
			wantErrContains: "url is required",
		},
		{
			name:            "too long",
			rawURL:          overlongURL,
			wantErr:         true,
			wantErrContains: "url too long",
		},
		{
			name:            "unescaped whitespace",
			rawURL:          "https://example.test/a b",
			wantErr:         true,
			wantErrContains: "unescaped whitespace",
		},
		{
			name:            "invalid parse",
			rawURL:          "http://[::1",
			wantErr:         true,
			wantErrContains: "invalid url",
		},
		{
			name:            "relative",
			rawURL:          "/relative/path",
			wantErr:         true,
			wantErrContains: "must be absolute",
		},
		{
			name:            "unsupported scheme",
			rawURL:          "ftp://example.test/file",
			wantErr:         true,
			wantErrContains: "unsupported url scheme",
		},
		{
			name:            "missing host",
			rawURL:          "https:///path",
			wantErr:         true,
			wantErrContains: "url must include host",
		},
		{
			name:            "userinfo rejected",
			rawURL:          "https://user:pass@example.test/file",
			wantErr:         true,
			wantErrContains: "userinfo is not allowed",
		},
		{
			name:            "localhost blocked by default",
			rawURL:          "http://localhost/",
			wantErr:         true,
			wantErrContains: "blocked hostname",
		},
		{
			name:                "localhost allowed when configured",
			rawURL:              "http://localhost/",
			allowPrivateNetwork: true,
			wantNormalized:      "http://localhost/",
			wantParsedHost:      localhost,
			wantParsedPath:      "/",
		},
		{
			name:            "loopback ip blocked",
			rawURL:          "http://127.0.0.1/",
			wantErr:         true,
			wantErrContains: "blocked ip address",
		},
		{
			name:            "ipv6 loopback blocked",
			rawURL:          "http://[::1]/",
			wantErr:         true,
			wantErrContains: "blocked ip address",
		},
		{
			name:                "private ip allowed when configured",
			rawURL:              "http://127.0.0.1/",
			allowPrivateNetwork: true,
			wantNormalized:      "http://127.0.0.1/",
			wantParsedHost:      "127.0.0.1",
			wantParsedPath:      "/",
		},
		{
			name:           "documentation hostname allowed at parse validation layer",
			rawURL:         "https://example.test/path",
			wantNormalized: "https://example.test/path",
			wantParsedHost: "example.test",
			wantParsedPath: "/path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, parsed, err := normalizeRawURL(tt.rawURL, tt.allowPrivateNetwork)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeRawURL(%q) error = nil, want error", tt.rawURL)
				}
				if tt.wantErrContains != "" {
					assertContains(t, err.Error(), tt.wantErrContains)
				}
				return
			}

			if err != nil {
				t.Fatalf("normalizeRawURL(%q) error = %v", tt.rawURL, err)
			}
			if got != tt.wantNormalized {
				t.Fatalf("normalizeRawURL(%q) normalized = %q, want %q", tt.rawURL, got, tt.wantNormalized)
			}
			if parsed == nil {
				t.Fatal("parsed URL is nil")
			}
			if parsed.Hostname() != tt.wantParsedHost {
				t.Fatalf("parsed.Hostname() = %q, want %q", parsed.Hostname(), tt.wantParsedHost)
			}
			if parsed.Path != tt.wantParsedPath {
				t.Fatalf("parsed.Path = %q, want %q", parsed.Path, tt.wantParsedPath)
			}
			if parsed.RawQuery != tt.wantParsedRawQuery {
				t.Fatalf("parsed.RawQuery = %q, want %q", parsed.RawQuery, tt.wantParsedRawQuery)
			}
			if tt.wantParsedNoFragment && parsed.Fragment != "" {
				t.Fatalf("parsed.Fragment = %q, want empty", parsed.Fragment)
			}
		})
	}
}

func TestValidateParsedURL(t *testing.T) {
	longPath := "/" + strings.Repeat("a", hardURLLength)

	tests := []struct {
		name            string
		u               *url.URL
		allowPrivate    bool
		wantErrContains string
	}{
		{
			name:            "nil url",
			u:               nil,
			wantErrContains: "url is nil",
		},
		{
			name:            "unsupported scheme",
			u:               &url.URL{Scheme: "file", Host: "example.test", Path: "/x"},
			wantErrContains: "unsupported url scheme",
		},
		{
			name:            "missing host",
			u:               &url.URL{Scheme: schemeHTTPS, Path: "/x"},
			wantErrContains: "url must include host",
		},
		{
			name:            "userinfo",
			u:               &url.URL{Scheme: schemeHTTPS, Host: "example.test", User: url.User("user"), Path: "/x"},
			wantErrContains: "userinfo is not allowed",
		},
		{
			name:            "too long after stringification",
			u:               &url.URL{Scheme: schemeHTTPS, Host: "example.test", Path: longPath},
			wantErrContains: "url too long",
		},
		{
			name:            "empty hostname",
			u:               &url.URL{Scheme: schemeHTTPS, Host: "   ", Path: "/x"},
			wantErrContains: "url must include hostname",
		},
		{
			name:            "hostname invalid chars",
			u:               &url.URL{Scheme: schemeHTTPS, Host: "bad\nhost", Path: "/x"},
			wantErrContains: "url hostname contains invalid characters",
		},
		{
			name:            "blocked localhost suffix",
			u:               &url.URL{Scheme: schemeHTTPS, Host: "service.localhost", Path: "/x"},
			wantErrContains: "blocked hostname",
		},
		{
			name:            "blocked private ip",
			u:               &url.URL{Scheme: schemeHTTPS, Host: "10.0.0.1", Path: "/x"},
			wantErrContains: "blocked ip address",
		},
		{
			name:         "private ip allowed",
			u:            &url.URL{Scheme: schemeHTTPS, Host: "10.0.0.1", Path: "/x"},
			allowPrivate: true,
		},
		{
			name: "public hostname accepted",
			u:    &url.URL{Scheme: schemeHTTPS, Host: "example.test", Path: "/x"},
		},
		{
			name: "https host with port accepted",
			u:    &url.URL{Scheme: schemeHTTPS, Host: "example.test:443", Path: "/x"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateParsedURL(tt.u, tt.allowPrivate)
			if tt.wantErrContains != "" {
				if err == nil {
					t.Fatalf("validateParsedURL(%#v) error = nil, want error", tt.u)
				}
				assertContains(t, err.Error(), tt.wantErrContains)
				return
			}

			if err != nil {
				t.Fatalf("validateParsedURL(%#v) error = %v", tt.u, err)
			}
		})
	}
}

func TestBlockedHostnameAndIP(t *testing.T) {
	hostnameTests := []struct {
		host string
		want bool
	}{
		{host: localhost, want: true},
		{host: "LOCALHOST.", want: true},
		{host: "api.localhost", want: true},
		{host: "local", want: true},
		{host: "printer.local", want: true},
		{host: "example.test", want: false},
		{host: "notlocal.example", want: false},
	}

	for _, tt := range hostnameTests {
		t.Run("host "+tt.host, func(t *testing.T) {
			if got := isBlockedHostname(tt.host); got != tt.want {
				t.Fatalf("isBlockedHostname(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}

	ipTests := []struct {
		ip   string
		want bool
	}{
		{ip: "0.0.0.0", want: true},
		{ip: "10.0.0.1", want: true},
		{ip: "100.64.0.1", want: true},
		{ip: "127.0.0.1", want: true},
		{ip: "169.254.1.1", want: true},
		{ip: "172.16.0.1", want: true},
		{ip: "192.168.1.1", want: true},
		{ip: "192.0.2.1", want: true},
		{ip: "198.51.100.1", want: true},
		{ip: "203.0.113.1", want: true},
		{ip: "224.0.0.1", want: true},
		{ip: "::", want: true},
		{ip: "::1", want: true},
		{ip: "fc00::1", want: true},
		{ip: "fe80::1", want: true},
		{ip: "ff00::1", want: true},
		{ip: "2001:db8::1", want: true},
		{ip: "8.8.8.8", want: false},
		{ip: "2001:4860:4860::8888", want: false},
	}

	for _, tt := range ipTests {
		t.Run("ip "+tt.ip, func(t *testing.T) {
			if got := isBlockedIP(net.ParseIP(tt.ip)); got != tt.want {
				t.Fatalf("isBlockedIP(%q) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}

	if !isBlockedIP(nil) {
		t.Fatal("isBlockedIP(nil) = false, want true")
	}
}

func TestSecureDialContextRejectsUnsafeInputsWithoutNetworkDial(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	dial := secureDialContext(false)

	tests := []struct {
		name      string
		address   string
		errSubstr string
	}{
		{name: "bad host port", address: "bad-address", errSubstr: "missing port"},
		{name: "empty host", address: ":80", errSubstr: "empty dial host"},
		{name: "localhost blocked", address: "localhost:80", errSubstr: "blocked hostname"},
		{name: "loopback ipv4 blocked", address: "127.0.0.1:80", errSubstr: "blocked ip address"},
		{name: "loopback ipv6 blocked", address: "[::1]:80", errSubstr: "blocked ip address"},
		{name: "private ipv4 blocked", address: "192.168.0.1:80", errSubstr: "blocked ip address"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := dial(ctx, "tcp", tt.address)
			if conn != nil {
				_ = conn.Close()
				t.Fatalf("secureDialContext(%q) returned non-nil conn, want nil", tt.address)
			}
			if err == nil {
				t.Fatalf("secureDialContext(%q) error = nil, want error", tt.address)
			}
			assertContains(t, err.Error(), tt.errSubstr)
		})
	}
}

func TestNewHTTPClientConfigAndRedirectPolicy(t *testing.T) {
	proxyURL, err := url.Parse("http://proxy.example.test:8080")
	if err != nil {
		t.Fatalf("url.Parse(proxy) error = %v", err)
	}

	cfg := testConfig(func(c *webToolConfig) {
		c.proxyURL = proxyURL
		c.timeout = 50 * time.Millisecond
		c.maxRedirects = 3
		c.allowPrivateNetwork = true
	})

	client, err := newHTTPClient(cfg)
	if err != nil {
		t.Fatalf("newHTTPClient() error = %v", err)
	}
	if client.Timeout != cfg.timeout {
		t.Fatalf("client.Timeout = %v, want %v", client.Timeout, cfg.timeout)
	}

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client.Transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy == nil {
		t.Fatal("transport.Proxy is nil, want configured proxy")
	}
	proxyReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.test/", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest(proxy check) error = %v", err)
	}
	gotProxy, err := transport.Proxy(proxyReq)
	if err != nil {
		t.Fatalf("transport.Proxy() error = %v", err)
	}
	if gotProxy.String() != proxyURL.String() {
		t.Fatalf("transport.Proxy() = %q, want %q", gotProxy.String(), proxyURL.String())
	}
	if transport.DialContext == nil {
		t.Fatal("transport.DialContext is nil")
	}
	if transport.MaxResponseHeaderBytes != hardResponseHeaderBytes {
		t.Fatalf("MaxResponseHeaderBytes = %d, want %d", transport.MaxResponseHeaderBytes, hardResponseHeaderBytes)
	}
	if transport.ResponseHeaderTimeout != cfg.timeout {
		t.Fatalf("ResponseHeaderTimeout = %v, want %v", transport.ResponseHeaderTimeout, cfg.timeout)
	}

	req := &http.Request{URL: mustParseURL(t, "https://example.test/next")}
	via := make([]*http.Request, 0, 3)
	via = append(
		via,
		&http.Request{URL: mustParseURL(t, "https://example.test/1")},
		&http.Request{URL: mustParseURL(t, "https://example.test/2")},
	)
	if err := client.CheckRedirect(req, via); err != nil {
		t.Fatalf("CheckRedirect below limit error = %v", err)
	}

	via = append(via, &http.Request{URL: mustParseURL(t, "https://example.test/3")})
	if err := client.CheckRedirect(req, via); err == nil {
		t.Fatal("CheckRedirect at limit error = nil, want error")
	} else {
		assertContains(t, err.Error(), "stopped after 3 redirects")
	}
}

func TestNewHTTPClientRedirectPolicyBlocksPrivateWhenDisabled(t *testing.T) {
	cfg := testConfig(func(c *webToolConfig) {
		c.allowPrivateNetwork = false
		c.maxRedirects = 5
	})
	client, err := newHTTPClient(cfg)
	if err != nil {
		t.Fatalf("newHTTPClient() error = %v", err)
	}

	req := &http.Request{URL: mustParseURL(t, "http://127.0.0.1/")}
	via := []*http.Request{{URL: mustParseURL(t, "https://example.test/start")}}

	err = client.CheckRedirect(req, via)
	if err == nil {
		t.Fatal("CheckRedirect(private) error = nil, want error")
	}
	assertContains(t, err.Error(), "blocked ip address")
}

func TestFetchURLBytesLocalServer(t *testing.T) {
	server := newFetchTestServer(t)
	defer server.Close()

	t.Run("happy path sends headers and strips fragment", func(t *testing.T) {
		cfg := testConfig()
		client := newTestHTTPClient(t, cfg)

		resp, err := fetchURLBytes(
			t.Context(),
			server.URL+testRequestHeadersPath+"?q=1#fragment-not-sent",
			cfg,
			client,
		)
		if err != nil {
			t.Fatalf("fetchURLBytes() error = %v", err)
		}

		if resp.finalURL != server.URL+testRequestHeadersPath+"?q=1" {
			t.Fatalf("finalURL = %q, want %q", resp.finalURL, server.URL+testRequestHeadersPath+"?q=1")
		}
		assertNotContains(t, resp.finalURL, "#fragment-not-sent")
		if resp.contentType != "text/plain; charset=utf-8" {
			t.Fatalf("contentType = %q, want text/plain; charset=utf-8", resp.contentType)
		}
		body := string(resp.data)
		assertContains(t, body, "ua="+testUserAgent)
		assertContains(t, body, "accept=text/html,application/xhtml+xml,text/plain,application/pdf,image/*,*/*;q=0.8")
		assertContains(t, body, "path="+testRequestHeadersPath)
		assertContains(t, body, "rawquery=q=1")
	})

	t.Run("redirect records final URL", func(t *testing.T) {
		cfg := testConfig()
		client := newTestHTTPClient(t, cfg)

		resp, err := fetchURLBytes(t.Context(), server.URL+testRedirectPath, cfg, client)
		if err != nil {
			t.Fatalf("fetchURLBytes(redirect) error = %v", err)
		}
		if resp.finalURL != server.URL+testFinalPath {
			t.Fatalf("finalURL = %q, want %q", resp.finalURL, server.URL+testFinalPath)
		}
		if string(resp.data) != testFinalBody {
			t.Fatalf("body = %q, want %q", string(resp.data), testFinalBody)
		}
	})

	t.Run("redirect limit", func(t *testing.T) {
		cfg := testConfig(func(c *webToolConfig) {
			c.maxRedirects = 2
		})
		client := newTestHTTPClient(t, cfg)

		_, err := fetchURLBytes(t.Context(), server.URL+testRedirectLoopPath, cfg, client)
		if err == nil {
			t.Fatal("fetchURLBytes(loop) error = nil, want error")
		}
		assertContains(t, err.Error(), "stopped after 2 redirects")
	})

	t.Run("non-2xx status", func(t *testing.T) {
		cfg := testConfig()
		client := newTestHTTPClient(t, cfg)

		_, err := fetchURLBytes(t.Context(), server.URL+testStatusPath, cfg, client)
		if err == nil {
			t.Fatal("fetchURLBytes(status) error = nil, want error")
		}
		assertContains(t, err.Error(), "http status 418")
	})

	t.Run("nil client", func(t *testing.T) {
		cfg := testConfig()

		_, err := fetchURLBytes(t.Context(), server.URL+testPlainPath, cfg, nil)
		if err == nil {
			t.Fatal("fetchURLBytes(nil client) error = nil, want error")
		}
		assertContains(t, err.Error(), "nil http client")
	})

	t.Run("context canceled before request", func(t *testing.T) {
		cfg := testConfig()
		client := newTestHTTPClient(t, cfg)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := fetchURLBytes(ctx, server.URL+testPlainPath, cfg, client)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("fetchURLBytes(canceled) error = %v, want context.Canceled", err)
		}
	})

	t.Run("content length too large", func(t *testing.T) {
		cfg := testConfig(func(c *webToolConfig) {
			c.maxFetchBytes = 3
		})
		client := newTestHTTPClient(t, cfg)

		_, err := fetchURLBytes(t.Context(), server.URL+testContentLengthTooLargePath, cfg, client)
		if err == nil {
			t.Fatal("fetchURLBytes(content length too large) error = nil, want error")
		}
		if !errors.Is(err, errResponseTooLarge) {
			t.Fatalf("error = %v, want errors.Is(err, errResponseTooLarge)", err)
		}
		assertContains(t, err.Error(), "content-length 1024")
	})

	t.Run("streaming body too large", func(t *testing.T) {
		cfg := testConfig(func(c *webToolConfig) {
			c.maxFetchBytes = 5
		})
		client := newTestHTTPClient(t, cfg)

		_, err := fetchURLBytes(t.Context(), server.URL+testStreamingTooLargePath, cfg, client)
		if err == nil {
			t.Fatal("fetchURLBytes(stream too large) error = nil, want error")
		}
		if !errors.Is(err, errResponseTooLarge) {
			t.Fatalf("error = %v, want errors.Is(err, errResponseTooLarge)", err)
		}
		assertContains(t, err.Error(), "max 5 bytes")
	})
}

func mustParseURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", rawURL, err)
	}
	return u
}
