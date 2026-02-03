package server

import (
	"embed"
	"html/template"
	"net/http"
	"path/filepath"
)

//go:embed templates/**/*.html
var templatesFS embed.FS

// Templates holds parsed templates for each page.
type Templates struct {
	pages map[string]*template.Template
}

// NewTemplates loads and parses all templates from the embedded filesystem.
func NewTemplates() (*Templates, error) {
	pages := make(map[string]*template.Template)

	// Define custom template functions
	funcMap := template.FuncMap{
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
	}

	// Get all page files
	pageFiles, err := templatesFS.ReadDir("templates/pages")
	if err != nil {
		return nil, err
	}

	for _, pageFile := range pageFiles {
		if pageFile.IsDir() {
			continue
		}

		pageName := pageFile.Name()
		pageName = pageName[:len(pageName)-len(filepath.Ext(pageName))]

		// Parse all layout files + components + this page with custom functions
		tmpl, err := template.New("").Funcs(funcMap).ParseFS(templatesFS,
			"templates/layout/*.html",
			"templates/components/*.html",
			"templates/pages/"+pageFile.Name(),
		)
		if err != nil {
			return nil, err
		}

		pages[pageName] = tmpl
	}

	return &Templates{pages: pages}, nil
}

// Render renders a page template with the given data.
func (t *Templates) Render(w http.ResponseWriter, pageName string, data any) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	tmpl, ok := t.pages[pageName]
	if !ok {
		return http.ErrAbortHandler
	}

	return tmpl.ExecuteTemplate(w, "base", data)
}
