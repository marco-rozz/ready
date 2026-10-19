package ioutil

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

var (
	// ErrInvalidPath is shared with policy to keep behavior consistent.
	ErrInvalidPath      = fspolicy.ErrInvalidPath
	ErrInvalidDir       = errors.New("invalid dir")
	ErrUnknownExtension = errors.New("unknown extension")
)

type ExtensionMode string

const (
	ExtensionModeText     ExtensionMode = "text"
	ExtensionModeImage    ExtensionMode = "image"
	ExtensionModeDocument ExtensionMode = "document"
	ExtensionModeDefault  ExtensionMode = "default"
)

var AllExtensionModes = []ExtensionMode{
	ExtensionModeText,
	ExtensionModeImage,
	ExtensionModeDocument,
	ExtensionModeDefault,
}

type FileExt string

const (
	ExtTxt      FileExt = ".txt"
	ExtMd       FileExt = ".md"
	ExtMarkdown FileExt = ".markdown"
	ExtLog      FileExt = ".log"
	ExtJSON     FileExt = ".json"
	ExtYAML     FileExt = ".yaml"
	ExtYML      FileExt = ".yml"
	ExtTOML     FileExt = ".toml"
	ExtJS       FileExt = ".js"
	ExtTS       FileExt = ".ts"
	ExtTSX      FileExt = ".tsx"
	ExtJSX      FileExt = ".jsx"
	ExtPY       FileExt = ".py"
	ExtGO       FileExt = ".go"
	ExtRS       FileExt = ".rs"
	ExtJAVA     FileExt = ".java"
	ExtC        FileExt = ".c"
	ExtCPP      FileExt = ".cpp"
	ExtH        FileExt = ".h"
	ExtHPP      FileExt = ".hpp"
	ExtCS       FileExt = ".cs"
	ExtRB       FileExt = ".rb"
	ExtPHP      FileExt = ".php"
	ExtHTML     FileExt = ".html"
	ExtHTM      FileExt = ".htm"
	ExtCSS      FileExt = ".css"
	ExtSCSS     FileExt = ".scss"
	ExtLESS     FileExt = ".less"
	ExtSQL      FileExt = ".sql"
	ExtMod      FileExt = ".mod"
	ExtSum      FileExt = ".sum"
	ExtJSONL    FileExt = ".jsonl"
	ExtShell    FileExt = ".sh"
	ExtBash     FileExt = ".bash"
	ExtBAT      FileExt = ".bat"
	ExtCMD      FileExt = ".cmd"
	ExtZsh      FileExt = ".zsh"
	ExtKsh      FileExt = ".ksh"
	ExtDash     FileExt = ".dash"
	ExtPS1      FileExt = ".ps1"
	ExtSWIFT    FileExt = ".swift"
	ExtM        FileExt = ".m"
	ExtKT       FileExt = ".kt"
	ExtPL       FileExt = ".pl"
	ExtSCALA    FileExt = ".scala"
	ExtHS       FileExt = ".hs"
	ExtLUA      FileExt = ".lua"
	ExtDART     FileExt = ".dart"
	ExtCmake    FileExt = ".cmake"
	ExtBazel    FileExt = ".bazel"
	ExtXML      FileExt = ".xml"

	ExtJPG  FileExt = ".jpg"
	ExtJPEG FileExt = ".jpeg"
	ExtPNG  FileExt = ".png"
	ExtGIF  FileExt = ".gif"
	ExtWEBP FileExt = ".webp"
	ExtBMP  FileExt = ".bmp"
	ExtSVG  FileExt = ".svg"

	ExtPDF  FileExt = ".pdf"
	ExtDOC  FileExt = ".doc"
	ExtDOCX FileExt = ".docx"
	ExtPPT  FileExt = ".ppt"
	ExtPPTX FileExt = ".pptx"
	ExtXLS  FileExt = ".xls"
	ExtXLSX FileExt = ".xlsx"
	ExtODT  FileExt = ".odt"
	ExtODS  FileExt = ".ods"
)

