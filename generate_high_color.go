//go:build generate && jabcode_high_color

package main

//go:generate go run -tags=generate,jabcode_non_iso_encode,jabcode_high_color ./wasm
