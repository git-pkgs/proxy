package server

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
	"sync"
)

//go:embed templates/**/*.html
var templatesFS embed.FS

// Templates holds lazily-parsed templates for each page.
type Templates struct {
	once  sync.Once
	pages map[string]*template.Template
	err   error
}

// load parses all templates from the embedded filesystem on first call.
func (t *Templates) load() error {
	t.once.Do(func() {
		pages := make(map[string]*template.Template)

		funcMap := template.FuncMap{
			"add":                 func(a, b int) int { return a + b },
			"sub":                 func(a, b int) int { return a - b },
			"supportedEcosystems": supportedEcosystems,
			"ecosystemBadgeClass": ecosystemBadgeClasses,
			"ecosystemPillClass":  ecosystemPillClasses,
			"ecosystemBadgeLabel": ecosystemBadgeLabel,
			"dict":                templateDict,
		}

		pageFiles, err := templatesFS.ReadDir("templates/pages")
		if err != nil {
			t.err = err
			return
		}

		for _, pageFile := range pageFiles {
			if pageFile.IsDir() {
				continue
			}

			pageName := pageFile.Name()
			pageName = pageName[:len(pageName)-len(filepath.Ext(pageName))]

			tmpl, err := template.New("").Funcs(funcMap).ParseFS(templatesFS,
				"templates/layout/*.html",
				"templates/components/*.html",
				"templates/pages/"+pageFile.Name(),
			)
			if err != nil {
				t.err = err
				return
			}

			pages[pageName] = tmpl
		}

		t.pages = pages
	})
	return t.err
}

// Render renders a page template with the given data.
func (t *Templates) Render(w http.ResponseWriter, pageName string, data any) error {
	if err := t.load(); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	tmpl, ok := t.pages[pageName]
	if !ok {
		return http.ErrAbortHandler
	}

	return tmpl.ExecuteTemplate(w, "base", data)
}

// templateDict builds a map from alternating key/value arguments, so a
// component can be invoked with named parameters rather than being handed a
// whole page struct it would have to reach through.
func templateDict(values ...any) (map[string]any, error) {
	const pair = 2

	if len(values)%pair != 0 {
		return nil, fmt.Errorf("dict: got %d arguments, want an even number of key/value pairs", len(values))
	}

	out := make(map[string]any, len(values)/pair)
	for i := 0; i < len(values); i += pair {
		key, ok := values[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %d is %T, want string", i, values[i])
		}
		out[key] = values[i+1]
	}
	return out, nil
}
