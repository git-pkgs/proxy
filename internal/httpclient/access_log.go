package httpclient

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/accesslog"
)

type accessLogTransport struct {
	base      http.RoundTripper
	accessLog *accesslog.Logger
	logger    *slog.Logger
}

// NewAccessLogTransport records each upstream HTTP exchange around base.
func NewAccessLogTransport(base http.RoundTripper, log *accesslog.Logger, logger *slog.Logger) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if logger == nil {
		logger = slog.Default()
	}
	if log == nil {
		return base
	}
	return &accessLogTransport{
		base:      base,
		accessLog: log,
		logger:    logger,
	}
}

func (t *accessLogTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)

	entry := accesslog.Entry{
		Event:      accesslog.EventUpstream,
		RequestID:  accesslog.RequestID(req.Context()),
		Method:     req.Method,
		URL:        accesslog.URLWithoutSecrets(req.URL),
		DurationMS: time.Since(start).Milliseconds(),
	}
	if resp != nil {
		entry.StatusCode = resp.StatusCode
	}
	if err != nil {
		entry.Error = errorWithoutSecrets(err, req.URL)
	}
	if writeErr := t.accessLog.Write(entry); writeErr != nil {
		t.logger.Error("failed to write access log", "error", writeErr)
	}

	return resp, err
}

func errorWithoutSecrets(err error, requestURL *url.URL) string {
	message := err.Error()
	if requestURL == nil {
		return message
	}

	cleanURL := accesslog.URLWithoutSecrets(requestURL)
	for _, value := range []string{requestURL.String(), requestURL.Redacted()} {
		if value != "" {
			message = strings.ReplaceAll(message, value, cleanURL)
		}
	}
	return message
}
