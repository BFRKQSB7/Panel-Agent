package web

import (
	"embed"
	"html/template"
)

//go:embed templates/*.html static/tokens.js
var templateFiles embed.FS

func ParseTemplates() (*template.Template, error) {
	return template.ParseFS(templateFiles, "templates/*.html")
}

func TokenScript() []byte {
	content, _ := templateFiles.ReadFile("static/tokens.js")
	return content
}
