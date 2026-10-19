//go:build windows

package fspolicy

import (
	"errors"
	"testing"
)

func TestRejectDriveRelativePath_Windows(t *testing.T) {
	t.Parallel()

	type tc struct {
		name    string
		in      string
		wantErr error
	}

	cases := []tc{
		{
			name:    "drive_relative_rejected",
			in:      `C:foo`,
			wantErr: errWindowsDriveRelativePath,
		},
		{
			name:    "absolute_drive_ok",
			in:      `C:\foo`,
			wantErr: nil,
		},
		{
			name:    "relative_no_drive_ok",
			in:      `foo\bar`,
			wantErr: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := rejectDriveRelativePath(c.in)
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("rejectDriveRelativePath(%q) error: %v", c.in, err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("expected errors.Is(err,%v)=true, got %v", c.wantErr, err)
			}
		})
	}
}
