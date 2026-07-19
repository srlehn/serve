//go:build jabcode_non_iso_encode

package jabstream

import (
	"slices"
	"testing"
)

func TestHighColorSupported(t *testing.T) {
	if got := SupportedColors(); !slices.Equal(got, []int{4, 8, 16, 32, 64, 128, 256}) {
		t.Fatalf("SupportedColors = %v", got)
	}
	for _, tt := range []struct {
		plan Plan
		want int
	}{
		// measured per mode at version 20, minus 4 envelope bytes
		{Plan{Colors: 16, Version: 20}, 2511},
		{Plan{Colors: 32, Version: 20}, 3131},
		{Plan{Colors: 64, Version: 20}, 3733},
		{Plan{Colors: 128, Version: 20}, 4356},
		{Plan{Colors: 256, Version: 20}, 4981},
		{Plan{Colors: 32, Version: 8}, 763},
	} {
		if got := Capacity(tt.plan); got != tt.want {
			t.Errorf("Capacity(%+v) = %d, want %d", tt.plan, got, tt.want)
		}
	}
}
