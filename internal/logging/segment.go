package logging

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// dayLayout is the date layout embedded in every segment name.
const dayLayout = "2006-01-02"

// segmentScheme derives segment names from logging.file_path and is the only
// place that builds or recognises them: rotation and retention both go
// through parse, so "retention deletes only segment files" holds by
// construction.
type segmentScheme struct {
	dir  string
	base string
	ext  string
}

// newSegmentScheme splits filePath into directory, base name and extension:
// .ready/logs/ready.log → (".ready/logs", "ready", ".log").
func newSegmentScheme(filePath string) segmentScheme {
	name := filepath.Base(filePath)
	ext := filepath.Ext(name)

	return segmentScheme{
		dir:  filepath.Dir(filePath),
		base: strings.TrimSuffix(name, ext),
		ext:  ext,
	}
}

// name returns the file name of the segment for day and index; index 0 is the
// unsuffixed first segment of the day.
func (s segmentScheme) name(day time.Time, idx int) string {
	n := s.base + "-" + day.Format(dayLayout)
	if idx > 0 {
		n += "." + strconv.Itoa(idx)
	}

	return n + s.ext
}

// path returns the full path of the segment for day and index.
func (s segmentScheme) path(day time.Time, idx int) string {
	return filepath.Join(s.dir, s.name(day, idx))
}

// join returns the full path of a file name in the segment directory.
func (s segmentScheme) join(fileName string) string {
	return filepath.Join(s.dir, fileName)
}

// parse recognises a segment file name strictly: exact "<base>-" prefix, a
// valid calendar date, an optional ".<n>" with n ≥ 1 and no leading zeros,
// and the exact extension. The returned day is local midnight in loc.
func (s segmentScheme) parse(fileName string, loc *time.Location) (day time.Time, idx int, ok bool) {
	rest, found := strings.CutPrefix(fileName, s.base+"-")
	if !found {
		return time.Time{}, 0, false
	}

	rest, found = strings.CutSuffix(rest, s.ext)
	if !found || len(rest) < len(dayLayout) {
		return time.Time{}, 0, false
	}

	day, err := time.ParseInLocation(dayLayout, rest[:len(dayLayout)], loc)
	if err != nil {
		return time.Time{}, 0, false
	}

	suffix := rest[len(dayLayout):]
	if suffix == "" {
		return day, 0, true
	}

	digits, found := strings.CutPrefix(suffix, ".")
	if !found || digits == "" || digits[0] == '0' || strings.TrimLeft(digits, "0123456789") != "" {
		return time.Time{}, 0, false
	}

	idx, err = strconv.Atoi(digits)
	if err != nil {
		return time.Time{}, 0, false
	}

	return day, idx, true
}

// dayOf truncates t to local midnight of its calendar day in loc.
func dayOf(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()

	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}
