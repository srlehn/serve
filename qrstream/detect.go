package qrstream

import (
	"fmt"
	"sort"

	"golang.org/x/text/encoding/charmap"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/common"
	"github.com/makiuchi-d/gozxing/qrcode/decoder"
	"github.com/makiuchi-d/gozxing/qrcode/detector"
)

// QR frames contain compressed binary, not text. ISO-8859-1 gives every byte
// a distinct rune so gozxing can preserve byte, numeric, and alphanumeric
// segments together in DecoderResult.Text without replacement characters.
var binaryByteEncoding = charmap.ISO8859_1

var decodeHints = map[gozxing.DecodeHintType]any{
	gozxing.DecodeHintType_TRY_HARDER:    true,
	gozxing.DecodeHintType_CHARACTER_SET: binaryByteEncoding,
}

// maxTriples bounds the finder-pattern combinations tried per bitmap.
const maxTriples = 48

// decodeBinarized detects and decodes a QR symbol from a binarized
// bitmap. Unlike gozxing's stock reader, which commits to the single
// "best" finder-pattern triple, it retries other plausible triples when
// the first choice fails - dense symbols routinely contain payload
// patterns that mimic finder patterns and mislead the detector (the
// approach of zxing's MultiFinderPatternFinder, which gozxing does not
// port).
func decodeBinarized(bmp *gozxing.BinaryBitmap) ([]byte, error) {
	matrix, err := bmp.GetBlackMatrix()
	if err != nil {
		return nil, err
	}
	finder := detector.NewFinderPatternFinder(matrix, nil)
	det := detector.NewDetector(matrix)
	dec := decoder.NewDecoder()

	info, ferr := finder.Find(decodeHints)
	if ferr == nil {
		if out, err := tryTriple(det, dec, info); err == nil {
			return out, nil
		}
	}

	centers := finder.GetPossibleCenters()
	if len(centers) < 3 {
		if ferr != nil {
			return nil, ferr
		}
		return nil, errNoQR
	}
	// Most-confirmed candidates first; require the three module-size
	// estimates of a triple to roughly agree before paying for a
	// perspective transform and Reed-Solomon decode.
	sorted := append([]*detector.FinderPattern(nil), centers...)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].GetCount() > sorted[b].GetCount() })
	attempts := 0
	for i := 0; i < len(sorted)-2; i++ {
		for j := i + 1; j < len(sorted)-1; j++ {
			for k := j + 1; k < len(sorted); k++ {
				a, b, c := sorted[i], sorted[j], sorted[k]
				lo, hi := a.GetEstimatedModuleSize(), a.GetEstimatedModuleSize()
				for _, p := range []*detector.FinderPattern{b, c} {
					lo = min(lo, p.GetEstimatedModuleSize())
					hi = max(hi, p.GetEstimatedModuleSize())
				}
				if hi > lo*1.5 {
					continue
				}
				if attempts++; attempts > maxTriples {
					return nil, errNoQR
				}
				bl, tl, tr := gozxing.ResultPoint_OrderBestPatterns(a, b, c)
				out, err := tryTriple(det, dec, detector.NewFinderPatternInfo(
					bl.(*detector.FinderPattern),
					tl.(*detector.FinderPattern),
					tr.(*detector.FinderPattern)))
				if err == nil {
					return out, nil
				}
			}
		}
	}
	if ferr != nil {
		return nil, ferr
	}
	return nil, errNoQR
}

func tryTriple(det *detector.Detector, dec *decoder.Decoder, info *detector.FinderPatternInfo) ([]byte, error) {
	dr, err := det.ProcessFinderPatternInfo(info)
	if err != nil {
		return nil, err
	}
	res, err := dec.Decode(dr.GetBits(), decodeHints)
	if err != nil {
		return nil, err
	}
	return rawFromDecoderResult(res)
}

// rawFromDecoderResult reconstructs all QR segments in their original order.
// Byte segments are decoded as ISO-8859-1 by decodeHints, while numeric and
// alphanumeric segments are ASCII, so each resulting rune maps to one byte.
func rawFromDecoderResult(res *common.DecoderResult) ([]byte, error) {
	return latin1Bytes(res.GetText())
}

func latin1Bytes(text string) ([]byte, error) {
	out := make([]byte, 0, len(text))
	for _, r := range text {
		if r > 0xff {
			return nil, fmt.Errorf("qrstream: decoded non-Latin-1 character %U", r)
		}
		out = append(out, byte(r))
	}
	return out, nil
}
