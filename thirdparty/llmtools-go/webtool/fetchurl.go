package webtool

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/internal/pdfutil"

	"github.com/flexigpt/llmtools-go/spec"
	"golang.org/x/net/html/charset"
)

const fetchURLFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/webtool/fetchurl.FetchURL"

var fetchURLTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019d6b4d-2cf4-71bb-a827-087029d7d09a",
	Slug:          "fetchurl",
	Version:       spec.VersionOne,
	DisplayName:   "Fetch URL",
	Description:   "Fetch an HTTP/HTTPS URL. Default auto mode returns compact text for HTML/text/PDF and image/file outputs for binary URLs. Use startIndex to continue truncated text.",
	Tags:          []string{"web", "fetch"},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"url": {
		"type": "string",
		"description": "Absolute http or https URL to fetch."
	},
	"encoding": {
		"type": "string",
		"enum": ["auto", "text", "binary"],
		"description": "Return mode. auto returns text for HTML/text/PDF and image/file outputs for binary URLs. text forces readable text. binary returns base64 image/file output.",
		"default": "auto"
	},
	"maxLength": {
		"type": "integer",
		"description": "Maximum number of text characters to return. Applies only to text output.",
		"default": 5000,
		"minimum": 1,
		"maximum": 100000
	},
	"startIndex": {
		"type": "integer",
		"description": "Start text output at this character index. Use the next startIndex from a truncated result to continue.",
		"default": 0,
		"minimum": 0
	},
	"raw": {
		"type": "boolean",
		"description": "For HTML/text responses, return raw decoded text instead of readable Markdown extraction.",
		"default": false
	}
},
"required": ["url"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: fetchURLFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type FetchURLEncoding string

const (
	FetchEncodingAuto   FetchURLEncoding = "auto"
	FetchEncodingText   FetchURLEncoding = "text"
	FetchEncodingBinary FetchURLEncoding = "binary"
)

type FetchURLArgs struct {
	URL        string `json:"url"`
	Encoding   string `json:"encoding,omitempty"`
	MaxLength  int    `json:"maxLength,omitempty"`
	StartIndex int    `json:"startIndex,omitempty"`
	Raw        bool   `json:"raw,omitempty"`
}

func fetchURL(
	ctx context.Context,
	args FetchURLArgs,
	cfg webToolConfig,
	client *http.Client,
) ([]spec.ToolOutputUnion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	encoding, err := normalizeFetchEncoding(args.Encoding)
	if err != nil {
		return nil, err
	}

	maxLength := normalizeMaxLength(args.MaxLength)
	if args.StartIndex < 0 {
		return nil, errors.New("startIndex must be >= 0")
	}

	resp, err := fetchURLBytes(ctx, args.URL, cfg, client)
	if err != nil {
		return nil, err
	}

	contentType := inferContentType(resp.contentType, resp.finalURL, resp.data)

	if encoding == FetchEncodingBinary {
		return binaryOutputs(resp.finalURL, contentType, resp.header, resp.data)
	}

	if isPDFContentType(contentType, resp.finalURL) {
		text, err := pdfutil.ExtractPDFTextSafeFromBytes(ctx, resp.data, cfg.maxFetchBytes)
		if err == nil && strings.TrimSpace(text) != "" {
			return textOutputs(resp.finalURL, text, args.StartIndex, maxLength), nil
		}
		if encoding == FetchEncodingAuto {
			return binaryOutputs(resp.finalURL, contentType, resp.header, resp.data)
		}
		if err != nil {
			return nil, fmt.Errorf("extract pdf text: %w", err)
		}
		return nil, errors.New("extract pdf text: empty text")
	}

	if isImageContentType(contentType) {
		if encoding == FetchEncodingAuto {
			return binaryOutputs(resp.finalURL, contentType, resp.header, resp.data)
		}
		return nil, fmt.Errorf("url content-type %q is an image; use encoding \"binary\" or \"auto\"", contentType)
	}

	if isTextLikeContentType(contentType) || looksLikeHTML(resp.data) || looksLikeUTF8Text(resp.data) {
		text, err := textFromResponse(ctx, resp.finalURL, resp.contentType, contentType, resp.data, args.Raw)
		if err != nil {
			if encoding == FetchEncodingAuto {
				return binaryOutputs(resp.finalURL, contentType, resp.header, resp.data)
			}
			return nil, err
		}
		return textOutputs(resp.finalURL, text, args.StartIndex, maxLength), nil
	}

	if encoding == FetchEncodingAuto {
		return binaryOutputs(resp.finalURL, contentType, resp.header, resp.data)
	}

	return nil, fmt.Errorf("url content-type %q is not text; use encoding \"binary\" or \"auto\"", contentType)
}

func normalizeFetchEncoding(in string) (FetchURLEncoding, error) {
	enc := FetchURLEncoding(strings.ToLower(strings.TrimSpace(in)))
	if enc == "" {
		return FetchEncodingAuto, nil
	}

	switch enc {
	case FetchEncodingAuto, FetchEncodingText, FetchEncodingBinary:
		return enc, nil
	default:
		return "", errors.New(`encoding must be "auto", "text", or "binary"`)
	}
}

func normalizeMaxLength(maxLength int) int {
	if maxLength <= 0 {
		return defaultFetchMaxLength
	}
	if maxLength > hardFetchMaxLength {
		return hardFetchMaxLength
	}
	return maxLength
}

func textFromResponse(
	ctx context.Context,
	finalURL string,
	rawContentType string,
	contentType string,
	data []byte,
	raw bool,
) (string, error) {
	if len(data) == 0 {
		return "", errors.New("empty response body")
	}

	decoded, err := decodeTextBytes(data, rawContentType)
	if err != nil {
		return "", err
	}

	if isHTMLContentType(contentType) || looksLikeHTML(data) {
		if raw {
			return strings.TrimSpace(decoded), nil
		}

		markdown, err := extractReadableMarkdownFromHTML(ctx, finalURL, []byte(decoded))
		if err == nil {
			markdown = strings.TrimSpace(markdown)
			if markdown != "" {
				return markdown, nil
			}
		}

		plain, plainErr := htmlToPlainText(ctx, []byte(decoded))
		if plainErr == nil {
			plain = strings.TrimSpace(plain)
			if plain != "" {
				return plain, nil
			}
		}

		decoded = strings.TrimSpace(decoded)
		if decoded != "" {
			return decoded, nil
		}

		if err != nil {
			return "", fmt.Errorf("extract html text: %w", err)
		}
		return "", errors.New("extract html text: empty text")
	}

	decoded = strings.TrimSpace(decoded)
	if decoded == "" {
		return "", errors.New("empty text response")
	}
	return decoded, nil
}

func decodeTextBytes(data []byte, contentType string) (string, error) {
	if len(data) == 0 {
		return "", nil
	}

	if _, params, err := mime.ParseMediaType(contentType); err == nil {
		if label := strings.TrimSpace(params["charset"]); label != "" && !isUTF8Charset(label) {
			reader, err := charset.NewReaderLabel(label, bytes.NewReader(data))
			if err == nil {
				limit := int64(len(data))*4 + 4096
				out, err := io.ReadAll(io.LimitReader(reader, limit))
				if err == nil {
					return strings.ToValidUTF8(string(out), "\uFFFD"), nil
				}
			}
		}
	}

	if utf8.Valid(data) {
		return string(data), nil
	}

	if hasNULByte(data) {
		return "", errors.New("response is not valid UTF-8 text")
	}

	return strings.ToValidUTF8(string(data), "\uFFFD"), nil
}

func isUTF8Charset(label string) bool {
	label = strings.ToLower(strings.TrimSpace(label))
	return label == "utf-8" || label == "utf8" || label == "us-ascii" || label == "ascii"
}

func textOutputs(rawURL, content string, startIndex, maxLength int) []spec.ToolOutputUnion {
	part, total, nextStart, hasMore := sliceStringByRune(content, startIndex, maxLength)

	var text string
	if startIndex >= total {
		text = fmt.Sprintf("No more content available for %s.", rawURL)
	} else {
		text = fmt.Sprintf("Contents of %s:\n%s", rawURL, part)
		if hasMore {
			text += fmt.Sprintf("\n\n[truncated: call fetchurl with startIndex=%d]", nextStart)
		}
	}

	return []spec.ToolOutputUnion{
		{
			Kind: spec.ToolOutputKindText,
			TextItem: &spec.ToolOutputText{
				Text: text,
			},
		},
	}
}

func binaryOutputs(rawURL, contentType string, header http.Header, data []byte) ([]spec.ToolOutputUnion, error) {
	if contentType == "" {
		contentType = string(ioutil.MIMEApplicationOctetStream)
	}

	base64Data := base64.StdEncoding.EncodeToString(data)
	name := filenameFromURL(rawURL, contentType, header)

	if isImageContentType(contentType) {
		return []spec.ToolOutputUnion{
			{
				Kind: spec.ToolOutputKindImage,
				ImageItem: &spec.ToolOutputImage{
					Detail:    spec.ImageDetailAuto,
					ImageName: name,
					ImageMIME: contentType,
					ImageData: base64Data,
				},
			},
		}, nil
	}

	return []spec.ToolOutputUnion{
		{
			Kind: spec.ToolOutputKindFile,
			FileItem: &spec.ToolOutputFile{
				FileName: name,
				FileMIME: contentType,
				FileData: base64Data,
			},
		},
	}, nil
}

func inferContentType(headerCT, rawURL string, data []byte) string {
	headerCT = normalizeContentType(headerCT)
	if headerCT != "" && !isGenericBinaryContentType(headerCT) {
		return headerCT
	}

	if ext := extensionFromURL(rawURL); ext != "" {
		mt, err := ioutil.MIMEFromExtensionString(ext)
		if err == nil {
			normalized := normalizeContentType(string(mt))
			if normalized != "" && !isGenericBinaryContentType(normalized) {
				return normalized
			}
		}

		if mt := mime.TypeByExtension(ext); mt != "" {
			normalized := normalizeContentType(mt)
			if normalized != "" && !isGenericBinaryContentType(normalized) {
				return normalized
			}
		}
	}

	if len(data) > 0 {
		sniffed := normalizeContentType(http.DetectContentType(data))
		if sniffed != "" && !isGenericBinaryContentType(sniffed) {
			return sniffed
		}
	}

	if headerCT != "" {
		return headerCT
	}
	return string(ioutil.MIMEApplicationOctetStream)
}

func isGenericBinaryContentType(ct string) bool {
	ct = normalizeContentType(ct)
	return ct == "" || strings.Contains(ct, "octet-stream") || strings.Contains(ct, "binary")
}

func isPDFContentType(ct, rawURL string) bool {
	ct = normalizeContentType(ct)
	if ct == string(ioutil.MIMEApplicationPDF) {
		return true
	}
	return strings.EqualFold(extensionFromURL(rawURL), ".pdf")
}

func isImageContentType(ct string) bool {
	ct = normalizeContentType(ct)
	return strings.HasPrefix(ct, "image/") && ct != "image/svg+xml"
}

func isHTMLContentType(ct string) bool {
	ct = normalizeContentType(ct)
	return ct == string(ioutil.MIMEBaseTextHTML) ||
		ct == "application/xhtml+xml" ||
		strings.Contains(ct, "html")
}

func isTextLikeContentType(ct string) bool {
	ct = normalizeContentType(ct)
	if ct == "" {
		return false
	}
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	if ct == "image/svg+xml" {
		return true
	}
	if strings.HasSuffix(ct, "+json") || strings.HasSuffix(ct, "+xml") {
		return true
	}

	switch ct {
	case string(ioutil.MIMEApplicationJSON),
		"application/xml",
		"application/javascript",
		"application/ecmascript",
		"application/x-javascript",
		"application/x-ndjson",
		"application/yaml",
		"application/x-yaml",
		"application/toml",
		"application/x-www-form-urlencoded":
		return true
	default:
		return false
	}
}

func looksLikeHTML(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	sample := data
	if len(sample) > 1024 {
		sample = sample[:1024]
	}
	s := strings.ToLower(strings.TrimSpace(string(sample)))
	return strings.Contains(s, "<html") ||
		strings.Contains(s, "<!doctype html") ||
		strings.Contains(s, "<head") ||
		strings.Contains(s, "<body") ||
		strings.Contains(s, "<title")
}

func looksLikeUTF8Text(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	if hasNULByte(data) {
		return false
	}
	return utf8.Valid(data)
}

func hasNULByte(data []byte) bool {
	return slices.Contains(data, 0)
}

func extensionFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err == nil {
		return strings.ToLower(path.Ext(u.Path))
	}
	return strings.ToLower(path.Ext(rawURL))
}

