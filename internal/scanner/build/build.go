// Package build wires a scanner.Pipeline from configuration.
//
// It lives in a subpackage so the core scanner package can be imported
// by scanner/osv (and future byte-level scanners) without an import
// cycle through the builder.
package build

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/enrichment"
	"github.com/git-pkgs/proxy/internal/scanner"
	ocigate "github.com/git-pkgs/proxy/internal/scanner/oci"
	osvscanner "github.com/git-pkgs/proxy/internal/scanner/osv"
	trivyscanner "github.com/git-pkgs/proxy/internal/scanner/trivy"
)

// Pipeline returns a scanner.Pipeline configured from cfg, or nil when
// scanners are disabled / no providers are listed. Returning nil rather
// than an empty pipeline lets handler call sites use a single nil
// guard.
func Pipeline(cfg config.ScannersConfig, enrich *enrichment.Service, db *database.DB, logger *slog.Logger) (*scanner.Pipeline, error) {
	if !cfg.Enabled || len(cfg.Providers) == 0 {
		return nil, nil
	}
	policy := scanner.Policy{
		BlockAtSeverity: scanner.ParseSeverity(cfg.ResolvedBlockSeverity()),
		WarnAtSeverity:  scanner.ParseSeverity(cfg.ResolvedWarnSeverity()),
	}
	p := scanner.NewPipeline(policy, logger)
	if db != nil {
		p.Cache = scanner.NewCache(db, cfg.ParseFindingsTTL())
	}
	for i, prov := range cfg.Providers {
		s, err := buildProvider(prov, enrich)
		if err != nil {
			return nil, fmt.Errorf("scanners.providers[%d]: %w", i, err)
		}
		fm := scanner.FailOpen
		if strings.EqualFold(prov.FailMode, "closed") {
			fm = scanner.FailClosed
		}
		var timeout time.Duration
		if prov.Timeout != "" {
			if d, err := time.ParseDuration(prov.Timeout); err == nil && d > 0 {
				timeout = d
			}
		}
		p.Register(s, fm, timeout)
	}
	return p, nil
}

// OCIGate returns a synchronous OCI manifest gate when scanners are
// enabled and the provider list includes a trivy entry. Returns nil when
// scanners are disabled or no trivy provider is configured — the
// container handler treats a nil gate as "no scanning".
//
// The gate inherits the trivy provider's Server, ExtraArgs, Binary,
// FailMode, and Timeout. findings_ttl applies to verdict caching by
// manifest digest.
func OCIGate(cfg config.ScannersConfig, db *database.DB, logger *slog.Logger) (*ocigate.Gate, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	for _, prov := range cfg.Providers {
		if !strings.EqualFold(prov.Type, "trivy") {
			continue
		}
		s := trivyscanner.New(trivyscanner.Options{
			Binary:    prov.Binary,
			ExtraArgs: prov.ExtraArgs,
			Server:    prov.Server,
		})
		var timeout time.Duration
		if prov.Timeout != "" {
			if d, err := time.ParseDuration(prov.Timeout); err == nil && d > 0 {
				timeout = d
			}
		}
		return &ocigate.Gate{
			Scanner: s,
			Policy: scanner.Policy{
				BlockAtSeverity: scanner.ParseSeverity(cfg.ResolvedBlockSeverity()),
				WarnAtSeverity:  scanner.ParseSeverity(cfg.ResolvedWarnSeverity()),
			},
			DB:         db,
			Logger:     logger,
			Timeout:    timeout,
			TTL:        cfg.ParseFindingsTTL(),
			FailClosed: strings.EqualFold(prov.FailMode, "closed"),
		}, nil
	}
	return nil, nil
}

func buildProvider(prov config.ScannerProviderConfig, enrich *enrichment.Service) (scanner.Scanner, error) {
	switch strings.ToLower(prov.Type) {
	case "osv":
		if enrich == nil {
			return nil, fmt.Errorf("osv scanner requires enrichment service")
		}
		return osvscanner.New(enrich), nil
	case "trivy":
		return trivyscanner.New(trivyscanner.Options{
			Binary:    prov.Binary,
			ExtraArgs: prov.ExtraArgs,
			Server:    prov.Server,
		}), nil
	default:
		return nil, fmt.Errorf("unknown scanner type %q", prov.Type)
	}
}
