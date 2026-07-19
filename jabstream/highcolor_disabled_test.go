//go:build !jabcode_non_iso_encode

package jabstream

import (
	"slices"
	"strings"
	"testing"
)

func TestHighColorUnavailableWithoutTag(t *testing.T) {
	if got := SupportedColors(); !slices.Equal(got, []int{4, 8}) {
		t.Fatalf("SupportedColors = %v", got)
	}
	for _, colors := range []int{16, 32, 64, 256} {
		if got := Capacity(Plan{Colors: colors, Version: 20}); got != 0 {
			t.Errorf("Capacity(%d colors) = %d, want 0", colors, got)
		}
		_, err := Encode(`f.bin`, []byte(`x`), &Options{Plan: Plan{Colors: colors, Version: 8}})
		if err == nil || !strings.Contains(err.Error(), `jabcode_non_iso_encode`) {
			t.Errorf("%d colors err = %v, want tag hint", colors, err)
		}
	}
}
