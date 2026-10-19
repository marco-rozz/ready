package ioutil

import (
	"path/filepath"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

// MIMEForPath returns a best-effort MIME type for a filesystem path with FSPolicy enforcement.
//
// Behavior:
//   - Path is resolved via policy (base dir + allowed roots).
//   - If the extension maps to a non-generic MIME type and a non-default mode, it returns without IO
//     (so it may succeed even if the file doesn't exist).
//   - Otherwise it requires an existing regular file via policy (enforcing BlockSymlinks rules) and sniffs bytes.
func MIMEForPath(
	p fspolicy.FSPolicy,
	path string,
	maxSniffBytes int64,
) (abs string, mimeType MIMEType, mode ExtensionMode, method MIMEDetectMethod, err error) {
	abs, err = p.ResolvePath(path, "")
	if err != nil {
		return "", MIMEEmpty, ExtensionModeDefault, MIMEDetectMethodSniff, err
	}

	ext := filepath.Ext(abs)
	if ext != "" {
		mt, e := MIMEFromExtensionString(ext)
		if e == nil && mt != MIMEEmpty && GetBaseMIME(mt) != string(MIMEApplicationOctetStream) {
			m := GetModeForMIME(mt)
			if m != ExtensionModeDefault {
				return abs, mt, m, MIMEDetectMethodExtension, nil
			}
		}
	}

	// Need sniff: enforce that the file exists and is a regular file according to policy.
	if _, err := p.RequireExistingRegularFileResolved(abs); err != nil {
		return abs, MIMEEmpty, ExtensionModeDefault, MIMEDetectMethodSniff, err
	}

	mt, m, e := SniffFileMIME(abs, maxSniffBytes)
	if e != nil {
		return abs, MIMEEmpty, ExtensionModeDefault, MIMEDetectMethodSniff, e
	}
	return abs, mt, m, MIMEDetectMethodSniff, nil
}
