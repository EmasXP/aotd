package avatar

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func TestProcess(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for x := range 400 {
		for y := range 200 {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 100, 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	out, err := Process(&buf)
	if err != nil {
		t.Fatal(err)
	}
	got, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := got.Bounds(); b.Dx() != Size || b.Dy() != Size {
		t.Errorf("size = %v", b)
	}
}

func TestProcessRejects(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty": nil,
		"html":  []byte("<html><script>alert(1)</script></html>"),
		"big":   bytes.Repeat([]byte{0xff}, MaxBytes+10),
		"fake":  append([]byte("\x89PNG\r\n\x1a\n"), strings.Repeat("x", 100)...),
	} {
		if _, err := Process(bytes.NewReader(data)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
