package sysmem

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// Values are gx10's real /proc/meminfo shape: 121.6 GiB total, 16 GiB swap.
const fixture = `MemTotal:       127512345 kB
MemFree:         47000000 kB
MemAvailable:    86300000 kB
Cached:          39900000 kB
SwapTotal:       16777216 kB
SwapFree:        12373196 kB
`

func TestReadParsesMeminfo(t *testing.T) {
	p := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(p, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.TotalGiB-121.6) > 0.1 {
		t.Fatalf("total: want ~121.6 GiB, got %.2f", got.TotalGiB)
	}
	if math.Abs(got.AvailableGiB-82.3) > 0.1 {
		t.Fatalf("available: want ~82.3 GiB, got %.2f", got.AvailableGiB)
	}
	// Swap used is derived, not reported: total minus free.
	if math.Abs(got.SwapUsedGiB-4.2) > 0.1 {
		t.Fatalf("swap used: want ~4.2 GiB, got %.2f", got.SwapUsedGiB)
	}
}

func TestReadOnRealHostDoesNotError(t *testing.T) {
	got, err := Read("/proc/meminfo")
	if err != nil {
		t.Skipf("no /proc/meminfo here: %v", err)
	}
	if got.TotalGiB <= 0 {
		t.Fatalf("implausible total: %.2f GiB", got.TotalGiB)
	}
}

func TestMissingFileErrors(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("missing file must error")
	}
}