func filenameFromURL(rawURL, contentType string, header http.Header) string {
	if header != nil {
		if cd := header.Get("Content-Disposition"); cd != "" {
			if _, params, err := mime.ParseMediaType(cd); err == nil {
				if name := sanitizeFilename(params["filename"]); name != "" {
					return name
				}
			}
		}
	}

	if u, err := url.Parse(rawURL); err == nil {
		name := path.Base(u.Path)
		if decoded, err := url.PathUnescape(name); err == nil {
			name = decoded
		}
		if name = sanitizeFilename(name); name != "" && name != "." && name != "/" {
			return name
		}
	}

	ext := extensionForContentType(contentType)
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return "image" + ext
	case contentType == string(ioutil.MIMEApplicationPDF):
		return "document.pdf"
	default:
		return "download" + ext
	}
}

func extensionForContentType(contentType string) string {
	contentType = normalizeContentType(contentType)
	if contentType == "" {
		return ""
	}

	exts, err := mime.ExtensionsByType(contentType)
	if err != nil || len(exts) == 0 {
		return ""
	}
	return exts[0]
}

func normalizeContentType(ct string) string {
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err == nil && strings.TrimSpace(mt) != "" {
		return strings.ToLower(strings.TrimSpace(mt))
	}
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}

	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == ':':
			return '-'
		case r < 32 || r == 127:
			return -1
		default:
			return r
		}
	}, name)

	name = strings.Trim(name, ". \t\r\n")
	if len(name) > 160 {
		name = name[:160]
	}
	return name
}

func sliceStringByRune(s string, start, maxLen int) (part string, total, nextStart int, hasMore bool) {
	if start < 0 {
		start = 0
	}
	if maxLen <= 0 {
		maxLen = defaultFetchMaxLength
	}

	total = utf8.RuneCountInString(s)
	if start >= total {
		return "", total, total, false
	}

	end := min(start+maxLen, total)

	byteStart := byteIndexForRune(s, start)
	byteEnd := byteIndexForRune(s, end)
	return s[byteStart:byteEnd], total, end, end < total
}

func byteIndexForRune(s string, target int) int {
	if target <= 0 {
		return 0
	}

	count := 0
	for i := range s {
		if count == target {
			return i
		}
		count++
	}
	return len(s)
}
