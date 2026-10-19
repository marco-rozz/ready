package webtool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

var errResponseTooLarge = errors.New("response too large")

var blockedIPPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("2001:db8::/32"),
}

type fetchedURL struct {
	data        []byte
	contentType string
	finalURL    string
	header      http.Header
}

func newHTTPClient(cfg webToolConfig) (*http.Client, error) {
	transport := defaultTransport()

	if cfg.proxyURL != nil {
		transport.Proxy = http.ProxyURL(cfg.proxyURL)
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}

	transport.DialContext = secureDialContext(cfg.allowPrivateNetwork)
	transport.MaxResponseHeaderBytes = hardResponseHeaderBytes
	transport.ResponseHeaderTimeout = defaultHeaderTimeout
	if cfg.timeout > 0 && cfg.timeout < transport.ResponseHeaderTimeout {
		transport.ResponseHeaderTimeout = cfg.timeout
	}

	client := &http.Client{
		Timeout:   cfg.timeout,
		Transport: transport,
	}

	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= cfg.maxRedirects {
			return fmt.Errorf("stopped after %d redirects", cfg.maxRedirects)
		}
		return validateParsedURL(req.URL, cfg.allowPrivateNetwork)
	}

	return client, nil
}

func defaultTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		return base.Clone()
	}

	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: defaultDialTimeout, KeepAlive: defaultDialKeepAlive}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       defaultIdleConnTimeout,
		TLSHandshakeTimeout:   defaultTLSHandshake,
		ExpectContinueTimeout: defaultTLSHandshake,
	}
}

func fetchURLBytes(ctx context.Context, rawURL string, cfg webToolConfig, client *http.Client) (*fetchedURL, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("nil http client")
	}

	normalizedURL, _, err := normalizeRawURL(rawURL, cfg.allowPrivateNetwork)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalizedURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", cfg.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/pdf,image/*,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetchURL %s: %w", normalizedURL, err)
	}
	defer resp.Body.Close()

	finalURL := normalizedURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetchURL %s: http status %d", finalURL, resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")

	if cfg.maxFetchBytes > 0 && resp.ContentLength > cfg.maxFetchBytes {
		return nil, fmt.Errorf(
			"%w (content-length %d; max %d)",
			errResponseTooLarge,
			resp.ContentLength,
			cfg.maxFetchBytes,
		)
	}

	var reader io.Reader = resp.Body
	if cfg.maxFetchBytes > 0 {
		reader = io.LimitReader(resp.Body, cfg.maxFetchBytes+1)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if cfg.maxFetchBytes > 0 && int64(len(data)) > cfg.maxFetchBytes {
		return nil, fmt.Errorf("%w (max %d bytes)", errResponseTooLarge, cfg.maxFetchBytes)
	}

	return &fetchedURL{
		data:        data,
		contentType: contentType,
		finalURL:    finalURL,
		header:      resp.Header.Clone(),
	}, nil
}

func normalizeRawURL(rawURL string, allowPrivateNetwork bool) (string, *url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", nil, errors.New("url is required")
	}
	if len(rawURL) > hardURLLength {
		return "", nil, fmt.Errorf("url too long (max %d bytes)", hardURLLength)
	}
	if strings.ContainsAny(rawURL, " \t\r\n") {
		return "", nil, errors.New("url must not contain unescaped whitespace")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return "", nil, fmt.Errorf("invalid url %q: %w", rawURL, err)
	}
	if !u.IsAbs() {
		return "", nil, fmt.Errorf("url %q must be absolute", rawURL)
	}

	u.Fragment = ""

	if err := validateParsedURL(u, allowPrivateNetwork); err != nil {
		return "", nil, err
	}
	uStr := u.String()
	return uStr, u, nil
}

func validateParsedURL(u *url.URL, allowPrivateNetwork bool) error {
	if u == nil {
		return errors.New("url is nil")
	}
	if u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS {
		return fmt.Errorf("unsupported url scheme %q; only http and https are allowed", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("url must include host")
	}
	if u.User != nil {
		return errors.New("url userinfo is not allowed")
	}
	if len(u.String()) > hardURLLength {
		return fmt.Errorf("url too long (max %d bytes)", hardURLLength)
	}

	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return errors.New("url must include hostname")
	}
	if strings.ContainsAny(host, "\x00\r\n") {
		return errors.New("url hostname contains invalid characters")
	}

	if !allowPrivateNetwork && isBlockedHostname(host) {
		return fmt.Errorf("blocked hostname %q", host)
	}

	if ip := net.ParseIP(host); ip != nil {
		if !allowPrivateNetwork && isBlockedIP(ip) {
			return fmt.Errorf("blocked ip address %s", ip.String())
		}
	}

	return nil
}

func secureDialContext(allowPrivateNetwork bool) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   defaultDialTimeout,
		KeepAlive: defaultDialKeepAlive,
	}
	resolver := net.DefaultResolver

	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}

		host = strings.Trim(host, "[]")
		if host == "" {
			return nil, errors.New("empty dial host")
		}

		if !allowPrivateNetwork && isBlockedHostname(host) {
			return nil, fmt.Errorf("blocked hostname %q", host)
		}

		if ip := net.ParseIP(host); ip != nil {
			if !allowPrivateNetwork && isBlockedIP(ip) {
				return nil, fmt.Errorf("blocked ip address %s", ip.String())
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}

		addrs, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("no ip addresses found for %q", host)
		}

		var firstErr error
		for _, addr := range addrs {
			ip := addr.IP
			if ip == nil {
				continue
			}
			if !allowPrivateNetwork && isBlockedIP(ip) {
				if firstErr == nil {
					firstErr = fmt.Errorf("blocked ip address %s for hostname %q", ip.String(), host)
				}
				continue
			}

			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			if firstErr == nil {
				firstErr = err
			}
		}

		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("no usable ip addresses found for %q", host)
	}
}

func isBlockedHostname(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	return host == localhost ||
		strings.HasSuffix(host, ".localhost") ||
		host == "local" ||
		strings.HasSuffix(host, ".local")
}

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() {
		return true
	}

	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()

	for _, prefix := range blockedIPPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
