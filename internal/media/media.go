// Package media stores profile photos and derives the sizes the clients use.
//
// Storage sits behind Store so the current local-disk implementation can be
// replaced with R2 without touching the handlers — the only difference is
// where bytes land and what URL they get.
package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"

	"github.com/disintegration/imaging"
)

// Variant is a derived size. The names appear in object keys and URLs.
type Variant string

const (
	VariantFull  Variant = "full"  // 1080px — profile detail hero
	VariantCard  Variant = "card"  // 480px  — discover grid, match cards
	VariantThumb Variant = "thumb" // 160px  — avatars, inbox rows
)

var variantWidths = map[Variant]int{
	VariantFull:  1080,
	VariantCard:  480,
	VariantThumb: 160,
}

// AllVariants is the order they get generated in.
var AllVariants = []Variant{VariantFull, VariantCard, VariantThumb}

const (
	// MaxUploadBytes caps what we will decode. A decoder handed an enormous
	// image will happily allocate until the box dies, so this is a memory
	// guard as much as a policy.
	MaxUploadBytes = 10 << 20 // 10 MB
	jpegQuality    = 82
)

var ErrUnsupportedImage = errors.New("unsupported image: send a JPEG, PNG, or WebP")

// Store is the seam between the handlers and wherever bytes actually live.
type Store interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Delete(ctx context.Context, key string) error
	URL(key string) string
}

// LocalStore writes to a directory nginx serves. Interim until a domain
// exists and the bucket can sit behind Cloudflare's edge.
type LocalStore struct {
	Root    string // e.g. /var/www/velora-media
	BaseURL string // e.g. http://89.167.77.99:8091/media
}

func (s LocalStore) Put(_ context.Context, key string, data []byte, _ string) error {
	full := filepath.Join(s.Root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create media dir: %w", err)
	}
	// Write to a temp file and rename: a reader can never observe a
	// half-written image, because rename is atomic on the same filesystem.
	tmp := full + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write media: %w", err)
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit media: %w", err)
	}
	return nil
}

func (s LocalStore) Delete(_ context.Context, key string) error {
	err := os.Remove(filepath.Join(s.Root, filepath.FromSlash(key)))
	if errors.Is(err, os.ErrNotExist) {
		return nil // already gone is the desired state
	}
	return err
}

func (s LocalStore) URL(key string) string {
	return strings.TrimRight(s.BaseURL, "/") + "/" + key
}

// Derived is one processed size, ready to store.
type Derived struct {
	Variant     Variant
	Key         string
	Data        []byte
	ContentType string
	Width       int
	Height      int
}

// Process decodes an upload and produces every variant as JPEG.
//
// Two things happen here that matter beyond resizing:
//
//   - Orientation is applied from EXIF before anything else. Phone cameras
//     record portrait shots as landscape plus a rotation flag; resize first
//     and every portrait photo ends up sideways.
//   - Re-encoding drops all metadata, which is how EXIF is stripped. That is
//     a safety requirement, not housekeeping: phone photos carry GPS, and on
//     a dating app publishing someone's home coordinates is the worst kind of
//     leak. Nothing here ever copies the original bytes through.
//
// Output is JPEG only for now. WebP and AVIF need either cgo or a CDN that
// negotiates format; both arrive with the move to R2.
func Process(userID string, raw []byte) ([]Derived, error) {
	if len(raw) == 0 {
		return nil, ErrUnsupportedImage
	}
	if len(raw) > MaxUploadBytes {
		return nil, fmt.Errorf("image is larger than %d MB", MaxUploadBytes>>20)
	}

	src, err := imaging.Decode(bytes.NewReader(raw), imaging.AutoOrientation(true))
	if err != nil {
		return nil, ErrUnsupportedImage
	}

	// Content-addressed prefix: the same bytes always land on the same key, and
	// a changed photo is a new key — so responses can be cached forever with
	// no invalidation story.
	sum := sha256.Sum256(raw)
	prefix := fmt.Sprintf("%s/%s", userID, hex.EncodeToString(sum[:])[:20])

	out := make([]Derived, 0, len(AllVariants))
	for _, variant := range AllVariants {
		width := variantWidths[variant]

		var resized image.Image
		if variant == VariantFull {
			// Fit inside the box, preserving aspect ratio. imaging.Fit never
			// upscales: blowing up a small original just wastes bytes looking
			// soft.
			resized = imaging.Fit(src, width, width*2, imaging.Lanczos)
		} else {
			// Square crop for grid tiles and avatars, anchored centre.
			resized = imaging.Fill(src, width, width, imaging.Center, imaging.Lanczos)
		}

		buf := new(bytes.Buffer)
		if err := imaging.Encode(buf, resized, imaging.JPEG, imaging.JPEGQuality(jpegQuality)); err != nil {
			return nil, fmt.Errorf("encode %s: %w", variant, err)
		}

		bounds := resized.Bounds()
		out = append(out, Derived{
			Variant:     variant,
			Key:         fmt.Sprintf("%s-%s.jpg", prefix, variant),
			Data:        buf.Bytes(),
			ContentType: "image/jpeg",
			Width:       bounds.Dx(),
			Height:      bounds.Dy(),
		})
	}
	return out, nil
}

// KeyFor returns the stored key for a variant of an already-processed photo.
func KeyFor(baseKey string, variant Variant) string {
	base := strings.TrimSuffix(baseKey, filepath.Ext(baseKey))
	if idx := strings.LastIndex(base, "-"); idx > 0 {
		base = base[:idx]
	}
	return fmt.Sprintf("%s-%s.jpg", base, variant)
}
