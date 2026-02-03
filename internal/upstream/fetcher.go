// Package upstream provides artifact fetching from upstream package registries.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/rs/dnscache"
)

var (
	ErrNotFound     = errors.New("artifact not found")
	ErrRateLimited  = errors.New("rate limited by upstream")
	ErrUpstreamDown = errors.New("upstream registry unavailable")
)

// Artifact contains the response from fetching an upstream artifact.
type Artifact struct {
	Body        io.ReadCloser
	Size        int64  // -1 if unknown
	ContentType string
	ETag        string
}

// FetcherInterface defines the interface for artifact fetchers.
type FetcherInterface interface {
	Fetch(ctx context.Context, url string) (*Artifact, error)
	Head(ctx context.Context, url string) (size int64, contentType string, err error)
}

// Fetcher downloads artifacts from upstream registries.
type Fetcher struct {
	client     *http.Client
	userAgent  string
	maxRetries int
	baseDelay  time.Duration
	authFn     func(url string) (headerName, headerValue string)
}

// Option configures a Fetcher.
type Option func(*Fetcher)

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(f *Fetcher) {
		f.client = c
	}
}

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(f *Fetcher) {
		f.userAgent = ua
	}
}

// WithMaxRetries sets the maximum retry attempts.
func WithMaxRetries(n int) Option {
	return func(f *Fetcher) {
		f.maxRetries = n
	}
}

// WithBaseDelay sets the base delay for exponential backoff.
func WithBaseDelay(d time.Duration) Option {
	return func(f *Fetcher) {
		f.baseDelay = d
	}
}

// WithAuthFunc sets a function that returns auth headers for a given URL.
// The function receives the request URL and returns a header name and value.
// Return empty strings to skip authentication for that URL.
func WithAuthFunc(fn func(url string) (headerName, headerValue string)) Option {
	return func(f *Fetcher) {
		f.authFn = fn
	}
}

// New creates a new Fetcher with the given options.
func New(opts ...Option) *Fetcher {
	// Create DNS cache with 5 minute refresh interval
	resolver := &dnscache.Resolver{}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			resolver.Refresh(true)
		}
	}()

	// Create custom dialer with DNS caching
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	f := &Fetcher{
		client: &http.Client{
			Timeout: 5 * time.Minute, // Artifacts can be large
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					host, port, err := net.SplitHostPort(addr)
					if err != nil {
						return nil, err
					}
					ips, err := resolver.LookupHost(ctx, host)
					if err != nil {
						return nil, err
					}
					for _, ip := range ips {
						conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
						if err == nil {
							return conn, nil
						}
					}
					return nil, fmt.Errorf("failed to dial any resolved IP")
				},
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
		userAgent:  "git-pkgs-proxy/1.0",
		maxRetries: 3,
		baseDelay:  500 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// Fetch downloads an artifact from the given URL.
// The caller must close the returned Artifact.Body when done.
func (f *Fetcher) Fetch(ctx context.Context, url string) (*Artifact, error) {
	var lastErr error

	for attempt := 0; attempt <= f.maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff with 10% jitter to prevent thundering herd
			delay := f.baseDelay * time.Duration(math.Pow(2, float64(attempt-1)))
			jitter := time.Duration(float64(delay) * (rand.Float64() * 0.1))
			delay += jitter

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		artifact, err := f.doFetch(ctx, url)
		if err == nil {
			return artifact, nil
		}

		lastErr = err

		// Don't retry on not found or client errors
		if errors.Is(err, ErrNotFound) {
			return nil, err
		}

		// Retry on rate limit and server errors
		if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrUpstreamDown) {
			continue
		}

		// Don't retry on other errors (network issues will be wrapped)
		return nil, err
	}

	return nil, lastErr
}

func (f *Fetcher) doFetch(ctx context.Context, url string) (*Artifact, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "*/*")

	// Add authentication header if configured
	if f.authFn != nil {
		if name, value := f.authFn(url); name != "" && value != "" {
			req.Header.Set(name, value)
		}
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching artifact: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		size := int64(-1)
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
				size = n
			}
		}

		return &Artifact{
			Body:        resp.Body,
			Size:        size,
			ContentType: resp.Header.Get("Content-Type"),
			ETag:        resp.Header.Get("ETag"),
		}, nil

	case resp.StatusCode == http.StatusNotFound:
		_ = resp.Body.Close()
		return nil, ErrNotFound

	case resp.StatusCode == http.StatusTooManyRequests:
		_ = resp.Body.Close()
		return nil, ErrRateLimited

	case resp.StatusCode >= 500:
		_ = resp.Body.Close()
		return nil, ErrUpstreamDown

	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}
}

// Head checks if an artifact exists and returns its metadata without downloading.
func (f *Fetcher) Head(ctx context.Context, url string) (size int64, contentType string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, "", fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("User-Agent", f.userAgent)

	// Add authentication header if configured
	if f.authFn != nil {
		if name, value := f.authFn(url); name != "" && value != "" {
			req.Header.Set(name, value)
		}
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("head request: %w", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return 0, "", ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	size = -1
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			size = n
		}
	}

	return size, resp.Header.Get("Content-Type"), nil
}
