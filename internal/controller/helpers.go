package controller

import (
	"fmt"
	"math"
)

// BorgBase reports repository sizes in gigabytes: currentUsage is a Float and
// quota an Int, both in GB. These helpers only render them for display in
// status, so a wrong guess about the unit is cosmetic rather than something
// that can affect a backup.
func formatBytes(gb float64) string {
	if gb <= 0 {
		return "0"
	}
	if gb < 1 {
		return fmt.Sprintf("%dMB", int(math.Round(gb*1000)))
	}
	if gb >= 1000 {
		return fmt.Sprintf("%.1fTB", gb/1000)
	}
	return fmt.Sprintf("%.1fGB", gb)
}

func formatGiB(gb int64) string {
	if gb <= 0 {
		return ""
	}
	return fmt.Sprintf("%dGB", gb)
}

// quotaBytes converts a spec quota into the value BorgBase expects.
func quotaBytes(gib *int32) *int64 {
	if gib == nil {
		return nil
	}
	v := int64(*gib)
	return &v
}

func int64Ptr(v *int32) *int64 {
	if v == nil {
		return nil
	}
	out := int64(*v)
	return &out
}
