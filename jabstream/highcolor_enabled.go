//go:build jabcode_non_iso_encode

package jabstream

import "github.com/srlehn/jabcode/encoder"

// This build exposes the extended high-color profile for every color
// count the jabcode encoder accepts. jabcode's measured limits on
// real captures: a phone camera on a display reads 16 colors
// reliably and 32 marginally, a laser print reads up to 32, a
// flatbed scan up to 128, and 256 decodes only pixel-exact digital
// images.
const highColorAvailable = true

func profileOptions(colors int) []encoder.Option {
	if colors > 8 {
		return []encoder.Option{encoder.WithProfile(encoder.ProfileHighColor)}
	}
	return nil
}
