package webtool

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const (
	defaultUserAgent       = "FlexiGPT-LLMTools/1.0 (+https://github.com/flexigpt/llmtools-go)"
	defaultHTTPTimeout     = 30 * time.Second
	defaultMaxRedirects    = 5
	defaultFetchMaxLength  = 5000
	defaultMaxFetchBytes   = 16 * 1024 * 1024
	defaultDialTimeout     = 30 * time.Second
	defaultDialKeepAlive   = 30 * time.Second
	defaultHeaderTimeout   = 15 * time.Second
	defaultIdleConnTimeout = 90 * time.Second
	defaultTLSHandshake    = 10 * time.Second

	hardUserAgentLength           = 512
	hardRedirects                 = 10
	hardFetchMaxLength            = 100000
	hardFetchBytes          int64 = 16 * 1024 * 1024
	hardURLLength                 = 4096
	hardResponseHeaderBytes       = 1 << 20

	schemeHTTPS = "https"
	schemeHTTP  = "http"
	localhost   = "localhost"
)

type webToolConfig struct {
	userAgent           string
	timeout             time.Duration
	maxFetchBytes       int64
	maxRedirects        int
	allowPrivateNetwork bool
	proxyURL            *url.URL
}

// WebTool is an instance-owned HTTP fetchURL tool.
//
// Default security posture is conservative:
// - only http/https URLs are allowed
// - localhost, private, link-local, multicast and reserved IP ranges are blocked
// - redirects are revalidated
// - response bytes are bounded.
type WebTool struct {
	mu     sync.RWMutex
	cfg    webToolConfig
	client *http.Client
}

type WebToolOption func(*WebTool) error

// WithUserAgent sets the User-Agent sent by fetchURL requests.
func WithUserAgent(userAgent string) WebToolOption {
	return func(wt *WebTool) error {
		userAgent = strings.TrimSpace(userAgent)
		if userAgent == "" {
			return errors.New("user-agent cannot be empty")
		}
		if len(userAgent) > hardUserAgentLength {
			return fmt.Errorf("user-agent too long (max %d bytes)", hardUserAgentLength)
		}
		if strings.ContainsAny(userAgent, "\r\n") {
			return errors.New("user-agent cannot contain CR/LF")
		}
		wt.cfg.userAgent = userAgent
		return nil
	}
}

// WithHTTPTimeout sets the HTTP client timeout.
//
// A zero timeout disables the client-level timeout, but callers should still
// use context deadlines or registry call timeouts.
func WithHTTPTimeout(timeout time.Duration) WebToolOption {
	return func(wt *WebTool) error {
		if timeout < 0 {
			return errors.New("http timeout cannot be negative")
		}
		wt.cfg.timeout = timeout
		return nil
	}
}

// WithMaxFetchBytes sets the maximum bytes downloaded for one fetch.
//
// It cannot exceed maxBytes, so the web tool remains aligned
// with local file read safety limits.
func WithMaxFetchBytes(maxBytes int64) WebToolOption {
	return func(wt *WebTool) error {
		if maxBytes <= 0 {
			return errors.New("max fetch bytes must be positive")
		}
		if maxBytes > hardFetchBytes {
			return fmt.Errorf(
				"max fetch bytes too large (%d; max %d)",
				maxBytes,
				hardFetchBytes,
			)
		}
		wt.cfg.maxFetchBytes = maxBytes
		return nil
	}
}

// WithMaxRedirects sets the maximum number of redirects.
//
// Use 0 to disable redirects.
func WithMaxRedirects(maxRedirects int) WebToolOption {
	return func(wt *WebTool) error {
		if maxRedirects < 0 {
			return errors.New("max redirects cannot be negative")
		}
		if maxRedirects > hardRedirects {
			return fmt.Errorf("max redirects too large (%d; max %d)", maxRedirects, hardRedirects)
		}
		wt.cfg.maxRedirects = maxRedirects
		return nil
	}
}

// WithAllowPrivateNetwork allows fetching private/internal network addresses.
//
// Keep this false for autonomous LLM usage unless the host application has its
// own network sandbox.
func WithAllowPrivateNetwork(allow bool) WebToolOption {
	return func(wt *WebTool) error {
		wt.cfg.allowPrivateNetwork = allow
		return nil
	}
}

// WithProxyURL configures an HTTP/HTTPS proxy.
func WithProxyURL(proxyRawURL string) WebToolOption {
	return func(wt *WebTool) error {
		proxyRawURL = strings.TrimSpace(proxyRawURL)
		if proxyRawURL == "" {
			wt.cfg.proxyURL = nil
			return nil
		}

		u, err := url.Parse(proxyRawURL)
		if err != nil {
			return fmt.Errorf("invalid proxy url: %w", err)
		}
		if u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS {
			return errors.New("proxy url scheme must be http or https")
		}
		if u.Host == "" {
			return errors.New("proxy url must include host")
		}

		wt.cfg.proxyURL = u
		return nil
	}
}

func NewWebTool(opts ...WebToolOption) (*WebTool, error) {
	wt := &WebTool{
		cfg: webToolConfig{
			userAgent:           defaultUserAgent,
			timeout:             defaultHTTPTimeout,
			maxFetchBytes:       defaultMaxFetchBytes,
			maxRedirects:        defaultMaxRedirects,
			allowPrivateNetwork: false,
			proxyURL:            nil,
		},
	}

	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(wt); err != nil {
			return nil, err
		}
	}

	client, err := newHTTPClient(wt.cfg)
	if err != nil {
		return nil, err
	}
	wt.client = client

	return wt, nil
}

func (wt *WebTool) FetchURLTool() spec.Tool {
	return toolutil.CloneTool(fetchURLTool)
}

func (wt *WebTool) FetchURL(ctx context.Context, args FetchURLArgs) ([]spec.ToolOutputUnion, error) {
	return toolutil.WithRecoveryResp(func() ([]spec.ToolOutputUnion, error) {
		if wt == nil {
			return nil, errors.New("nil web tool")
		}
		if ctx == nil {
			ctx = context.Background()
		}

		cfg, client := wt.snapshot()
		return fetchURL(ctx, args, cfg, client)
	})
}

func (wt *WebTool) snapshot() (webToolConfig, *http.Client) {
	wt.mu.RLock()
	defer wt.mu.RUnlock()
	return wt.cfg, wt.client
}