type MIMEType string

const (
	MIMEEmpty                  MIMEType = ""
	MIMEApplicationOctetStream MIMEType = "application/octet-stream"

	MIMETextPlain    MIMEType = "text/plain; charset=utf-8"
	MIMETextMarkdown MIMEType = "text/markdown; charset=utf-8"
	MIMETextHTML     MIMEType = "text/html; charset=utf-8"
	MIMEBaseTextHTML MIMEType = "text/html"
	MIMETextCSS      MIMEType = "text/css; charset=utf-8"

	MIMEApplicationJSON MIMEType = "application/json"
	MIMEApplicationXML  MIMEType = "application/xml"
	MIMEApplicationYAML MIMEType = "application/x-yaml"
	MIMEApplicationTOML MIMEType = "application/toml"
	MIMEApplicationSQL  MIMEType = "application/sql"
	MIMEApplicationJS   MIMEType = "application/javascript"

	MIMEImageJPEG MIMEType = "image/jpeg"
	MIMEImagePNG  MIMEType = "image/png"
	MIMEImageGIF  MIMEType = "image/gif"
	MIMEImageWEBP MIMEType = "image/webp"
	MIMEImageBMP  MIMEType = "image/bmp"
	MIMEImageSVG  MIMEType = "image/svg+xml"

	MIMEApplicationPDF        MIMEType = "application/pdf"
	MIMEApplicationMSWord     MIMEType = "application/msword"
	MIMEApplicationMSPowerPt  MIMEType = "application/vnd.ms-powerpoint"
	MIMEApplicationMSExcel    MIMEType = "application/vnd.ms-excel"
	MIMEApplicationOpenXMLDoc MIMEType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	MIMEApplicationOpenXMLPPT MIMEType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	MIMEApplicationOpenXMLXLS MIMEType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	MIMEApplicationODT        MIMEType = "application/vnd.oasis.opendocument.text"
	MIMEApplicationODS        MIMEType = "application/vnd.oasis.opendocument.spreadsheet"
)

// ExtensionToMIMEType is an internal registry of common/explicitly-supported extensions.
var ExtensionToMIMEType = map[FileExt]MIMEType{
	ExtTxt:      MIMETextPlain,
	ExtMd:       MIMETextMarkdown,
	ExtMarkdown: MIMETextMarkdown,
	ExtLog:      MIMETextPlain,
	ExtJSON:     MIMEApplicationJSON,
	ExtYAML:     MIMEApplicationYAML,
	ExtYML:      MIMEApplicationYAML,
	ExtTOML:     MIMEApplicationTOML,
	ExtJS:       MIMEApplicationJS,
	ExtTS:       MIMETextPlain,
	ExtTSX:      MIMETextPlain,
	ExtJSX:      MIMETextPlain,
	ExtPY:       MIMETextPlain,
	ExtGO:       MIMETextPlain,
	ExtRS:       MIMETextPlain,
	ExtJAVA:     MIMETextPlain,
	ExtC:        MIMETextPlain,
	ExtCPP:      MIMETextPlain,
	ExtH:        MIMETextPlain,
	ExtHPP:      MIMETextPlain,
	ExtCS:       MIMETextPlain,
	ExtRB:       MIMETextPlain,
	ExtPHP:      MIMETextPlain,
	ExtHTML:     MIMETextHTML,
	ExtHTM:      MIMETextHTML,
	ExtCSS:      MIMETextCSS,
	ExtSCSS:     MIMETextPlain,
	ExtLESS:     MIMETextPlain,
	ExtSQL:      MIMEApplicationSQL,
	ExtMod:      MIMETextPlain,
	ExtSum:      MIMETextPlain,
	ExtJSONL:    MIMETextPlain,
	ExtShell:    MIMETextPlain,
	ExtSWIFT:    MIMETextPlain,
	ExtM:        MIMETextPlain,
	ExtKT:       MIMETextPlain,
	ExtPL:       MIMETextPlain,
	ExtSCALA:    MIMETextPlain,
	ExtHS:       MIMETextPlain,
	ExtLUA:      MIMETextPlain,
	ExtDART:     MIMETextPlain,
	ExtCmake:    MIMETextPlain,
	ExtBazel:    MIMETextPlain,
	ExtXML:      MIMEApplicationXML,

	ExtJPG:  MIMEImageJPEG,
	ExtJPEG: MIMEImageJPEG,
	ExtPNG:  MIMEImagePNG,
	ExtGIF:  MIMEImageGIF,
	ExtWEBP: MIMEImageWEBP,
	ExtBMP:  MIMEImageBMP,
	ExtSVG:  MIMEImageSVG,

	ExtPDF:  MIMEApplicationPDF,
	ExtDOC:  MIMEApplicationMSWord,
	ExtDOCX: MIMEApplicationOpenXMLDoc,
	ExtPPT:  MIMEApplicationMSPowerPt,
	ExtPPTX: MIMEApplicationOpenXMLPPT,
	ExtXLS:  MIMEApplicationMSExcel,
	ExtXLSX: MIMEApplicationOpenXMLXLS,
	ExtODT:  MIMEApplicationODT,
	ExtODS:  MIMEApplicationODS,
}

