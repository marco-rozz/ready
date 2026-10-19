package fstool

import (
	"context"
	"testing"
)

func TestMIMEForExtension(t *testing.T) {
	ft := mustNewFSTool(t)

	tests := []struct {
		name    string
		ctx     func(t *testing.T) context.Context
		args    MIMEForExtensionArgs
		wantErr func(error) bool

		wantNormExt string
		wantMIME    string
		wantBase    string
		wantMode    MIMEMode
		wantKnown   bool
	}{
		{
			name:    testNameContextCanceled,
			ctx:     canceledContext,
			args:    MIMEForExtensionArgs{Extension: testExtPNG},
			wantErr: wantErrIs(context.Canceled),
		},
		{
			name:    "empty_extension_errors",
			args:    MIMEForExtensionArgs{Extension: testWhitespace},
			wantErr: wantErrContains(testErrInvalidPath),
		},
		{
			name:        "known_png_dot",
			args:        MIMEForExtensionArgs{Extension: testExtDotPNG},
			wantErr:     wantErrNone,
			wantNormExt: testExtDotPNG,
			wantMIME:    testMIMEImagePNG,
			wantBase:    testMIMEImagePNG,
			wantMode:    MIMEModeImage,
			wantKnown:   true,
		},
		{
			name:        "known_txt_base_strips_charset",
			args:        MIMEForExtensionArgs{Extension: testExtTXT},
			wantErr:     wantErrNone,
			wantNormExt: testExtTXT,
			wantMIME:    testMIMETextPlainCharset,
			wantBase:    testMIMETextPlain,
			wantMode:    MIMEModeText,
			wantKnown:   true,
		},
		{
			name:        "unknown_extension_returns_octet_stream_no_error",
			args:        MIMEForExtensionArgs{Extension: testExtUnknown},
			wantErr:     wantErrNone,
			wantNormExt: testExtUnknown,
			wantMIME:    testMIMEApplicationOctetStream,
			wantBase:    testMIMEApplicationOctetStream,
			wantMode:    MIMEModeDefault,
			wantKnown:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if tt.ctx != nil {
				ctx = tt.ctx(t)
			}

			out, err := ft.MIMEForExtension(ctx, tt.args)
			if tt.wantErr == nil {
				tt.wantErr = wantErrNone
			}
			if !tt.wantErr(err) {
				t.Fatalf("err=%v did not match expectation", err)
			}
			if err != nil {
				return
			}
			if out == nil {
				t.Fatalf("expected non-nil out")
			}

			if tt.wantNormExt != "" && out.NormalizedExtension != tt.wantNormExt {
				t.Fatalf("NormalizedExtension=%q want=%q", out.NormalizedExtension, tt.wantNormExt)
			}
			if tt.wantMIME != "" && out.MIMEType != tt.wantMIME {
				t.Fatalf("MIMEType=%q want=%q", out.MIMEType, tt.wantMIME)
			}
			if tt.wantBase != "" && out.BaseMIMEType != tt.wantBase {
				t.Fatalf("BaseMIMEType=%q want=%q", out.BaseMIMEType, tt.wantBase)
			}
			if tt.wantMode != "" && out.Mode != tt.wantMode {
				t.Fatalf("Mode=%q want=%q", out.Mode, tt.wantMode)
			}
			if out.Known != tt.wantKnown {
				t.Fatalf("Known=%v want=%v", out.Known, tt.wantKnown)
			}
		})
	}
}
