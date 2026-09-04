// Package ui holds the server-rendered interface.
//
// Templates are compiled into the binary so deployment stays one file plus a
// systemd unit: no node toolchain, no second build artifact to version, and a
// small surface on a component that is root-equivalent by construction.
package ui

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"strconv"
	"time"
)

//go:embed templates/*.html
var files embed.FS

// staticFS holds the panel's client-side assets - the board's stylesheet
// and scripts. `static/*` (not a single named file) so a new .css/.js
// under static/ ships automatically; the httpapi side serves the whole
// directory under one GET /static/ route rather than one route per file.
//
//go:embed static/*
var staticFS embed.FS

// StaticFS exposes the embedded static assets for httpapi to serve (see
// server.go's "GET /static/dragdrop.js" route). The embedded paths keep
// their "static/" directory prefix, which is exactly what
// http.FileServerFS resolves a request for "/static/dragdrop.js" to once it
// strips the URL's leading slash - so no fs.Sub rewrite is needed here.
func StaticFS() embed.FS {
	return staticFS
}

func Templates() (*template.Template, error) {
	return template.New("").Funcs(template.FuncMap{
		// dict builds the argument for a partial that needs more than one
		// field. Without it a multi-field partial cannot be called at all,
		// which is why the four destructive paths each hand-rolled the same
		// confirmation form - and why they drifted into confirming four
		// different things.
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, errors.New("dict: needs an even number of arguments")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
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
		// size picks a unit that keeps the figure meaningful. A 476 KiB
		// download rendered as "0.0 GiB" reads as nothing at all - which is
		// wrong twice over, because it is both present and deletable.
		"size": func(b int64) string {
			const k, m, g = 1024.0, 1024.0 * 1024, 1024.0 * 1024 * 1024
			f := float64(b)
			switch {
			case f >= g:
				return strconv.FormatFloat(f/g, 'f', 1, 64) + " GiB"
			case f >= m:
				return strconv.FormatFloat(f/m, 'f', 0, 64) + " MiB"
			case f >= k:
				return strconv.FormatFloat(f/k, 'f', 0, 64) + " KiB"
			default:
				return strconv.FormatInt(b, 10) + " B"
			}
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
		// ago renders a timestamp as elapsed time. "3 days ago" answers the
		// question a key list is actually asked - is this still in use - which
		// an ISO timestamp makes the reader compute for themselves.
		// gib1 formats a GiB figure to one decimal. The pool is 121.6 and the
		// difference between 24 and 24.9 matters; nothing finer does.
		"gib1": func(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) },
		// pctf keeps a CSS width from carrying fifteen decimals.
		"pctf": func(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) },
		// secs renders a duration the way an operator reads one: seconds under
		// a minute, minutes and seconds above.
		// neg renders an over-commit as a positive number, so the copy can read
		// "12.4 GiB over" instead of "-12.4 GiB over".
		"neg": func(f float64) float64 { return -f },
		"secs": func(f float64) string {
			if f <= 0 {
				return "—"
			}
			if f < 60 {
				return strconv.FormatFloat(f, 'f', 0, 64) + "s"
			}
			return strconv.Itoa(int(f)/60) + "m " + strconv.Itoa(int(f)%60) + "s"
		},
		"ago": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%dh ago", int(d.Hours()))
			default:
				return fmt.Sprintf("%dd ago", int(d.Hours()/24))
			}
		},
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