// BaseMIMEToMode maps base mime types (no parameters) to a coarse mode.
var BaseMIMEToMode = map[string]ExtensionMode{
	"":                         ExtensionModeDefault,
	"application/octet-stream": ExtensionModeDefault,

	// Text.
	"text/plain":             ExtensionModeText,
	"text/markdown":          ExtensionModeText,
	"text/html":              ExtensionModeText,
	"text/css":               ExtensionModeText,
	"application/json":       ExtensionModeText,
	"application/xml":        ExtensionModeText,
	"application/x-yaml":     ExtensionModeText,
	"application/yaml":       ExtensionModeText,
	"application/toml":       ExtensionModeText,
	"application/sql":        ExtensionModeText,
	"application/javascript": ExtensionModeText,

	// Images.
	"image/jpeg":    ExtensionModeImage,
	"image/png":     ExtensionModeImage,
	"image/gif":     ExtensionModeImage,
	"image/webp":    ExtensionModeImage,
	"image/bmp":     ExtensionModeImage,
	"image/svg+xml": ExtensionModeImage,

	// Documents.
	"application/pdf":               ExtensionModeDocument,
	"application/msword":            ExtensionModeDocument,
	"application/vnd.ms-powerpoint": ExtensionModeDocument,
	"application/vnd.ms-excel":      ExtensionModeDocument,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ExtensionModeDocument,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ExtensionModeDocument,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ExtensionModeDocument,
	"application/vnd.oasis.opendocument.text":                                   ExtensionModeDocument,
	"application/vnd.oasis.opendocument.spreadsheet":                            ExtensionModeDocument,
}

var ModeToExtensions = func() map[ExtensionMode][]FileExt {
	m := make(map[ExtensionMode][]FileExt, len(AllExtensionModes))
	for ext, mt := range ExtensionToMIMEType {
		mode := GetModeForMIME(mt)
		m[mode] = append(m[mode], ext)
	}
	return m
}()

type MIMEDetectMethod string

const (
	MIMEDetectMethodExtension MIMEDetectMethod = "extension"
	MIMEDetectMethodSniff     MIMEDetectMethod = "sniff"
)

