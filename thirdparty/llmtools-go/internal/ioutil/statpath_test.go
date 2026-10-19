package ioutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

const (
	statPathTestEmptyPathName       = "empty path"
	statPathTestInvalidPathName     = "invalid path"
	statPathTestNonExistentPathName = "non-existent path"
)

func TestStatPath(t *testing.T) {
	dir := t.TempDir()

	filePath := filepath.Join(dir, "file.txt")
	fileContent := "some content"
	writeFile(t, filePath, fileContent)
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("failed to stat test file: %v", err)
	}

	subdirPath := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(subdirPath, 0o755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	nonExistentPath := filepath.Join(dir, "missing.txt")

	tests := []struct {
		name            string
		path            string
		wantExists      bool
		wantIsDir       bool
		wantName        string
		wantSize        int64 // -1 means "don't check"
		wantErr         bool
		wantErrContains string
	}{
		{
			name:            statPathTestEmptyPathName,
			path:            "",
			wantErr:         true,
			wantErrContains: statPathTestInvalidPathName,
		},
		{
			name:       statPathTestNonExistentPathName,
			path:       nonExistentPath,
			wantExists: false,
			wantIsDir:  false,
			wantName:   "",
			wantSize:   0,
		},
		{
			name:       "existing file",
			path:       filePath,
			wantExists: true,
			wantIsDir:  false,
			wantName:   filepath.Base(filePath),
			wantSize:   fileInfo.Size(),
		},
		{
			name:       "existing directory",
			path:       subdirPath,
			wantExists: true,
			wantIsDir:  true,
			wantName:   filepath.Base(subdirPath),
			wantSize:   -1, // don't assert exact size (OS-dependent)
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := fspolicy.New("", nil, true)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			got, err := StatPath(policy, tc.path)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrContains)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil {
				t.Fatalf("expected non-nil PathInfo")
			}

			if got.Exists != tc.wantExists {
				t.Errorf("Exists = %v, want %v", got.Exists, tc.wantExists)
			}
			if got.IsDir != tc.wantIsDir {
				t.Errorf("IsDir = %v, want %v", got.IsDir, tc.wantIsDir)
			}
			if tc.wantName != "" && got.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tc.wantName)
			}
			if tc.wantSize >= 0 && got.Size != tc.wantSize {
				t.Errorf("Size = %d, want %d", got.Size, tc.wantSize)
			}

			if tc.wantExists {
				if got.ModTime == nil {
					t.Errorf("ModTime is nil, want non-nil for existing path")
				}
			} else {
				if got.ModTime != nil {
					t.Errorf("ModTime is non-nil for non-existent path")
				}
			}
		})
	}
}

func TestGetPathInfoFromFileInfo(t *testing.T) {
	dir := t.TempDir()

	filePath := filepath.Join(dir, "file.txt")
	writeFile(t, filePath, "file content")
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}
	dirPath := filepath.Join(dir, "subdir")

	tests := []struct {
		name  string
		path  string
		isDir bool
	}{
		{
			name:  "file info",
			path:  filePath,
			isDir: false,
		},
		{
			name:  "directory info",
			path:  dirPath,
			isDir: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info, err := os.Stat(tc.path)
			if err != nil {
				t.Fatalf("stat failed: %v", err)
			}

			got := getPathInfoFromFileInfo(tc.path, info)

			if got.Path != tc.path {
				t.Errorf("Path = %q, want %q", got.Path, tc.path)
			}
			if got.Name != info.Name() {
				t.Errorf("Name = %q, want %q", got.Name, info.Name())
			}
			if !got.Exists {
				t.Errorf("Exists = false, want true")
			}
			if got.IsDir != tc.isDir {
				t.Errorf("IsDir = %v, want %v", got.IsDir, tc.isDir)
			}
			if got.Size != info.Size() {
				t.Errorf("Size = %d, want %d", got.Size, info.Size())
			}
			if got.ModTime == nil {
				t.Fatalf("ModTime is nil, want non-nil")
			}
			if !got.ModTime.Equal(info.ModTime().UTC()) {
				t.Errorf("ModTime = %v, want %v", got.ModTime, info.ModTime().UTC())
			}
		})
	}
}

func TestStatPath_SymlinkBehavior(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("symlink test skipped on Windows")
	}

	dir := t.TempDir()
	realTxt := filepath.Join(dir, "real.txt")
	mustWriteBytes(t, realTxt, []byte("x"))

	link := filepath.Join(dir, "link.txt")
	mustSymlinkOrSkip(t, realTxt, link)

	pBlock, err := fspolicy.New(dir, nil, true)
	if err != nil {
		t.Fatalf("New policy block: %v", err)
	}
	_, err = StatPath(pBlock, link)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, fspolicy.ErrSymlinkDisallowed) {
		t.Fatalf("err=%v; want errors.Is(_, %v)=true", err, fspolicy.ErrSymlinkDisallowed)
	}

	pAllow, err := fspolicy.New(dir, nil, false)
	if err != nil {
		t.Fatalf("New policy allow: %v", err)
	}
	info, err := StatPath(pAllow, link)
	if err != nil {
		t.Fatalf("StatPath error: %v", err)
	}
	if info == nil || !info.Exists || info.IsDir {
		t.Fatalf("unexpected PathInfo: %+v", info)
	}
}
