package ioutil

import (
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

type PathInfo struct {
	Path    string     `json:"path"`
	Name    string     `json:"name"`
	Exists  bool       `json:"exists"`
	IsDir   bool       `json:"isDir"`
	Size    int64      `json:"size,omitempty"`
	ModTime *time.Time `json:"modTime,omitempty"`
}

// StatPath returns basic metadata for the supplied path without mutating the filesystem.
// If the path does not exist, exists == false and err == nil.
//
// FSPolicy enforcement:
//   - path resolved via policy (base dir + allowed roots)
//   - if policy.BlockSymlinks == true: refuses symlink targets (Lstat + reject).
func StatPath(p fspolicy.FSPolicy, path string) (pathInfo *PathInfo, err error) {
	abs, err := p.ResolvePath(path, "")
	if err != nil {
		return nil, err
	}

	pathInfo = &PathInfo{
		Path:   abs,
		Exists: false,
	}

	var info fs.FileInfo
	if p.BlockSymlinks() {
		info, err = os.Lstat(abs)
	} else {
		info, err = os.Stat(abs)
	}

	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pathInfo, nil
		}
		return nil, err
	}

	if p.BlockSymlinks() && (info.Mode()&os.ModeSymlink) != 0 {
		return nil, fspolicy.ErrSymlinkDisallowed
	}

	pInfo := getPathInfoFromFileInfo(abs, info)
	return &pInfo, nil
}

func getPathInfoFromFileInfo(path string, info fs.FileInfo) PathInfo {
	m := info.ModTime().UTC()
	return PathInfo{
		Path:    path,
		Name:    info.Name(),
		Exists:  true,
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		ModTime: &m,
	}
}
