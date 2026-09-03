package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// HTTPScanner adapts an external HTTP scanning service to the Scanner
// interface. It POSTs a small JSON notification describing the staged
// artifact, including a signed fetch URL; the external service is
// responsible for GETting that URL itself, running the real scan against
// those bytes, and replying with a verdict before the request's deadline.
//
// Request body:
//
//	{
//	  "ecosystem": "npm", "name": "left-pad", "version": "1.0.0",
//	  "filename": "left-pad-1.0.0.tgz", "purl": "pkg:npm/left-pad@1.0.0",
//	  "content_type": "application/octet-stream", "size": 1234,
//	  "fetch_url": "https://proxy.internal/_internal/scan-fetch?..."
//	}
//
// Response body:
//
//	{
//	  "allowed": true, "reason": "",
//	  "findings": [{"severity": "high", "title": "...", "description": "..."}]
//	}
//
// Any compliant adapter — a trivy wrapper, a clamav-rest bridge, a Wiz
// connector, or an in-house service — need only implement this contract.
type HTTPScanner struct {
	name    string
	url     string
	headers map[string]string
	client  *http.Client
}

// NewHTTPScanner creates an HTTPScanner named name that notifies url of
// staged artifacts, attaching headers to every request (e.g. for auth).
// If client is nil, http.DefaultClient is used.
func NewHTTPScanner(name, url string, headers map[string]string, client *http.Client) *HTTPScanner {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPScanner{name: name, url: url, headers: headers, client: client}
}

// Name returns the scanner's configured name.
func (s *HTTPScanner) Name() string { return s.name }

type httpScanResponse struct {
	Allowed  bool      `json:"allowed"`
	Reason   string    `json:"reason"`
	Findings []Finding `json:"findings"`
}

// Scan notifies the configured URL of req and waits for a verdict.
func (s *HTTPScanner) Scan(ctx context.Context, req Request) (Result, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Result{}, fmt.Errorf("marshal scan request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("build scan request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range s.headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return Result{}, fmt.Errorf("calling scanner %q: %w", s.name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("scanner %q returned status %d", s.name, resp.StatusCode)
	}

	var out httpScanResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Result{}, fmt.Errorf("decoding scanner %q response: %w", s.name, err)
	}

	return Result{
		Allowed:  out.Allowed,
		Reason:   out.Reason,
		Findings: out.Findings,
	}, nil
}
