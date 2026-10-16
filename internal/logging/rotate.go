package logging

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// defaultRetryInterval throttles reopen/rotation retries and write retries
	// while the log file is failing.
	defaultRetryInterval = 30 * time.Second
	// maxIndexProbe bounds the O_EXCL retry loop when a segment name is taken.
	maxIndexProbe = 1000
	dirPerm       = 0o700
	filePerm      = 0o600
	// defaultLogFileName is the file name used when logging.file_path names a
	// directory.
	defaultLogFileName = "ready.log"
)

// minPlausibleYear marks segment days earlier than 2025-01-01 as implausible
// (clock unset at boot, e.g. an RTC-less Pi at 1970): they are exempt from the
// retention day window so the current boot's log survives NTP sync.
const minPlausibleYear = 2025

// rotateOptions configures a RotatingFile.
type rotateOptions struct {
	// Path is logging.file_path; segments live in its directory.
	Path           string
	MaxSegmentSize int64
	MaxTotalSize   int64
	RetentionDays  int
	// Now and Location drive calendar days (default time.Now / time.Local).
	Now      func() time.Time
	Location *time.Location
	// FS is the filesystem (default osFS).
	FS fileSystem
	// RetryInterval throttles retries while failing (default 30s).
	RetryInterval time.Duration
	// Report is called once per healthy↔failing transition of the log file.
	Report func(healthy bool, err error)
	// ReportRetention is called for each retention failure (listing or
	// deletion); the run continues.
	ReportRetention func(err error)
	// afterRetention, when set, is called at the end of every retention run
	// (test hook; nil in production).
	afterRetention func()
}

// RotatingFile is the log-file sink: an io.Writer writing one zerolog record
// per Write call into a series of dated, never-renamed segments, rotating at
// local midnight or at MaxSegmentSize, with background retention. Runtime
// failures never surface to the caller: dropped records return success.
// See the architecture decision records and openspec change log-file-rotation (the design doc–D11).
type RotatingFile struct {
	opts   rotateOptions
	scheme segmentScheme
	lock   io.Closer

	mu sync.Mutex
	// active is the open handle of the reserved segment; nil after a failure
	// closed it or after Close.
	active logFile
	// activeName/activeDay/activeIdx identify the reserved segment. It stays
	// reserved (never deleted by retention) even while active is nil, so a
	// reopen after a failure can safely append to it.
	activeName  string
	activeDay   time.Time
	activeIdx   int
	needNewline bool
	openErr     error
	nextOpen    time.Time
	writeErr    error
	nextWrite   time.Time
	healthy     bool
	closed      bool

	size atomic.Int64

	signal chan struct{}
	stop   chan struct{}
	done   chan struct{}
}

// Guarantees at compile time that RotatingFile implements io.WriteCloser.
var _ io.WriteCloser = (*RotatingFile)(nil)

// openRotatingFile prepares the segment directory, takes the directory lock,
// selects the active segment and starts the retention worker.
// Startup failures are returned (fatal). The returned warnings are boot WARNs
// for the caller to log (legacy file, permission tightening failure).
func openRotatingFile(opts rotateOptions) (*RotatingFile, []string, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.Location == nil {
		opts.Location = time.Local
	}

	if opts.FS == nil {
		opts.FS = osFS{}
	}

	if opts.RetryInterval <= 0 {
		opts.RetryInterval = defaultRetryInterval
	}

	opts.Path = resolveLogPath(opts.FS, opts.Path)

	r := &RotatingFile{
		opts:    opts,
		scheme:  newSegmentScheme(opts.Path),
		healthy: true,
		signal:  make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}

	if err := opts.FS.MkdirAll(r.scheme.dir, dirPerm); err != nil {
		return nil, nil, fmt.Errorf("failed to create log directory %q: %w", r.scheme.dir, err)
	}

	lock, err := opts.FS.Lock(opts.Path + ".lock")
	if err != nil {
		if errors.Is(err, ErrLogDirLocked) {
			return nil, nil, fmt.Errorf("another ready serve is already writing to this log directory: %w", err)
		}

		return nil, nil, fmt.Errorf("failed to lock log directory %q: %w", r.scheme.dir, err)
	}

	r.lock = lock

	warnings, err := r.openInitial()
	if err != nil {
		_ = lock.Close()

		return nil, nil, err
	}

	go r.retentionLoop()

	r.triggerRetention()

	return r, warnings, nil
}

