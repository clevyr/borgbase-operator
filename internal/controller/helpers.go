package controller

import (
	"fmt"
	"math"
)

// Labels the operator stamps on everything it creates, and checks before it
// writes to anything it did not create in this pass.
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedByValue = "borgbase-operator"
)

// BorgBase reports repository sizes in gigabytes: currentUsage is a Float and
// quota an Int, both in GB. These helpers only render them for display in
// status, so a wrong guess about the unit is cosmetic rather than something
// that can affect a backup.
func formatUsage(gb float64) string {
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

func formatQuota(gb int64) string {
	if gb <= 0 {
		return ""
	}
	return fmt.Sprintf("%dGB", gb)
}

// int64Ptr widens an optional spec value into what the BorgBase client takes.
func int64Ptr(v *int32) *int64 {
	if v == nil {
		return nil
	}
	out := int64(*v)
	return &out
}
