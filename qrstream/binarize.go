package qrstream

import (
	"errors"
	"image"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

var errNoQR = errors.New("qrstream: no QR code found")

var pureDecodeHints = map[gozxing.DecodeHintType]any{
	gozxing.DecodeHintType_TRY_HARDER:    true,
	gozxing.DecodeHintType_PURE_BARCODE:  true,
	gozxing.DecodeHintType_CHARACTER_SET: binaryByteEncoding,
}

// decodeImage extracts the raw byte content of a QR symbol from a
// possibly low-quality photograph or video still. It tries a ladder of
// binarization strategies, each through the multi-triple detector; on
// the reference corpus of 19 hand-held photos this ladder decoded all
// frames where a single strategy lost 2.
func decodeImage(img image.Image) ([]byte, error) {
	src := gozxing.NewLuminanceSourceFromImage(img)
	var firstErr error
	for _, attempt := range []func() ([]byte, error){
		func() ([]byte, error) { return binarizedDecode(gozxing.NewHybridBinarizer(src)) },
		func() ([]byte, error) { return binarizedDecode(gozxing.NewGlobalHistgramBinarizer(src)) },
		func() ([]byte, error) {
			// global Otsu re-threshold rescues unevenly exposed photos
			osrc := gozxing.NewLuminanceSourceFromImage(otsuBinarize(img))
			return binarizedDecode(gozxing.NewHybridBinarizer(osrc))
		},
		func() ([]byte, error) {
			// gozxing's grid sampler gets imprecise below ~10 px per
			// module on high symbol versions; doubling rescues those
			usrc := gozxing.NewLuminanceSourceFromImage(scale2x(img))
			return binarizedDecode(gozxing.NewHybridBinarizer(usrc))
		},
		func() ([]byte, error) {
			// last resort for pristine, axis-aligned renders: skip
			// finder-pattern geometry entirely
			return pureDecode(gozxing.NewHybridBinarizer(src))
		},
	} {
		out, err := attempt()
		if err == nil {
			return out, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

func binarizedDecode(b gozxing.Binarizer) ([]byte, error) {
	bmp, err := gozxing.NewBinaryBitmap(b)
	if err != nil {
		return nil, err
	}
	return decodeBinarized(bmp)
}

func pureDecode(b gozxing.Binarizer) ([]byte, error) {
	bmp, err := gozxing.NewBinaryBitmap(b)
	if err != nil {
		return nil, err
	}
	res, err := qrcode.NewQRCodeReader().Decode(bmp, pureDecodeHints)
	if err != nil {
		return nil, err
	}
	return latin1Bytes(res.GetText())
}

// scale2x doubles the image with nearest-neighbor sampling.
func scale2x(img image.Image) image.Image {
	b := img.Bounds()
	out := image.NewGray(image.Rect(0, 0, 2*b.Dx(), 2*b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			v := uint8((299*(r>>8) + 587*(g>>8) + 114*(bl>>8)) / 1000)
			out.Pix[(2*y)*out.Stride+2*x] = v
			out.Pix[(2*y)*out.Stride+2*x+1] = v
			out.Pix[(2*y+1)*out.Stride+2*x] = v
			out.Pix[(2*y+1)*out.Stride+2*x+1] = v
		}
	}
	return out
}

// otsuBinarize re-thresholds the image globally (Otsu's method). It
// rescues frames where uneven exposure defeats the local binarizers.
func otsuBinarize(img image.Image) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	lum := make([]byte, w*h)
	var hist [256]int
	i := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			v := byte((299*(r>>8) + 587*(g>>8) + 114*(bl>>8)) / 1000)
			lum[i] = v
			hist[v]++
			i++
		}
	}
	total := w * h
	sum := 0.0
	for v, c := range hist {
		sum += float64(v * c)
	}
	sumB, wB := 0.0, 0
	maxVar, thresh := 0.0, 128
	for t := range 256 {
		wB += hist[t]
		if wB == 0 {
			continue
		}
		wF := total - wB
		if wF == 0 {
			break
		}
		sumB += float64(t * hist[t])
		mB := sumB / float64(wB)
		mF := (sum - sumB) / float64(wF)
		v := float64(wB) * float64(wF) * (mB - mF) * (mB - mF)
		if v > maxVar {
			maxVar, thresh = v, t
		}
	}
	out := image.NewGray(image.Rect(0, 0, w, h))
	for i, v := range lum {
		if int(v) > thresh {
			out.Pix[i] = 255
		}
	}
	return out
}
