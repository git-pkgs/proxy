// Package upstream provides artifact fetching from upstream package registries.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"
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

// Fetcher downloads artifacts from upstream registries.
type Fetcher struct {
	client     *http.Client
	userAgent  string
	maxRetries int
	baseDelay  time.Duration
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

// New creates a new Fetcher with the given options.
func New(opts ...Option) *Fetcher {
	f := &Fetcher{
		client: &http.Client{
			Timeout: 5 * time.Minute, // Artifacts can be large
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
			delay := f.baseDelay * time.Duration(math.Pow(2, float64(attempt-1)))
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
