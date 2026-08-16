// Package ui holds the server-rendered interface.
//
// Templates are compiled into the binary so deployment stays one file plus a
// systemd unit: no node toolchain, no second build artifact to version, and a
// small surface on a component that is root-equivalent by construction.
package ui

import (
	"embed"
	"fmt"
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
		// gibOf converts measured bytes for display. The larder measures in
		// bytes because that is what the filesystem reports; every number in
		// this project is quoted in GiB.
		"gibOf": func(b int64) float64 {
			return float64(b) / (1024 * 1024 * 1024)
		},
		// pct sizes a segment of the pool bar. Clamped to [0,100] because a
		// mis-declared footprint should distort one segment, not blow the bar
		// past its container and hide every model after it.
		"pct": func(part, whole float64) float64 {
			if whole <= 0 {
				return 0
			}
			v := part / whole * 100
			if v < 0 {
				return 0
			}
			if v > 100 {
				return 100
			}
			return v
		},
		"sub": func(a, b float64) float64 { return a - b },
		// dur renders an uptime a person reads at a glance. Seconds matter for
		// a model that just restarted; days matter for one that has not.
		"dur": func(sec float64) string {
			switch {
			case sec < 90:
				return fmt.Sprintf("%.0fs", sec)
			case sec < 5400:
				return fmt.Sprintf("%.0fm", sec/60)
			case sec < 172800:
				return fmt.Sprintf("%.1fh", sec/3600)
			default:
				return fmt.Sprintf("%.1fd", sec/86400)
			}
		},
	}).ParseFS(files, "templates/*.html")
}
