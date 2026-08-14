// Package sysmem reads the real pool size.
//
// Never hardcode 128: MemTotal on gx10 reports 121.6 GiB, and planning against
// the nominal figure over-commits by 6 GiB before anything is deployed.
package sysmem

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

type Stats struct {
	TotalGiB     float64
	AvailableGiB float64
	SwapUsedGiB  float64
}

func Read(path string) (Stats, error) {
	f, err := os.Open(path)
	if err != nil {
		return Stats{}, err
	}
	defer f.Close()

	kb := map[string]float64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			continue
		}
		kb[strings.TrimSuffix(parts[0], ":")] = v
	}
	const toGiB = 1024 * 1024
	return Stats{
		TotalGiB:     kb["MemTotal"] / toGiB,
		AvailableGiB: kb["MemAvailable"] / toGiB,
		SwapUsedGiB:  (kb["SwapTotal"] - kb["SwapFree"]) / toGiB,
	}, sc.Err()
}