func MIMEForLocalFile(
	path string,
	maxSniffBytes int64,
) (mimeType MIMEType, mode ExtensionMode, method MIMEDetectMethod, err error) {
	if strings.TrimSpace(path) == "" {
		return MIMEEmpty, ExtensionModeDefault, MIMEDetectMethodSniff, ErrInvalidPath
	}

	ext := filepath.Ext(path)
	if ext != "" {
		mt, e := MIMEFromExtensionString(ext)
		if e == nil && mt != MIMEEmpty && GetBaseMIME(mt) != string(MIMEApplicationOctetStream) {
			m := GetModeForMIME(mt)
			if m != ExtensionModeDefault {
				return mt, m, MIMEDetectMethodExtension, nil
			}
		}
	}

	mt, m, e := SniffFileMIME(path, maxSniffBytes)
	if e != nil {
		return MIMEEmpty, ExtensionModeDefault, MIMEDetectMethodSniff, e
	}
	return mt, m, MIMEDetectMethodSniff, nil
}

func MIMEFromExtensionString(ext string) (MIMEType, error) {
	if strings.TrimSpace(ext) == "" {
		return MIMEEmpty, ErrInvalidPath
	}

	e := GetNormalizedExt(ext)
	if e == "" {
		return MIMEEmpty, ErrInvalidPath
	}

	if mt, ok := ExtensionToMIMEType[e]; ok {
		return mt, nil
	}

	if t := mime.TypeByExtension(string(e)); t != "" {
		return MIMEType(t), nil
	}

	return MIMEApplicationOctetStream, ErrUnknownExtension
}

func SniffFileMIME(path string, maxSniffBytes int64) (MIMEType, ExtensionMode, error) {
	if strings.TrimSpace(path) == "" {
		return MIMEEmpty, ExtensionModeDefault, ErrInvalidPath
	}

	f, err := os.Open(path)
	if err != nil {
		return MIMEEmpty, ExtensionModeDefault, err
	}
	defer f.Close()

	r := io.Reader(f)
	if maxSniffBytes > 0 {
		r = io.LimitReader(f, maxSniffBytes)
	}
	buf := make([]byte, maxSniffBytes)
	n, err := r.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return MIMEEmpty, ExtensionModeDefault, err
	}

	sample := buf[:n]
	if len(sample) == 0 {
		return MIMETextPlain, ExtensionModeText, nil
	}

	mt := MIMEType(http.DetectContentType(sample))
	m := GetModeForMIME(mt)

	if m != ExtensionModeDefault {
		return mt, m, nil
	}

	if isProbablyTextSample(sample) {
		return MIMETextPlain, ExtensionModeText, nil
	}

	if GetBaseMIME(mt) == string(MIMEApplicationOctetStream) || mt == MIMEEmpty {
		return MIMEApplicationOctetStream, ExtensionModeDefault, nil
	}

	return mt, ExtensionModeDefault, nil
}

func GetModeForMIME(mt MIMEType) ExtensionMode {
	base := GetBaseMIME(mt)
	if m, ok := BaseMIMEToMode[base]; ok {
		return m
	}

	switch {
	case strings.HasPrefix(base, "text/"):
		return ExtensionModeText
	case strings.HasPrefix(base, "image/"):
		return ExtensionModeImage
	}

	if strings.HasSuffix(base, "+json") || strings.HasSuffix(base, "+xml") {
		return ExtensionModeText
	}

	return ExtensionModeDefault
}

func GetBaseMIME(mt MIMEType) string {
	s := strings.TrimSpace(strings.ToLower(string(mt)))
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func GetNormalizedExt(ext string) FileExt {
	e := strings.TrimSpace(ext)
	if e == "" {
		return FileExt("")
	}
	if !strings.HasPrefix(e, ".") {
		e = "." + e
	}
	return FileExt(strings.ToLower(e))
}

func isProbablyTextSample(p []byte) bool {
	if len(p) == 0 {
		return true
	}
	nulCount := 0
	controlCount := 0
	for _, b := range p {
		if b == 0 {
			nulCount++
			continue
		}
		if b < 32 && b != 9 && b != 10 && b != 13 {
			controlCount++
		}
	}
	if nulCount > 0 {
		return false
	}
	return controlCount*10 <= len(p)
}
