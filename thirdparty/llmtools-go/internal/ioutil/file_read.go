package ioutil

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
)

type ReadEncoding string

const (
	ReadEncodingText   ReadEncoding = "text"
	ReadEncodingBinary ReadEncoding = "binary"
)

// ReadFile reads a file and returns its contents.
// If maxBytes > 0, it enforces a hard cap during reading.
//
// NOTE: This is a raw IO helper; callers should resolve/enforce policy before calling.
func ReadFile(path string, encoding ReadEncoding, maxBytes int64) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || strings.ContainsRune(path, 0) {
		return "", ErrInvalidPath
	}

	if encoding != ReadEncodingText && encoding != ReadEncodingBinary {
		return "", errors.New(`encoding must be "text" or "binary"`)
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	r := io.Reader(f)
	if maxBytes > 0 {
		// Avoid maxBytes+1 overflow.
		var limit int64
		if maxBytes < math.MaxInt64 {
			limit = maxBytes + 1
		} else {
			limit = math.MaxInt64
		}
		r = io.LimitReader(f, limit)

	}

	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}

	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return "", fmt.Errorf(
			"file %q exceeds maximum allowed size (%d bytes): %w",
			path, maxBytes, ErrFileExceedsMaxSize,
		)
	}

	if encoding == ReadEncodingText {
		return string(data), nil
	}
	return base64.StdEncoding.EncodeToString(data), nil
}
