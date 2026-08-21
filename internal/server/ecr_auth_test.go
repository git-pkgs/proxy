package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
)

func testECRTokens() *ecrTokens {
	return newECRTokens(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestECRTokensCachesUntilExpiry(t *testing.T) {
	e := testECRTokens()
	calls := 0
	e.getToken = func(_ context.Context, region string) (string, time.Time, error) {
		calls++
		if region != "eu-west-1" {
			t.Errorf("region = %q, want eu-west-1", region)
		}
		return "QVdTOnNlY3JldA==", time.Now().Add(12 * time.Hour), nil
	}

	name, value := e.header("eu-west-1")
	if name != "Authorization" || value != "Basic QVdTOnNlY3JldA==" {
		t.Fatalf("header() = %q, %q", name, value)
	}

	e.header("eu-west-1")
	e.header("eu-west-1")
	if calls != 1 {
		t.Fatalf("getToken called %d times, want 1", calls)
	}
}

func TestECRTokensRefreshesAfterExpiry(t *testing.T) {
	e := testECRTokens()
	calls := 0
	e.getToken = func(_ context.Context, _ string) (string, time.Time, error) {
		calls++
		return "dG9rZW4=", time.Now().Add(time.Minute), nil
	}

	e.header("us-east-1")
	e.header("us-east-1")
	if calls != 2 {
		t.Fatalf("getToken called %d times, want 2 (token within skew window)", calls)
	}
}

func TestECRTokensPerRegion(t *testing.T) {
	e := testECRTokens()
	seen := map[string]int{}
	e.getToken = func(_ context.Context, region string) (string, time.Time, error) {
		seen[region]++
		return region + "-token", time.Now().Add(time.Hour), nil
	}

	e.header("eu-west-1")
	e.header("us-east-1")
	e.header("eu-west-1")

	if seen["eu-west-1"] != 1 || seen["us-east-1"] != 1 {
		t.Fatalf("per-region calls = %v, want one each", seen)
	}
}

func TestECRTokensConcurrentMissesShareOneFetch(t *testing.T) {
	e := testECRTokens()
	var calls atomic.Int32
	release := make(chan struct{})
	e.getToken = func(_ context.Context, _ string) (string, time.Time, error) {
		calls.Add(1)
		<-release
		return "dG9rZW4=", time.Now().Add(time.Hour), nil
	}

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			name, value := e.header("eu-west-1")
			if name != "Authorization" || value != "Basic dG9rZW4=" {
				t.Errorf("header() = %q, %q", name, value)
			}
		}()
	}

	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("getToken called %d times, want 1", got)
	}
}

func TestECRTokensErrorReturnsNoAuth(t *testing.T) {
	e := testECRTokens()
	e.getToken = func(_ context.Context, _ string) (string, time.Time, error) {
		return "", time.Time{}, errors.New("no credentials")
	}

	name, value := e.header("eu-west-1")
	if name != "" || value != "" {
		t.Fatalf("header() = %q, %q; want empty on error", name, value)
	}
}

func TestAuthForURLRoutesECRType(t *testing.T) {
	e := testECRTokens()
	e.getToken = func(_ context.Context, region string) (string, time.Time, error) {
		if region != "eu-west-1" {
			t.Errorf("region = %q, want eu-west-1", region)
		}
		return "QVdTOnNlY3JldA==", time.Now().Add(time.Hour), nil
	}

	s := &Server{
		ecr: e,
		cfg: &config.Config{
			Upstream: config.UpstreamConfig{
				Auth: map[string]config.AuthConfig{
					"https://123456789012.dkr.ecr.eu-west-1.amazonaws.com": {
						Type:   "ecr",
						Region: "eu-west-1",
					},
					"https://ghcr.io": {
						Type:  "bearer",
						Token: "ghcr-token",
					},
				},
			},
		},
	}

	name, value := s.authForURL("https://123456789012.dkr.ecr.eu-west-1.amazonaws.com/v2/my/repo/manifests/latest")
	if name != "Authorization" || value != "Basic QVdTOnNlY3JldA==" {
		t.Fatalf("ecr authForURL() = %q, %q", name, value)
	}

	name, value = s.authForURL("https://ghcr.io/v2/owner/repo/blobs/sha256:abc")
	if name != "Authorization" || value != "Bearer ghcr-token" {
		t.Fatalf("bearer authForURL() = %q, %q", name, value)
	}

	name, value = s.authForURL("https://registry-1.docker.io/v2/")
	if name != "" || value != "" {
		t.Fatalf("unmatched authForURL() = %q, %q; want empty", name, value)
	}
}