// resolveLogPath maps the directory form of logging.file_path onto the file
// form: a path ending with a separator, or naming an existing directory,
// becomes <dir>/ready.log, so "<runtime home>/logs/" yields ready-YYYY-MM-DD.log
// segments and ready.log.lock inside that directory.
func resolveLogPath(fsys fileSystem, filePath string) string {
	if strings.HasSuffix(filePath, "/") || strings.HasSuffix(filePath, string(filepath.Separator)) {
		return filepath.Join(filePath, defaultLogFileName)
	}

	if info, err := fsys.Stat(filePath); err == nil && info.IsDir() {
		return filepath.Join(filePath, defaultLogFileName)
	}

	return filePath
}

// openInitial selects and opens the startup segment: today's highest index,
// appended when under the threshold, else the next index, else index 0.
func (r *RotatingFile) openInitial() ([]string, error) {
	var warnings []string

	if info, err := r.opts.FS.Stat(r.opts.Path); err == nil && info.Mode().IsRegular() {
		warnings = append(warnings, fmt.Sprintf("legacy log file %q is no longer written; logs now go to dated segments %s (the file is left untouched and may be deleted)",
			r.opts.Path, r.scheme.name(r.today(), 0)))
	}

	today := r.today()

	maxIdx, found, err := r.maxIndex(today)
	if err != nil {
		return nil, err
	}

	if found {
		path := r.scheme.path(today, maxIdx)

		info, statErr := r.opts.FS.Stat(path)
		if statErr != nil {
			return nil, fmt.Errorf("failed to stat log segment %q: %w", path, statErr)
		}

		if info.Size() < r.opts.MaxSegmentSize {
			if warn, appendErr := r.openAppend(today, maxIdx, info); appendErr != nil {
				return nil, appendErr
			} else if warn != "" {
				warnings = append(warnings, warn)
			}

			return warnings, nil
		}
	}

	next := 0
	if found {
		next = maxIdx + 1
	}

	f, idx, err := r.createSegment(today, next)
	if err != nil {
		return nil, err
	}

	r.install(f, today, idx, 0)

	return warnings, nil
}

// openAppend opens an existing segment for append, tightening its mode to
// 0600 and arming needNewline when its last byte is not '\n'.
func (r *RotatingFile) openAppend(day time.Time, idx int, info fs.FileInfo) (string, error) {
	path := r.scheme.path(day, idx)

	f, err := r.opts.FS.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, filePerm)
	if err != nil {
		return "", fmt.Errorf("failed to open log segment %q: %w", path, err)
	}

	var warning string

	if info.Mode().Perm()&0o077 != 0 {
		if chmodErr := r.opts.FS.Chmod(path, filePerm); chmodErr != nil {
			warning = fmt.Sprintf("failed to tighten log segment %q to 0600: %v", path, chmodErr)
		}
	}

	r.install(f, day, idx, info.Size())
	r.needNewline = endsWithoutNewline(f, info.Size())

	return warning, nil
}

// endsWithoutNewline reports whether a file of the given size has a last
// byte other than '\n' (a record cut by a crash).
func endsWithoutNewline(f logFile, size int64) bool {
	if size == 0 {
		return false
	}

	var last [1]byte
	if _, err := f.ReadAt(last[:], size-1); err != nil {
		return false
	}

	return last[0] != '\n'
}

// install makes f the active, reserved segment.
func (r *RotatingFile) install(f logFile, day time.Time, idx int, size int64) {
	r.active = f
	r.activeDay = day
	r.activeIdx = idx
	r.activeName = r.scheme.name(day, idx)
	r.needNewline = false
	r.size.Store(size)
}

// createSegment creates a brand-new segment for day starting at index from,
// with O_EXCL so an existing file is never reopened.
func (r *RotatingFile) createSegment(day time.Time, from int) (logFile, int, error) {
	for idx := from; idx < from+maxIndexProbe; idx++ {
		path := r.scheme.path(day, idx)

		f, err := r.opts.FS.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_APPEND|os.O_RDWR, filePerm)
		if err == nil {
			return f, idx, nil
		}

		if !errors.Is(err, fs.ErrExist) {
			return nil, 0, fmt.Errorf("failed to create log segment %q: %w", path, err)
		}
	}

	return nil, 0, fmt.Errorf("failed to create log segment for %s: %d names taken", day.Format(dayLayout), maxIndexProbe)
}

// maxIndex returns the highest existing segment index for day.
func (r *RotatingFile) maxIndex(day time.Time) (maxIdx int, found bool, err error) {
	entries, err := r.opts.FS.ReadDir(r.scheme.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}

		return 0, false, fmt.Errorf("failed to list log directory %q: %w", r.scheme.dir, err)
	}

	for _, e := range entries {
		d, idx, ok := r.scheme.parse(e.Name(), r.opts.Location)
		if !ok || !d.Equal(day) {
			continue
		}

		if !found || idx > maxIdx {
			maxIdx, found = idx, true
		}
	}

	return maxIdx, found, nil
}

