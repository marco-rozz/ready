package ioutil

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

type ImageInfo struct {
	PathInfo

	Width    int      `json:"width,omitempty"`
	Height   int      `json:"height,omitempty"`
	Format   string   `json:"format,omitempty"`   // e.g. "jpeg", "png"
	MIMEType MIMEType `json:"mimeType,omitempty"` // e.g. "image/jpeg"
}

// ImageData holds metadata (and optionally content) for an image file.
type ImageData struct {
	ImageInfo

	Base64Data string `json:"base64Data,omitempty"` // optional, if requested
}

// ReadImage inspects an image file and returns its intrinsic metadata.
// If includeBase64 is true, Base64Data will contain the base64-encoded file contents.
// If the file does not exist, Exists == false and err == nil.
//
// FSPolicy enforcement:
//   - path is resolved via policy (base dir + allowed roots)
//   - if policy.BlockSymlinks == true: refuses symlink parent components and refuses symlink file.
//   - even if symlinks are allowed, symlink files are refused (strict).
func ReadImage(
	p fspolicy.FSPolicy,
	path string,
	includeBase64Data bool,
	maxBytes int64,
) (*ImageData, error) {
	if strings.TrimSpace(path) == "" {
		return nil, ErrInvalidPath
	}

	out := &ImageData{}

	abs, err := p.ResolvePath(path, "")
	if err != nil {
		return nil, err
	}
	out.Path = abs

	parent := filepath.Dir(abs)
	if p.BlockSymlinks() && parent != "" && parent != "." {
		if err := p.VerifyDirResolved(parent); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				out.Exists = false
				return out, nil
			}
			return nil, err
		}
	}

	st, err := os.Lstat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			out.Exists = false
			return out, nil
		}
		return nil, err
	}

	out.Exists = true
	out.Name = st.Name()
	out.IsDir = st.IsDir()
	out.Size = st.Size()
	mt := st.ModTime().UTC()
	out.ModTime = &mt

	if (st.Mode() & os.ModeSymlink) != 0 {
		if p.BlockSymlinks() {
			return nil, fmt.Errorf("%w: refusing to operate on symlink file: %s", fspolicy.ErrSymlinkDisallowed, abs)
		}
		// Even if symlinks allowed, image decoding would follow; keep strict.
		return nil, fmt.Errorf("refusing to operate on symlink file: %s", abs)
	}

	if out.IsDir {
		return nil, errors.New("path points to a directory, expected file")
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("expected regular file: %s", abs)
	}

	if includeBase64Data {

		f, err := os.Open(out.Path)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		r := io.Reader(f)
		if maxBytes > 0 {
			r = io.LimitReader(f, maxBytes+1)
		}

		data, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		if maxBytes > 0 && int64(len(data)) > maxBytes {
			return nil, fmt.Errorf(
				"file %q exceeds maximum allowed size (%d bytes): %w",
				out.Path,
				maxBytes,
				ErrFileExceedsMaxSize,
			)
		}

		reader := bytes.NewReader(data)
		if err := decodeImageConfig(out, reader); err != nil {
			return nil, err
		}
		out.Base64Data = base64.StdEncoding.EncodeToString(data)
		return out, nil
	}

	// No base64 requested: just open and decode config.
	f, err := os.Open(out.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := io.Reader(f)
	if maxBytes > 0 {
		r = io.LimitReader(f, maxBytes)
	}

	if err := decodeImageConfig(out, r); err != nil {
		return nil, err
	}

	return out, nil
}

func decodeImageConfig(info *ImageData, reader io.Reader) error {
	cfg, fmtName, err := image.DecodeConfig(reader)
	if err != nil {
		return err
	}

	info.Width = cfg.Width
	info.Height = cfg.Height
	info.Format = fmtName

	m, err := MIMEFromExtensionString(fmtName)
	if err != nil {
		return fmt.Errorf("unsupported image format %q: %w", fmtName, err)
	}
	if GetModeForMIME(m) != ExtensionModeImage {
		return fmt.Errorf("unsupported image MIME type %q", m)
	}
	info.MIMEType = m
	return nil
}
