package controller

import (
	"fmt"
	"math"
)

const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedByValue = "borgbase-operator"
)

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

func int64Ptr(v *int32) *int64 {
	if v == nil {
		return nil
	}
	out := int64(*v)
	return &out
}
