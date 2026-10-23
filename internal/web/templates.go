package web

import (
	"embed"
	"html/template"
)

//go:embed templates/*.html
var templateFiles embed.FS

func ParseTemplates() (*template.Template, error) {
	return template.ParseFS(templateFiles, "templates/*.html")
}
