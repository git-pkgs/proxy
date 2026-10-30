package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var (
	homebrewFormulaPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9@+._-]*$`)
	homebrewVersionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]*$`)
)

// homebrewMaxFormulaJSON bounds one formula's JSON API document, a few KB.
const homebrewMaxFormulaJSON = 1 << 20

// homebrewFormula is the part of a formula's JSON API document that names
// its bottles' image tag.
type homebrewFormula struct {
	Versions struct {
		Stable string `json:"stable"`
	} `json:"versions"`
	Revision int `json:"revision"`
	Bottle   struct {
		Stable struct {
			Rebuild int            `json:"rebuild"`
			Files   map[string]any `json:"files"`
		} `json:"stable"`
	} `json:"bottle"`
}

// BottleImage returns the homebrew/core image and tag for a formula version.
// Only the current version gets its revision and rebuild from the JSON API;
// any other version is used as the tag as given.
func (h *HomebrewHandler) BottleImage(ctx context.Context, formula, version string) (ImageRef, error) {
	if !homebrewFormulaPattern.MatchString(formula) {
		return ImageRef{}, fmt.Errorf("invalid Homebrew formula name %q", formula)
	}
	if !homebrewVersionPattern.MatchString(version) {
		return ImageRef{}, fmt.Errorf("invalid Homebrew version %q", version)
	}
	f, err := h.fetchFormula(ctx, formula)
	if err != nil {
		return ImageRef{}, err
	}
	tag := version
	if version == f.Versions.Stable {
		if len(f.Bottle.Stable.Files) == 0 {
			return ImageRef{}, fmt.Errorf("homebrew formula %s %s has no bottles", formula, version)
		}
		if f.Revision > 0 {
			tag += "_" + strconv.Itoa(f.Revision)
		}
		if f.Bottle.Stable.Rebuild > 0 {
			tag += "-" + strconv.Itoa(f.Bottle.Stable.Rebuild)
		}
	}
	name := strings.NewReplacer("@", "/", "+", "x").Replace(formula)
	return ImageRef{Name: homebrewArtifactRepository + "/" + name, References: []string{tag}}, nil
}

func (h *HomebrewHandler) fetchFormula(ctx context.Context, formula string) (*homebrewFormula, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.apiUpstream+"/formula/"+formula+".json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading the Homebrew formula: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("homebrew/core has no formula %q", formula)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("reading the Homebrew formula: upstream answered %d", resp.StatusCode)
	}
	var f homebrewFormula
	if err := json.NewDecoder(io.LimitReader(resp.Body, homebrewMaxFormulaJSON)).Decode(&f); err != nil {
		return nil, fmt.Errorf("reading the Homebrew formula: %w", err)
	}
	if f.Versions.Stable == "" {
		return nil, errors.New("reading the Homebrew formula: it has no stable version")
	}
	return &f, nil
}
