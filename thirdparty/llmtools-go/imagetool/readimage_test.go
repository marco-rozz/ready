package imagetool

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

func TestReadImage(t *testing.T) {
	tmpDir := t.TempDir()
	imgPath := filepath.Join(tmpDir, "img.png")
	rawPNG := writePNG(t, imgPath, 8, 6)
	fi, err := os.Stat(imgPath)
	if err != nil {
		t.Fatalf("stat image: %v", err)
	}

	missingPath := filepath.Join(tmpDir, "missing.png")

	textPath := filepath.Join(tmpDir, "notimg.txt")
	if err := os.WriteFile(textPath, []byte("plain"), 0o600); err != nil {
		t.Fatalf("write text: %v", err)
	}

	badPNGPath := filepath.Join(tmpDir, "bad.png")
	if err := os.WriteFile(badPNGPath, []byte("not a png"), 0o600); err != nil {
		t.Fatalf("write bad png: %v", err)
	}

	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()

	expiredCtx, cancel2 := context.WithDeadline(t.Context(), time.Now().Add(-1*time.Second))
	cancel2()

	type tc struct {
		name      string
		ctx       context.Context
		args      ReadImageArgs
		wantErr   bool
		wantErrIs error
		check     func(t *testing.T, out *ReadImageOut)
	}
	tests := []tc{
		{
			name: "metadata only",
			ctx:  t.Context(),
			args: ReadImageArgs{
				Path:              imgPath,
				IncludeBase64Data: false,
			},
			check: func(t *testing.T, out *ReadImageOut) {
				t.Helper()
				if out == nil {
					t.Fatalf("expected non-nil out")
				}
				if !out.Exists {
					t.Fatalf("expected Exists=true")
				}
				cleanpath := filepath.Clean(imgPath)
				if !strings.Contains(out.Path, cleanpath) {
					// We need to check contains as output will be cleaned and with system aliases modified.
					t.Fatalf(
						"Path mismatch: got %q want %q",
						out.Path,
						cleanpath,
					)
				}
				if out.Name != filepath.Base(imgPath) {
					t.Fatalf("Name mismatch: got %q want %q", out.Name, filepath.Base(imgPath))
				}
				if out.Width != 8 || out.Height != 6 {
					t.Fatalf("unexpected dimensions: width=%d height=%d", out.Width, out.Height)
				}
				if out.Format != "png" {
					t.Fatalf("Format mismatch: got %q want %q", out.Format, "png")
				}
				if out.MIMEType != "image/png" {
					t.Fatalf("MIMEType mismatch: got %q want %q", out.MIMEType, "image/png")
				}
				if out.SizeBytes != fi.Size() {
					t.Fatalf("SizeBytes mismatch: got %d want %d", out.SizeBytes, fi.Size())
				}
				if out.ModTime == nil {
					t.Fatalf("expected ModTime to be set")
				}
				if !out.ModTime.Equal(fi.ModTime()) {
					t.Fatalf("ModTime mismatch: got %v want %v", out.ModTime, fi.ModTime())
				}
				if out.Base64Data != "" {
					t.Fatalf("expected Base64Data empty when IncludeBase64Data=false")
				}
			},
		},
		{
			name: "metadata + base64 (bytes round-trip)",
			ctx:  t.Context(),
			args: ReadImageArgs{
				Path:              imgPath,
				IncludeBase64Data: true,
			},
			check: func(t *testing.T, out *ReadImageOut) {
				t.Helper()
				if out == nil {
					t.Fatalf("expected non-nil out")
				}
				if !out.Exists {
					t.Fatalf("expected Exists=true")
				}
				if out.Base64Data == "" {
					t.Fatalf("expected Base64Data to be set when IncludeBase64Data=true")
				}
				got, err := base64.StdEncoding.DecodeString(out.Base64Data)
				if err != nil {
					t.Fatalf("base64 decode: %v", err)
				}
				if !bytes.Equal(got, rawPNG) {
					t.Fatalf("decoded Base64Data does not match file bytes")
				}
			},
		},
		{
			name: "non-existent path is not an error (no base64)",
			ctx:  t.Context(),
			args: ReadImageArgs{
				Path:              missingPath,
				IncludeBase64Data: false,
			},
			check: func(t *testing.T, out *ReadImageOut) {
				t.Helper()
				if out == nil {
					t.Fatalf("expected non-nil out")
				}
				if out.Exists {
					t.Fatalf("expected Exists=false for missing file")
				}
				if out.Base64Data != "" {
					t.Fatalf("expected Base64Data empty for missing file")
				}
				// Metadata should be absent/zero values for missing files.
				if out.SizeBytes != 0 {
					t.Fatalf("expected SizeBytes=0 for missing file, got %d", out.SizeBytes)
				}
				if out.ModTime != nil {
					t.Fatalf("expected ModTime=nil for missing file, got %v", out.ModTime)
				}
				if out.Width != 0 || out.Height != 0 {
					t.Fatalf("expected zero dimensions for missing file, got %dx%d", out.Width, out.Height)
				}
				if out.Format != "" || out.MIMEType != "" {
					t.Fatalf("expected empty Format/MIMEType for missing file, got %q / %q", out.Format, out.MIMEType)
				}
			},
		},
		{
			name: "non-existent path is not an error (includeBase64Data=true still empty)",
			ctx:  t.Context(),
			args: ReadImageArgs{
				Path:              missingPath,
				IncludeBase64Data: true,
			},
			check: func(t *testing.T, out *ReadImageOut) {
				t.Helper()
				if out == nil {
					t.Fatalf("expected non-nil out")
				}
				if out.Exists {
					t.Fatalf("expected Exists=false for missing file")
				}
				if out.Base64Data != "" {
					t.Fatalf("expected Base64Data empty for missing file even when IncludeBase64Data=true")
				}
			},
		},
		{
			name:    "non-image file errors (.txt)",
			ctx:     t.Context(),
			args:    ReadImageArgs{Path: textPath},
			wantErr: true,
		},
		{
			name:    "non-image file errors (bad .png contents)",
			ctx:     t.Context(),
			args:    ReadImageArgs{Path: badPNGPath},
			wantErr: true,
		},
		{
			name:    "directory path errors",
			ctx:     t.Context(),
			args:    ReadImageArgs{Path: tmpDir},
			wantErr: true,
		},
		{
			name:    "empty path errors",
			ctx:     t.Context(),
			args:    ReadImageArgs{},
			wantErr: true,
		},
		{
			name:      "context canceled returns context error (even with valid path)",
			ctx:       canceledCtx,
			args:      ReadImageArgs{Path: imgPath, IncludeBase64Data: true},
			wantErr:   true,
			wantErrIs: context.Canceled,
		},
		{
			name:      "context deadline exceeded returns context error (even with empty args)",
			ctx:       expiredCtx,
			args:      ReadImageArgs{},
			wantErr:   true,
			wantErrIs: context.DeadlineExceeded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := fspolicy.New("", nil, true)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			out, err := readImage(tt.ctx, tt.args, p)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (out=%+v)", out)
				}
				if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("error mismatch: got %v, want errors.Is(..., %v)=true", err, tt.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, out)
			}
		})
	}
}

func writePNG(t *testing.T, path string, w, h int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		t.Fatalf("encode png: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close image file: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read image file: %v", err)
	}
	return raw
}