func (r *RotatingFile) today() time.Time {
	return dayOf(r.opts.Now(), r.opts.Location)
}

// Write writes one complete zerolog record. It never returns an error: on a
// file failure the record is dropped from the file only.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()

	if r.closed {
		r.mu.Unlock()

		return len(p), nil
	}

	now := r.opts.Now()
	today := dayOf(now, r.opts.Location)

	if r.active != nil && r.rotationDue(today, len(p)) && !now.Before(r.nextOpen) {
		r.rotate(today, now)
	}

	if r.active == nil && !now.Before(r.nextOpen) {
		r.reopen(today, now)
	}

	if r.active != nil && (r.writeErr == nil || !now.Before(r.nextWrite)) {
		r.writeRecord(p, now)
	}

	transition, healthy, cause := r.updateHealth()
	r.mu.Unlock()

	if transition && r.opts.Report != nil {
		r.opts.Report(healthy, cause)
	}

	return len(p), nil
}

// rotationDue reports whether a record of n bytes must go to a new segment:
// the day changed, or it would push a non-empty segment past the threshold.
func (r *RotatingFile) rotationDue(today time.Time, n int) bool {
	if !today.Equal(r.activeDay) {
		return true
	}

	size := r.size.Load()

	return size > 0 && size+int64(n) > r.opts.MaxSegmentSize
}

// rotate opens a new segment (next free index of today) and closes the old
// one only on success; on failure records keep going to the old segment and
// the next attempt is throttled.
func (r *RotatingFile) rotate(today, now time.Time) {
	if err := r.opts.FS.MkdirAll(r.scheme.dir, dirPerm); err != nil {
		r.openFailed(fmt.Errorf("failed to create log directory %q: %w", r.scheme.dir, err), now)

		return
	}

	next := 0

	maxIdx, found, err := r.maxIndex(today)
	if err != nil {
		r.openFailed(err, now)

		return
	}

	if found {
		next = maxIdx + 1
	}

	if today.Equal(r.activeDay) && r.activeIdx+1 > next {
		next = r.activeIdx + 1
	}

	f, idx, err := r.createSegment(today, next)
	if err != nil {
		r.openFailed(err, now)

		return
	}

	if r.active != nil {
		_ = r.active.Close()
	}

	r.install(f, today, idx, 0)
	r.openErr = nil
	r.writeErr = nil
	r.triggerRetention()
}

// reopen restores a handle after a failure closed it: the reserved segment is
// reopened for append when it is still today's and under the threshold (it
// was never eligible for retention), otherwise a new segment is created.
func (r *RotatingFile) reopen(today, now time.Time) {
	if !today.Equal(r.activeDay) || r.size.Load() >= r.opts.MaxSegmentSize {
		r.rotate(today, now)

		return
	}

	if err := r.opts.FS.MkdirAll(r.scheme.dir, dirPerm); err != nil {
		r.openFailed(fmt.Errorf("failed to create log directory %q: %w", r.scheme.dir, err), now)

		return
	}

	path := r.scheme.path(r.activeDay, r.activeIdx)

	f, err := r.opts.FS.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, filePerm)
	if err != nil {
		r.openFailed(fmt.Errorf("failed to reopen log segment %q: %w", path, err), now)

		return
	}

	var size int64
	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size()
	}

	pending := r.needNewline
	r.install(f, r.activeDay, r.activeIdx, size)
	r.needNewline = pending || endsWithoutNewline(f, size)
	r.openErr = nil
	r.writeErr = nil
}

func (r *RotatingFile) openFailed(err error, now time.Time) {
	r.openErr = err
	r.nextOpen = now.Add(r.opts.RetryInterval)
}

// writeRecord writes p (prefixed by '\n' when the previous record was cut)
// and tracks partial writes. A failure on a retry closes the handle so the
// next retry reopens it.
func (r *RotatingFile) writeRecord(p []byte, now time.Time) {
	data := p
	if r.needNewline {
		data = make([]byte, 0, len(p)+1)
		data = append(data, '\n')
		data = append(data, p...)
	}

	n, err := r.active.Write(data)
	if n > 0 {
		r.size.Add(int64(n))
		// Only a partial write can leave a line open; a complete record
		// always ends with '\n'.
		r.needNewline = n < len(data) && data[n-1] != '\n'
	}

	if err == nil && n < len(data) {
		err = io.ErrShortWrite
	}

	if err == nil {
		r.writeErr = nil

		return
	}

	retrying := r.writeErr != nil
	r.writeErr = fmt.Errorf("failed to write log segment %q: %w", r.activeName, err)
	r.nextWrite = now.Add(r.opts.RetryInterval)

	if retrying {
		_ = r.active.Close()
		r.active = nil
		r.nextOpen = now.Add(r.opts.RetryInterval)
	}
}

