// Package avatar validates uploaded profile pictures and re-encodes them as
// square JPEGs. Re-encoding drops EXIF and any non-image payload.
package avatar

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	MaxBytes  = 2 << 20
	Size      = 256
	maxPixels = 40_000_000 // refuse decompression bombs before decoding
)

var ErrInvalid = errors.New("Upload a JPEG, PNG, WebP or GIF image under 2 MB.")

var allowed = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

// Process reads an uploaded image and returns a Size×Size JPEG.
func Process(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil || len(data) > MaxBytes || len(data) == 0 {
		return nil, ErrInvalid
	}
	if !allowed[http.DetectContentType(data)] {
		return nil, ErrInvalid
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPixels {
		return nil, ErrInvalid
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrInvalid
	}
	// Centre-crop to a square, then scale.
	b := src.Bounds()
	side := min(b.Dx(), b.Dy())
	x0 := b.Min.X + (b.Dx()-side)/2
	y0 := b.Min.Y + (b.Dy()-side)/2
	crop := image.Rect(x0, y0, x0+side, y0+side)
	dst := image.NewRGBA(image.Rect(0, 0, Size, Size))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, crop, draw.Src, nil)

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Save writes a processed avatar under dir with a random name and returns
// that name.
func Save(dir string, jpg []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	name := hex.EncodeToString(b) + ".jpg"
	if err := os.WriteFile(filepath.Join(dir, name), jpg, 0o644); err != nil {
		return "", err
	}
	return name, nil
}

// Remove deletes a previously saved avatar; name comes from the database,
// but is still reduced to its base name.
func Remove(dir, name string) {
	if name == "" {
		return
	}
	os.Remove(filepath.Join(dir, filepath.Base(name)))
}
