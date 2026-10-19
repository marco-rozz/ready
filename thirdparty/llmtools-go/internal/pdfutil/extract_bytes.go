package pdfutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/ledongthuc/pdf"
)

// ExtractPDFTextSafeFromBytes extracts text from in-memory PDF bytes.
//
// It mirrors ExtractPDFTextSafe for local files but avoids writing URL-fetched
// PDFs to disk. It is panic-safe because PDF parsers may panic on malformed
// input.
func ExtractPDFTextSafeFromBytes(ctx context.Context, data []byte, maxTextBytes int64) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("panic during PDF text extraction from bytes", "panic", r)
			err = fmt.Errorf("panic during PDF text extraction: %v", r)
		}
	}()

	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", errors.New("empty PDF data")
	}

	reader := bytes.NewReader(data)
	r, err := pdf.NewReader(reader, int64(len(data)))
	if err != nil {
		return "", err
	}

	plain, err := r.GetPlainText()
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	source := plain
	if maxTextBytes > 0 {
		source = io.LimitReader(plain, maxTextBytes)
	}

	if _, err := io.Copy(&buf, source); err != nil {
		return "", err
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	text = strings.TrimSpace(buf.String())
	if text == "" {
		return "", errors.New("empty PDF text after extraction")
	}
	return text, nil
}
