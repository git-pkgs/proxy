package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
)

const (
	ecrTokenTimeout         = 10 * time.Second
	ecrTokenSkew            = 5 * time.Minute
	ecrDefaultTokenLifetime = 12 * time.Hour
)

// ecrTokens caches AWS ECR authorization tokens per region and refreshes them
// on demand when they expire. Tokens are obtained via the AWS SDK default
// credential chain, so IAM roles for service accounts, instance profiles, and
// environment credentials all work without extra configuration.
type ecrTokens struct {
	logger *slog.Logger

	mu    sync.Mutex
	cache map[string]ecrToken

	// getToken fetches a fresh authorization token for the given region and
	// returns the raw base64 "AWS:password" value plus its expiry. Overridable
	// in tests.
	getToken func(ctx context.Context, region string) (string, time.Time, error)
}

type ecrToken struct {
	value     string
	expiresAt time.Time
}

func newECRTokens(logger *slog.Logger) *ecrTokens {
	return &ecrTokens{
		logger:   logger,
		cache:    make(map[string]ecrToken),
		getToken: fetchECRToken,
	}
}

// header returns an Authorization header for the given region, fetching and
// caching a token on first use and after expiry. On failure it logs and
// returns empty strings so the request proceeds unauthenticated and the
// upstream 401 surfaces to the client.
func (e *ecrTokens) header(region string) (name, value string) {
	e.mu.Lock()
	if tok, ok := e.cache[region]; ok && time.Now().Before(tok.expiresAt) {
		e.mu.Unlock()
		return "Authorization", "Basic " + tok.value
	}
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), ecrTokenTimeout)
	defer cancel()

	token, expiresAt, err := e.getToken(ctx, region)
	if err != nil {
		e.logger.Error("fetching ECR authorization token", "region", region, "error", err)
		return "", ""
	}
	if token == "" {
		e.logger.Error("ECR authorization token response was empty", "region", region)
		return "", ""
	}

	e.mu.Lock()
	e.cache[region] = ecrToken{value: token, expiresAt: expiresAt.Add(-ecrTokenSkew)}
	e.mu.Unlock()

	return "Authorization", "Basic " + token
}

func fetchECRToken(ctx context.Context, region string) (string, time.Time, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return "", time.Time{}, err
	}

	out, err := ecr.NewFromConfig(cfg).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return "", time.Time{}, err
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return "", time.Time{}, nil
	}

	data := out.AuthorizationData[0]
	expiresAt := time.Now().Add(ecrDefaultTokenLifetime)
	if data.ExpiresAt != nil {
		expiresAt = *data.ExpiresAt
	}
	return *data.AuthorizationToken, expiresAt, nil
}
