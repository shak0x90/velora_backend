package media

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// The pixel cap is the only thing between an upload form and the box running
// out of memory, so it is pinned here. A decompression bomb is small on disk
// by definition — the byte limit cannot see it coming.

func smallPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 200, G: 90, B: 75, A: 255})
	buf := new(bytes.Buffer)
	if err := png.Encode(buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// forgeDimensions rewrites the IHDR width and height of a PNG and repairs the
// chunk CRC. The result is a handful of bytes claiming to be enormous — which
// is precisely what a decompression bomb looks like to a byte-size check.
func forgeDimensions(raw []byte, width, height uint32) []byte {
	out := append([]byte(nil), raw...)
	// 8-byte signature, then a chunk: 4 length, 4 type ("IHDR"), then the data,
	// which begins with width and height.
	const ihdrData = 8 + 4 + 4
	binary.BigEndian.PutUint32(out[ihdrData:], width)
	binary.BigEndian.PutUint32(out[ihdrData+4:], height)

	// The CRC covers the chunk type and its data, and the decoder refuses a
	// mismatch before it ever reports the dimensions.
	const ihdrLen = 13
	crcStart := ihdrData - 4
	crc := crc32.ChecksumIEEE(out[crcStart : crcStart+4+ihdrLen])
	binary.BigEndian.PutUint32(out[crcStart+4+ihdrLen:], crc)
	return out
}

func TestProcessAcceptsAnOrdinaryImage(t *testing.T) {
	derived, err := Process("user-1", smallPNG(t, 64, 48))
	if err != nil {
		t.Fatalf("expected a normal image to process, got %v", err)
	}
	if len(derived) != len(AllVariants) {
		t.Fatalf("got %d variants, want %d", len(derived), len(AllVariants))
	}
	for _, d := range derived {
		if len(d.Data) == 0 {
			t.Errorf("%s variant has no bytes", d.Variant)
		}
		if d.ContentType != "image/jpeg" {
			t.Errorf("%s variant is %q, want image/jpeg", d.Variant, d.ContentType)
		}
	}
}

func TestProcessRejectsADecompressionBomb(t *testing.T) {
	// Forty thousand square is 1.6 billion pixels — roughly six gigabytes once
	// decoded, from a file that fits in a network packet.
	bomb := forgeDimensions(smallPNG(t, 1, 1), 40000, 40000)
	if len(bomb) > 4096 {
		t.Fatalf("fixture is %d bytes; the whole point is that it is tiny", len(bomb))
	}

	_, err := Process("user-1", bomb)
	if err == nil {
		t.Fatal("a 1.6 gigapixel image was accepted")
	}
	if !strings.Contains(err.Error(), "megapixels") {
		t.Fatalf("expected the pixel cap to reject it, got %v", err)
	}
}

func TestProcessRejectsEmptyAndOversizedInput(t *testing.T) {
	if _, err := Process("user-1", nil); err != ErrUnsupportedImage {
		t.Errorf("empty input: got %v, want ErrUnsupportedImage", err)
	}
	if _, err := Process("user-1", []byte("this is not an image")); err != ErrUnsupportedImage {
		t.Errorf("garbage input: got %v, want ErrUnsupportedImage", err)
	}
	if _, err := Process("user-1", make([]byte, MaxUploadBytes+1)); err == nil {
		t.Error("an oversized upload was accepted")
	}
}

func TestKeyForSwapsTheVariant(t *testing.T) {
	base := "user-1/0123456789abcdef0123-full.jpg"
	if got := KeyFor(base, VariantThumb); got != "user-1/0123456789abcdef0123-thumb.jpg" {
		t.Errorf("KeyFor = %q", got)
	}
	// Idempotent: deriving from an already-derived key must not stack suffixes.
	if got := KeyFor(KeyFor(base, VariantThumb), VariantCard); got != "user-1/0123456789abcdef0123-card.jpg" {
		t.Errorf("KeyFor round trip = %q", got)
	}
}
