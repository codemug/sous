package ui

import "strconv"

// fmtGiB renders a footprint at one decimal. Zero is rendered as an em dash by
// the caller, because "0.0 GiB" reads as a missing measurement when for a
// CPU-only recipe it is the correct answer.
func fmtGiB(f float64) string {
	return strconv.FormatFloat(f, 'f', 1, 64) + " GiB"
}
