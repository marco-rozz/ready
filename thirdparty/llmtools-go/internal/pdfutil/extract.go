package pdfutil

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
	"github.com/ledongthuc/pdf"
)

// ExtractPDFTextSafe extracts text from a local PDF with a byte limit and panic recovery.
func ExtractPDFTextSafe(ctx context.Context, path string, maxBytes int) (string, error) {
	return toolutil.WithRecoveryResp(func() (string, error) {
		return extractPDFTextSafe(ctx, path, maxBytes)
	})
}

func extractPDFTextSafe(ctx context.Context, path string, maxBytes int) (text string, err error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	reader, err := r.GetPlainText()
	if err != nil {
		return "", err
	}

	source := reader
	if maxBytes > 0 {
		source = io.LimitReader(reader, int64(maxBytes))
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, source); err != nil {
		return "", err
	}

	text = strings.TrimSpace(buf.String())
	if text == "" {
		return "", errors.New("empty PDF text after extraction")
	}
	return text, nil
}
