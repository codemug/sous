package ui

import (
	"regexp"
	"strings"
	"testing"
)

// EVERY SELECTOR THE SCRIPT PATCHES MUST EXIST IN SOME TEMPLATE.
//
// Two of them did not. [data-committed] was never rendered anywhere, so that
// setText had never done anything since the day it was written; [data-live-dot]
// was never rendered either, which made mark() a no-op and a dropped stream
// invisible - the page just quietly stopped updating, which is precisely the
// failure the dot exists to show.
//
// Neither showed up as an error. A querySelector that matches nothing returns
// null and the guarded call does nothing, so the feature is simply absent and
// the page looks fine.
func TestEveryLivePatchTargetIsRendered(t *testing.T) {
	live, err := files.ReadFile("templates/live.html")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := files.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	var others strings.Builder
	for _, e := range entries {
		if e.Name() == "live.html" {
			continue
		}
		b, err := files.ReadFile("templates/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		others.Write(b)
	}
	markup := others.String()

	// The selectors the script looks for, as it writes them.
	re := regexp.MustCompile(`\[(data-[a-z-]+)\]`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(live), -1) {
		attr := m[1]
		if seen[attr] {
			continue
		}
		seen[attr] = true
		if !strings.Contains(markup, attr) {
			t.Errorf("live.html patches [%s] but no template renders it; "+
				"that patch has never done anything", attr)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no data- selectors found; the scan is not looking at the script")
	}
}
