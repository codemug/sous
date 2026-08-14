package config

import "testing"

func TestRequiresListenAndModels(t *testing.T) {
	if _, err := FromFlags([]string{"-models", "/m"}); err == nil {
		t.Fatal("accepted a missing listen address")
	}
	if _, err := FromFlags([]string{"-listen", "127.0.0.1:8080"}); err == nil {
		t.Fatal("accepted a missing model dir")
	}
}

// Sous is root-equivalent by construction, and the network boundary is the
// mitigation. Binding everything removes it.
func TestRefusesWildcardBind(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "[::]:8080"} {
		if _, err := FromFlags([]string{"-listen", addr, "-models", "/m"}); err == nil {
			t.Fatalf("accepted wildcard bind %q", addr)
		}
	}
}

func TestAcceptsTailnetAddress(t *testing.T) {
	c, err := FromFlags([]string{"-listen", "100.119.51.26:8090", "-models", "/models"})
	if err != nil {
		t.Fatalf("rejected a valid tailnet address: %v", err)
	}
	if c.Host() != "100.119.51.26" {
		t.Fatalf("Host() = %q", c.Host())
	}
	if c.Reserve != 24 {
		t.Fatalf("default reserve should be the calibrated 24 GiB, got %.0f", c.Reserve)
	}
}

func TestRejectsInvertedPortRange(t *testing.T) {
	_, err := FromFlags([]string{"-listen", "127.0.0.1:1", "-models", "/m",
		"-port-low", "200", "-port-high", "100"})
	if err == nil {
		t.Fatal("accepted an inverted port range")
	}
}
