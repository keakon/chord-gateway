package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"testing"

	"golang.org/x/image/draw"
)

func TestImageUploadAdmissionCancellation(t *testing.T) {
	release, err := acquireImageUpload(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		release, err := acquireImageUpload(ctx)
		if release != nil {
			release()
		}
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting upload = %v", err)
	}
	if len(imageUploadSlot) != 1 {
		t.Fatal("cancelled waiter released the active upload")
	}
}

func TestWechatImageCancellationBeforeReading(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a := newTestWechatAdapter(t)
	a.ctx = ctx
	if err := a.SendImage("sample-chat", &ReceivedImage{Path: "missing-image"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled upload = %v", err)
	}
	if len(imageUploadSlot) != 0 {
		t.Fatal("cancelled upload retained capacity")
	}
}

func TestOpenReceivedImageValidatesAndRewinds(t *testing.T) {
	i := imageForUpload(t)
	f, err := openReceivedImage(i)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	_ = f.Close()
	want, _ := os.ReadFile(i.Path)
	if err != nil || !bytes.Equal(data, want) {
		t.Fatal("validated upload was not rewound", err)
	}
	for _, field := range []string{"digest", "size", "dimensions", "mime"} {
		t.Run(field, func(t *testing.T) {
			i := imageForUpload(t)
			switch field {
			case "digest":
				i.SHA256 = "invalid"
			case "size":
				i.Size--
			case "dimensions":
				i.Width++
			case "mime":
				i.MIME = "image/jpeg"
			}
			if f, err := openReceivedImage(i); err == nil {
				f.Close()
				t.Fatal("changed image accepted")
			}
		})
	}
}

func TestImageUploadPreviewDownscaleAndAlpha(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 4096, 2))
	for x := range 4096 {
		for y := range 2 {
			src.SetNRGBA(x, y, color.NRGBA{R: 180, G: 100, A: 80})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, src); err != nil {
		t.Fatal(err)
	}
	data, err := imageUploadPreview(t.Context(), bytes.NewReader(encoded.Bytes()), 1024)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := png.Decode(bytes.NewReader(data))
	if err != nil || preview.Bounds().Dx() != 2048 || preview.Bounds().Dy() != 1 {
		t.Fatal("preview size", err)
	}
	_, _, _, alpha := preview.At(100, 0).RGBA()
	if alpha == 0 || alpha >= 65535 {
		t.Fatal("downscale lost transparency")
	}
	if _, err := imageUploadPreview(t.Context(), bytes.NewReader(encoded.Bytes()), 1); err == nil {
		t.Fatal("impossible output limit accepted")
	}
}

func TestReadReceivedImageReusesBufferForEncryption(t *testing.T) {
	i := imageForUpload(t)
	data, err := readReceivedImage(i)
	if err != nil {
		t.Fatal(err)
	}
	if cap(data)-len(data) < aes.BlockSize {
		t.Fatal("received image does not reserve a full padding block")
	}
	ciphertext, err := encryptILinkImage(data, make([]byte, aes.BlockSize))
	if err != nil || &ciphertext[0] != &data[0] {
		t.Fatalf("image encryption allocated another image buffer: %v", err)
	}
}

func TestReadReceivedImageRejectsChangedFiles(t *testing.T) {
	for _, change := range []string{"growth", "truncation", "content"} {
		t.Run(change, func(t *testing.T) {
			i := imageForUpload(t)
			data, err := os.ReadFile(i.Path)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "growth":
				data = append(data, 0)
			case "truncation":
				data = data[:len(data)-1]
			case "content":
				data[0] ^= 1
			}
			if err := os.WriteFile(i.Path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readReceivedImage(i); err == nil {
				t.Fatal("changed image accepted")
			}
		})
	}
}

type cancellingImageReader struct {
	io.Reader
	cancel context.CancelFunc
}

func (r *cancellingImageReader) Read(p []byte) (int, error) {
	r.cancel()
	return r.Reader.Read(p)
}

func TestImageUploadPreviewCancellation(t *testing.T) {
	for _, phase := range []string{"before_decode", "during_decode"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var source io.Reader = bytes.NewReader(testImageBytes(t))
			if phase == "before_decode" {
				cancel()
				source = bytes.NewReader(nil)
			} else {
				source = &cancellingImageReader{Reader: source, cancel: cancel}
			}
			if _, err := imageUploadPreview(ctx, source, 1024); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled preview = %v", err)
			}
		})
	}
}

// Keep the old kernel as a measured baseline for the production preview resize.
func BenchmarkImagePreviewScale(b *testing.B) {
	src := image.NewNRGBA(image.Rect(0, 0, 3000, 3000))
	for i := range src.Pix {
		src.Pix[i] = byte(i * 17)
	}
	b.Run("CatmullRom", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			dst := image.NewNRGBA(image.Rect(0, 0, 1024, 1024))
			draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
		}
	})
	b.Run("Preview", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			resizeUploadPreview(src, 1024, 1024)
		}
	})
}
