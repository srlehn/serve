//go:build generate && jabcode_high_color

package main

func jabWasmFlags() []string {
	return []string{`-tags=jabcode_non_iso_encode,jabcode_high_color`}
}