// updateHealth folds the open/write error state into the reported health and
// returns whether it changed.
func (r *RotatingFile) updateHealth() (transition, healthy bool, cause error) {
	cause = r.openErr
	if cause == nil {
		cause = r.writeErr
	}

	if cause == nil && r.active == nil {
		cause = errors.New("log segment is not open")
	}

	healthy = cause == nil
	if healthy == r.healthy {
		return false, healthy, cause
	}

	r.healthy = healthy

	return true, healthy, cause
}

// Close stops the retention worker, closes the active segment, releases the
// directory lock and makes every later Write a silent no-op.
func (r *RotatingFile) Close() error {
	r.mu.Lock()

	if r.closed {
		r.mu.Unlock()

		return nil
	}

	r.closed = true

	var errs []error

	if r.active != nil {
		errs = append(errs, r.active.Close())
		r.active = nil
	}

	r.mu.Unlock()

	close(r.stop)
	<-r.done

	errs = append(errs, r.lock.Close())

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("failed to close log file: %w", err)
	}

	return nil
}

// triggerRetention schedules a retention run without blocking (coalescing).
func (r *RotatingFile) triggerRetention() {
	select {
	case r.signal <- struct{}{}:
	default:
	}
}

func (r *RotatingFile) retentionLoop() {
	defer close(r.done)

	for {
		select {
		case <-r.stop:
			return
		case <-r.signal:
			r.runRetention()

			if r.opts.afterRetention != nil {
				r.opts.afterRetention()
			}
		}
	}
}

// segmentInfo is one non-active segment seen by a retention run.
type segmentInfo struct {
	name   string
	day    time.Time
	idx    int
	size   int64
	future bool
}

// runRetention applies the day window and the total cap. It
// never holds the write mutex while touching the filesystem.
func (r *RotatingFile) runRetention() {
	r.mu.Lock()
	reserved := r.activeName
	r.mu.Unlock()

	today := r.today()
	activeSize := r.size.Load()

	entries, err := r.opts.FS.ReadDir(r.scheme.dir)
	if err != nil {
		r.reportRetention(fmt.Errorf("failed to list log directory %q: %w", r.scheme.dir, err))

		return
	}

	loc := r.opts.Location
	y, m, d := today.Date()
	windowStart := time.Date(y, m, d-(r.opts.RetentionDays-1), 0, 0, 0, 0, loc)
	plausible := time.Date(minPlausibleYear, time.January, 1, 0, 0, 0, 0, loc)

	var kept []segmentInfo

	for _, e := range entries {
		if e.Name() == reserved || !e.Type().IsRegular() {
			continue
		}

		day, idx, ok := r.scheme.parse(e.Name(), loc)
		if !ok {
			continue
		}

		seg := segmentInfo{name: e.Name(), day: day, idx: idx, future: day.After(today)}

		if !seg.future && !day.Before(plausible) && day.Before(windowStart) {
			r.remove(seg.name)

			continue
		}

		info, infoErr := e.Info()
		if infoErr != nil {
			continue // vanished meanwhile
		}

		seg.size = info.Size()
		kept = append(kept, seg)
	}

	r.enforceCap(kept, activeSize)
}

// enforceCap deletes future-dated segments first, then the oldest by (day,
// index), while the total including the active segment exceeds the cap.
func (r *RotatingFile) enforceCap(segs []segmentInfo, activeSize int64) {
	total := activeSize
	for _, s := range segs {
		total += s.size
	}

	sort.Slice(segs, func(i, j int) bool {
		a, b := segs[i], segs[j]
		if a.future != b.future {
			return a.future
		}

		if !a.day.Equal(b.day) {
			return a.day.Before(b.day)
		}

		return a.idx < b.idx
	})

	for _, s := range segs {
		if total <= r.opts.MaxTotalSize {
			return
		}

		if r.remove(s.name) {
			total -= s.size
		}
	}
}

// remove deletes one segment, reporting failures; it returns true on success.
func (r *RotatingFile) remove(name string) bool {
	path := r.scheme.join(name)
	if err := r.opts.FS.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.reportRetention(fmt.Errorf("failed to delete log segment %q: %w", path, err))

		return false
	}

	return true
}

func (r *RotatingFile) reportRetention(err error) {
	if r.opts.ReportRetention != nil {
		r.opts.ReportRetention(err)
	}
}
