//go:build !jabcode_non_iso_encode

package jabstream

import "github.com/srlehn/jabcode/encoder"

// Without the jabcode_non_iso_encode tag only the untagged ISO
// encoder is compiled, which stops at 8 module colors.
const highColorAvailable = false

func profileOptions(int) []encoder.Option { return nil }
