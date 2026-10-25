package im

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"os"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const MaxGatewayImageDimension = 12000

// Serialize image-sized allocations across adapters. Queued images stay on
// disk; the slot covers decode, resize, encode and upload, not just decode.
//
// Every adapter shares this process-wide budget. Tests that inspect or occupy
// the slot must remain serial; unrelated tests may still run in parallel.
var imageUploadSlot = make(chan struct{}, 1)

func AcquireImageUpload(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case imageUploadSlot <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-imageUploadSlot
			return nil, err
		}
		return func() { <-imageUploadSlot }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for image upload capacity: %w", ctx.Err())
	}
}

func validateReceivedImageHeader(src io.Reader, i *ReceivedImage) error {
	cfg, format, err := image.DecodeConfig(src)
	if err != nil {
		return fmt.Errorf("decode received image header: %w", err)
	}
	mime := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp"}[format]
	if cfg.Width != i.Width || cfg.Height != i.Height || mime != i.MIME || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > MaxGatewayImageDimension || cfg.Height > MaxGatewayImageDimension || int64(cfg.Width)*int64(cfg.Height) > 36_000_000 {
		return fmt.Errorf("received image dimensions or MIME mismatch")
	}
	return nil
}

// OpenReceivedImage verifies and rewinds the same open file used by Feishu,
// without retaining its compressed original or copying it into a multipart
// buffer.
func OpenReceivedImage(i *ReceivedImage) (*os.File, error) {
	if i.Size <= 0 || i.Size > MaxGatewayImageBytes {
		return nil, fmt.Errorf("image exceeds upload limit")
	}
	f, err := os.Open(i.Path)
	if err != nil {
		return nil, fmt.Errorf("open received image: %w", err)
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(f, i.Size+1))
	if err == nil && (n != i.Size || hex.EncodeToString(digest.Sum(nil)) != i.SHA256) {
		err = fmt.Errorf("received image size or digest changed")
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err == nil {
		err = validateReceivedImageHeader(f, i)
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("validate received image: %w", err)
	}
	return f, nil
}

func ReadReceivedImage(i *ReceivedImage) ([]byte, error) {
	if i.Size <= 0 || i.Size > MaxGatewayImageBytes {
		return nil, fmt.Errorf("image exceeds upload limit")
	}
	f, err := os.Open(i.Path)
	if err != nil {
		return nil, fmt.Errorf("open received image: %w", err)
	}
	defer f.Close()
	// Read at most the advertised size plus one byte to detect growth. Reserve
	// a full padding block so WeChat can encrypt without another image buffer.
	data := make([]byte, int(i.Size)+1, int(i.Size)+aes.BlockSize)
	n, err := io.ReadFull(f, data)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, fmt.Errorf("read received image: %w", err)
	}
	data = data[:n]
	digest := sha256.Sum256(data)
	if int64(len(data)) != i.Size || hex.EncodeToString(digest[:]) != i.SHA256 {
		return nil, fmt.Errorf("received image size or digest changed")
	}
	if err := validateReceivedImageHeader(bytes.NewReader(data), i); err != nil {
		return nil, err
	}
	return data, nil
}

// ImageUploadPreview builds the platform preview, preserving alpha; the
// original remains in the Chord session.
func ImageUploadPreview(ctx context.Context, src io.Reader, limit int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	original, _, err := image.Decode(src)
	if err != nil {
		return nil, fmt.Errorf("decode upload preview: %w", err)
	}
	bounds := original.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	scale := max(width, height)
	if scale > 2048 {
		width = max(1, width*2048/scale)
		height = max(1, height*2048/scale)
	}
	for width > 0 && height > 0 {
		// Codec and resize calls cannot be preempted; check cancellation between
		// stages so a cancelled transfer does not continue processing or upload.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dst := resizeUploadPreview(original, width, height)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, dst); err != nil {
			return nil, fmt.Errorf("encode upload preview: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if encoded.Len() < limit {
			return encoded.Bytes(), nil
		}
		if width == 1 && height == 1 {
			break
		}
		width = max(1, width/2)
		height = max(1, height/2)
	}
	return nil, fmt.Errorf("image preview exceeds platform limit")
}

func resizeUploadPreview(original image.Image, width, height int) *image.RGBA {
	// RGBA selects the interpolator's fast paths and preserves alpha. An NRGBA
	// destination falls back to Set(color.Color), allocating per output pixel.
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	// ApproxBiLinear samples directly into dst, avoiding the kernel scaler's
	// destination-width * source-height float64 intermediate image.
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), original, original.Bounds(), draw.Src, nil)
	return dst
}
