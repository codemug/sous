// Package ui holds the server-rendered interface.
//
// Templates are compiled into the binary so deployment stays one file plus a
// systemd unit: no node toolchain, no second build artifact to version, and a
// small surface on a component that is root-equivalent by construction.
package ui

import (
	"embed"
	"html/template"
)

//go:embed templates/*.html
var files embed.FS

func Templates() (*template.Template, error) {
	return template.New("").Funcs(template.FuncMap{
		"gib": func(f float64) string {
			if f == 0 {
				return "—"
			}
			return fmtGiB(f)
		},
	}).ParseFS(files, "templates/*.html")
}
