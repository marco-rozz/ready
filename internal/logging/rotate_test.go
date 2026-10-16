package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeClock is a goroutine-safe settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// faultFS wraps osFS and injects failures.
type faultFS struct {
	osFS

	mu          sync.Mutex
	failWrites  error // every Write fails with this error
	shortWrites bool  // next Write stores half the bytes, then errors
	failCreate  error // O_EXCL creations fail with this error
	opens       int
}

func (f *faultFS) OpenFile(name string, flag int, perm fs.FileMode) (logFile, error) {
	f.mu.Lock()
	f.opens++
	failCreate := f.failCreate
	f.mu.Unlock()

	if failCreate != nil && flag&os.O_EXCL != 0 {
		return nil, failCreate
	}

	lf, err := f.osFS.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}

	return &faultFile{logFile: lf, fs: f}, nil
}

func (f *faultFS) set(fn func(*faultFS)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

func (f *faultFS) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.opens
}

type faultFile struct {
	logFile
	fs *faultFS
}

func (w *faultFile) Write(p []byte) (int, error) {
	w.fs.mu.Lock()
	failWrites, short := w.fs.failWrites, w.fs.shortWrites
	w.fs.shortWrites = false
	w.fs.mu.Unlock()

	if short {
		n, _ := w.logFile.Write(p[:len(p)/2])

		return n, syscall.ENOSPC
	}

	if failWrites != nil {
		return 0, failWrites
	}

	return w.logFile.Write(p)
}

// reports records Report calls.
type reports struct {
	mu     sync.Mutex
	health []bool
}

func (r *reports) add(healthy bool, _ error) {
	r.mu.Lock()
	r.health = append(r.health, healthy)
	r.mu.Unlock()
}

func (r *reports) get() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]bool(nil), r.health...)
}

type rfHarness struct {
	t       *testing.T
	dir     string
	path    string
	clock   *fakeClock
	fs      *faultFS
	reports *reports
	runs    chan struct{}
	rf      *RotatingFile
	mod     func(*rotateOptions)
}

func rfDate(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
}

func newHarness(t *testing.T, start time.Time, mod func(*rotateOptions)) *rfHarness {
	t.Helper()

	dir := t.TempDir()
	h := &rfHarness{
		t:       t,
		dir:     dir,
		path:    filepath.Join(dir, legacyName),
		clock:   &fakeClock{t: start},
		fs:      &faultFS{},
		reports: &reports{},
		runs:    make(chan struct{}, 64),
	}

	return h.withOptions(mod)
}

func (h *rfHarness) withOptions(mod func(*rotateOptions)) *rfHarness {
	h.mod = mod

	return h
}

// open starts the RotatingFile (after the test pre-created any files).
func (h *rfHarness) open() []string {
	h.t.Helper()

	opts := rotateOptions{
		Path:           h.path,
		MaxSegmentSize: 1 << 20,
		MaxTotalSize:   100 << 20,
		RetentionDays:  7,
		Now:            h.clock.Now,
		Location:       time.UTC,
		FS:             h.fs,
		Report:         h.reports.add,
		afterRetention: func() { h.runs <- struct{}{} },
	}

	if h.mod != nil {
		h.mod(&opts)
	}

	rf, warnings, err := openRotatingFile(opts)
	if err != nil {
		h.t.Fatalf("openRotatingFile: %v", err)
	}

	h.rf = rf
	h.t.Cleanup(func() { _ = rf.Close() })
	h.waitRetention()

	return warnings
}

func (h *rfHarness) waitRetention() {
	h.t.Helper()

	select {
	case <-h.runs:
	case <-time.After(5 * time.Second):
		h.t.Fatal("retention run did not complete")
	}
}

func (h *rfHarness) write(msg string) {
	h.t.Helper()

	rec := fmt.Sprintf(`{"message":%q}`+"\n", msg)
	if n, err := h.rf.Write([]byte(rec)); err != nil || n != len(rec) {
		h.t.Fatalf("Write = %d, %v", n, err)
	}
}

func (h *rfHarness) writeRaw(p []byte) {
	h.t.Helper()

	if _, err := h.rf.Write(p); err != nil {
		h.t.Fatalf("Write: %v", err)
	}
}

func (h *rfHarness) create(name string, content []byte) {
	h.t.Helper()

	if err := os.WriteFile(filepath.Join(h.dir, name), content, 0o600); err != nil {
		h.t.Fatalf("WriteFile: %v", err)
	}
}

func (h *rfHarness) read(name string) string {
	h.t.Helper()

	data, err := os.ReadFile(filepath.Join(h.dir, name))
	if err != nil {
		h.t.Fatalf("ReadFile(%s): %v", name, err)
	}

	return string(data)
}

func (h *rfHarness) files() []string {
	h.t.Helper()

	entries, err := os.ReadDir(h.dir)
	if err != nil {
		h.t.Fatalf("ReadDir: %v", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	sort.Strings(names)

	return names
}

func (h *rfHarness) exists(name string) bool {
	_, err := os.Stat(filepath.Join(h.dir, name))

	return err == nil
}

func assertFiles(t *testing.T, got []string, want ...string) {
	t.Helper()

	sort.Strings(want)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("files = %v, want %v", got, want)
	}
}

// --- 4.1 midnight rotation ---------------------------------------------------

func TestRotateAtMidnight(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 23, 59), nil)
	h.open()

	h.write("before")
	h.clock.Set(rfDate(2026, 10, 1, 0, 0).Add(5 * time.Second))
	h.write("after")
	h.waitRetention()

	if got := h.read(segToday); !strings.Contains(got, "before") || strings.Contains(got, "after") {
		t.Errorf("09-30 segment = %q", got)
	}

	if got := h.read("ready-2026-10-01.log"); !strings.HasPrefix(got, `{"message":"after"}`) {
		t.Errorf("10-01 segment = %q, want record as first line", got)
	}

	// A silent day produces no segment.
	h.clock.Set(rfDate(2026, 10, 3, 9, 0))
	h.write("later")
	h.waitRetention()

	if h.exists("ready-2026-10-02.log") {
		t.Error("segment created for a day with no records")
	}

	assertFiles(t, h.files(), segToday, "ready-2026-10-01.log", "ready-2026-10-03.log", lockName)
}

// --- directory form of file_path --------------------------------------------

func TestDirectoryFormOfFilePath(t *testing.T) {
	for _, form := range []string{"trailing-separator", "existing-directory"} {
		t.Run(form, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "logs")
			path := dir

			switch form {
			case "trailing-separator":
				path = dir + string(filepath.Separator) // directory created by Open
			case "existing-directory":
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
			}

			clock := &fakeClock{t: rfDate(2026, 9, 30, 10, 0)}

			rf, warnings, err := openRotatingFile(rotateOptions{
				Path: path, MaxSegmentSize: 1 << 20, MaxTotalSize: 1 << 30, RetentionDays: 7,
				Now: clock.Now, Location: time.UTC,
			})
			if err != nil {
				t.Fatalf("openRotatingFile: %v", err)
			}

			if len(warnings) != 0 {
				t.Errorf("warnings = %v, want none (a directory is not a legacy file)", warnings)
			}

			_, _ = rf.Write([]byte("{}\n"))
			_ = rf.Close()

			for _, n := range []string{segToday, lockName} {
				if _, statErr := os.Stat(filepath.Join(dir, n)); statErr != nil {
					t.Errorf("%s missing in %s: %v", n, dir, statErr)
				}
			}
		})
	}
}

// --- 4.2 size rotation -------------------------------------------------------

func TestRotateOnSizeBeforeWrite(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), func(o *rotateOptions) { o.MaxSegmentSize = 100 })
	h.open()

	h.writeRaw(append(bytes.Repeat([]byte("a"), 59), '\n'))
	h.writeRaw(append(bytes.Repeat([]byte("b"), 59), '\n'))
	h.waitRetention()

	if got := h.read(segToday); len(got) != 60 {
		t.Errorf("first segment size = %d, want 60 (record not split)", len(got))
	}

	if got := h.read("ready-2026-09-30.1.log"); got != strings.Repeat("b", 59)+"\n" {
		t.Errorf("second segment = %q", got)
	}
}

func TestOversizedRecordWrittenWhole(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), func(o *rotateOptions) { o.MaxSegmentSize = 100 })
	h.open()

	big := append(bytes.Repeat([]byte("x"), 249), '\n')
	h.writeRaw(big)

	if got := h.read(segToday); got != string(big) {
		t.Errorf("oversized record not written whole into the empty segment (len %d)", len(got))
	}

	h.write("next")
	h.waitRetention()

	if !h.exists("ready-2026-09-30.1.log") {
		t.Error("record after an oversized segment did not rotate")
	}
}

// --- 4.3 restart -------------------------------------------------------------

func TestRestartAppendsUnderThreshold(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), func(o *rotateOptions) { o.MaxSegmentSize = 100 })
	h.create(segToday, bytes.Repeat([]byte("z"), 100))
	h.create("ready-2026-09-30.1.log", []byte(`{"old":true}`+"\n"))
	h.open()

	h.write("new")

	got := h.read("ready-2026-09-30.1.log")
	if !strings.HasPrefix(got, `{"old":true}`+"\n") || !strings.Contains(got, "new") {
		t.Errorf("append on restart = %q", got)
	}
}

func TestRestartAtThresholdUsesNextIndex(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), func(o *rotateOptions) { o.MaxSegmentSize = 100 })
	h.create("ready-2026-09-30.1.log", bytes.Repeat([]byte("z"), 100))
	h.open()
	h.write("new")

	if !strings.Contains(h.read("ready-2026-09-30.2.log"), "new") {
		t.Error("restart at threshold did not create the next index")
	}
}

func TestRestartOnNewDay(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), nil)
	h.create("ready-2026-09-29.log", []byte("{}\n"))
	h.open()
	h.write("new")

	if !strings.Contains(h.read(segToday), "new") {
		t.Error("restart on a new day did not create today's segment")
	}
}

func TestRestartAfterCrashStartsNewLine(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), nil)
	h.create(segToday, []byte(`{"cut":`))
	h.open()
	h.write("next")

	lines := strings.Split(strings.TrimSuffix(h.read(segToday), "\n"), "\n")
	if len(lines) != 2 || lines[0] != `{"cut":` || !json.Valid([]byte(lines[1])) {
		t.Errorf("lines after crash-truncated tail = %q", lines)
	}
}

func TestLegacyFileWarning(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), nil)
	h.create(legacyName, []byte("legacy\n"))

	warnings := h.open()
	if len(warnings) != 1 || !strings.Contains(warnings[0], legacyName) {
		t.Errorf("warnings = %v, want one legacy warning", warnings)
	}

	h.write("x")

	if h.read(legacyName) != "legacy\n" {
		t.Error("legacy file modified")
	}
}

// --- 4.4 retention -----------------------------------------------------------

func TestRetentionCalendarWindowAndForeignFiles(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), nil)
	for _, n := range []string{
		"ready-2026-09-22.log", "ready-2026-09-23.log", "ready-2026-09-23.1.log",
		"ready-2026-09-24.log", "ready-2026-09-29.log",
		legacyName, "thoughts.jsonl", "ready-notes.txt", "ready-2026-09-01.log.gz",
	} {
		h.create(n, []byte("{}\n"))
	}

	h.open()

	assertFiles(t, h.files(),
		"ready-2026-09-24.log", "ready-2026-09-29.log", segToday,
		legacyName, "thoughts.jsonl", "ready-notes.txt", "ready-2026-09-01.log.gz", lockName)
}

func TestRetentionTotalCapOrder(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), func(o *rotateOptions) { o.MaxTotalSize = 300 })
	hundred := bytes.Repeat([]byte("s"), 100)

	for _, n := range []string{
		"ready-2026-09-28.log", "ready-2026-09-29.log", "ready-2026-09-29.1.log",
		"ready-2026-10-05.log", "ready-1970-01-01.log",
	} {
		h.create(n, hundred)
	}

	h.create("foreign.log", bytes.Repeat([]byte("f"), 1000)) // uncounted
	h.open()

	// 500 bytes of segments + empty active: the future segment goes first,
	// then the implausible 1970 one (oldest), leaving exactly 300.
	assertFiles(t, h.files(),
		"ready-2026-09-28.log", "ready-2026-09-29.log", "ready-2026-09-29.1.log", segToday,
		"foreign.log", lockName)
}

func TestRetentionActiveCountedButKept(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), func(o *rotateOptions) {
		o.MaxTotalSize = 150
		o.MaxSegmentSize = 1000
	})
	h.create("ready-2026-09-29.log", bytes.Repeat([]byte("s"), 100))
	h.open()

	h.writeRaw(append(bytes.Repeat([]byte("a"), 199), '\n'))
	h.clock.Advance(time.Minute)
	h.rf.triggerRetention()
	h.waitRetention()

	assertFiles(t, h.files(), segToday, lockName)
}

func TestRetentionImplausibleAndFutureExemptFromWindow(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), nil)
	h.create("ready-1970-01-01.log", []byte("{}\n"))
	h.create("ready-2026-10-05.log", []byte("{}\n"))
	h.open()

	if !h.exists("ready-1970-01-01.log") || !h.exists("ready-2026-10-05.log") {
		t.Errorf("implausible/future segments deleted by the day window: %v", h.files())
	}
}

// --- 4.5 DST and clock jumps -------------------------------------------------

func TestLocalTimeGovernsDay(t *testing.T) {
	rome, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	h := newHarness(t, time.Date(2026, 9, 30, 23, 30, 0, 0, time.UTC), func(o *rotateOptions) { o.Location = rome })
	h.open()
	h.write("x")

	if !h.exists("ready-2026-10-01.log") {
		t.Errorf("files = %v, want a 2026-10-01 segment", h.files())
	}
}

func TestDSTDaysKeepCalendarWindow(t *testing.T) {
	rome, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	// 2026-03-29 is a 23h day (DST starts).
	h := newHarness(t, time.Date(2026, 3, 30, 0, 30, 0, 0, rome), func(o *rotateOptions) {
		o.Location = rome
		o.RetentionDays = 2
	})
	h.create("ready-2026-03-28.log", []byte("{}\n"))
	h.create("ready-2026-03-29.log", []byte("{}\n"))
	h.open()

	assertFiles(t, h.files(), "ready-2026-03-29.log", "ready-2026-03-30.log", lockName)

	// 2026-10-25 is a 25h day (DST ends): 23:30 and 00:10 land on distinct days.
	h.clock.Set(time.Date(2026, 10, 25, 23, 30, 0, 0, rome))
	h.write("late")
	h.waitRetention()
	h.clock.Set(time.Date(2026, 10, 26, 0, 10, 0, 0, rome))
	h.write("early")
	h.waitRetention()

	if !strings.Contains(h.read("ready-2026-10-25.log"), "late") || !strings.Contains(h.read("ready-2026-10-26.log"), "early") {
		t.Errorf("DST-end rotation wrong: %v", h.files())
	}
}

func TestClockJumpForwardFrom1970(t *testing.T) {
	h := newHarness(t, rfDate(1970, 1, 1, 0, 0), nil)
	h.open()
	h.write("boot")

	h.clock.Set(rfDate(2026, 9, 30, 10, 0))
	h.write("synced")
	h.waitRetention()

	if !strings.Contains(h.read("ready-1970-01-01.log"), "boot") {
		t.Error("boot log written before clock sync was deleted")
	}

	if !strings.Contains(h.read(segToday), "synced") {
		t.Error("no segment for the synced day")
	}
}

func TestClockBackOntoExistingDayCreatesNewIndex(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), nil)
	h.create("ready-2026-09-29.log", []byte("{\"a\":1}\n"))
	h.create("ready-2026-09-29.1.log", []byte("{\"b\":1}\n"))
	h.open()
	h.write("today")

	h.clock.Set(rfDate(2026, 9, 29, 12, 0))
	h.write("back")
	h.waitRetention()

	if got := h.read("ready-2026-09-29.2.log"); !strings.HasPrefix(got, `{"message":"back"}`) {
		t.Errorf("clock-back segment = %q", got)
	}

	if h.read("ready-2026-09-29.log") != "{\"a\":1}\n" || h.read("ready-2026-09-29.1.log") != "{\"b\":1}\n" {
		t.Error("existing segments of the earlier day were modified")
	}
}

// --- 4.6 failures ------------------------------------------------------------

func TestDiskFullReportsOnceAndThrottles(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), nil)
	h.open()
	h.write("ok")

	opensBefore := h.fs.openCount()
	h.fs.set(func(f *faultFS) { f.failWrites = syscall.ENOSPC })

	for range 600 { // 10 minutes, one record per second
		h.write("dropped")
		h.clock.Advance(time.Second)
	}

	if opens := h.fs.openCount() - opensBefore; opens > 600/30+1 {
		t.Errorf("reopen attempts = %d over 10 minutes, want ≤ %d", opens, 600/30+1)
	}

	h.fs.set(func(f *faultFS) { f.failWrites = nil })

	for range 61 {
		h.write("recovered")
		h.clock.Advance(time.Second)
	}

	if got := h.reports.get(); len(got) != 2 || got[0] || !got[1] {
		t.Errorf("reports = %v, want [false true]", got)
	}

	if !strings.Contains(h.read(segToday), "recovered") {
		t.Error("records not written after recovery")
	}
}

func TestShortWriteNextRecordOnNewLine(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), nil)
	h.open()

	h.fs.set(func(f *faultFS) { f.shortWrites = true })
	h.write("partial-record")
	h.clock.Advance(time.Minute)
	h.write("whole")

	lines := strings.Split(strings.TrimSuffix(h.read(segToday), "\n"), "\n")
	if len(lines) != 2 || !json.Valid([]byte(lines[1])) || !strings.Contains(lines[1], "whole") {
		t.Errorf("lines after short write = %q", lines)
	}
}

func TestFailedRotationKeepsOldSegment(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 23, 59), nil)
	h.open()
	h.write("before")

	h.fs.set(func(f *faultFS) { f.failCreate = errors.New("injected create failure") })
	h.clock.Set(rfDate(2026, 10, 1, 0, 0))
	h.write("fallback")

	if !strings.Contains(h.read(segToday), "fallback") {
		t.Error("record not appended to the old segment after a failed rotation")
	}

	h.fs.set(func(f *faultFS) { f.failCreate = nil })
	h.clock.Advance(31 * time.Second)
	h.write("rotated")
	h.waitRetention()

	if !strings.Contains(h.read("ready-2026-10-01.log"), "rotated") {
		t.Error("later attempt did not open the new segment")
	}

	if got := h.reports.get(); len(got) != 2 || got[0] || !got[1] {
		t.Errorf("reports = %v, want [false true]", got)
	}
}

func TestDirectoryRecreatedOnRotation(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 23, 0), nil)
	h.open()
	h.write("x")

	if err := os.RemoveAll(h.dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	h.clock.Set(rfDate(2026, 10, 1, 1, 0))
	h.write("y")
	h.waitRetention()

	if !strings.Contains(h.read("ready-2026-10-01.log"), "y") {
		t.Error("segment not created in the recreated directory")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(h.dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("recreated dir mode = %v, %v; want 0700", info, err)
		}
	}
}

func TestWritesAfterCloseAreSilent(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), nil)
	h.open()
	h.write("x")

	if err := h.rf.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	before := h.files()

	h.clock.Set(rfDate(2026, 10, 1, 10, 0))
	h.write("after-close")

	assertFiles(t, h.files(), before...)

	if got := h.reports.get(); len(got) != 0 {
		t.Errorf("reports after Close = %v, want none", got)
	}
}

// --- 4.6b lock and permissions ----------------------------------------------

func TestSecondWriterRejectedByLock(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), nil)
	h.open()
	h.write("first")

	opts := h.rf.opts
	opts.afterRetention = nil

	if _, _, err := openRotatingFile(opts); !errors.Is(err, ErrLogDirLocked) || !strings.Contains(err.Error(), lockName) {
		t.Fatalf("second open err = %v, want ErrLogDirLocked naming the lock file", err)
	}

	if err := h.rf.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rf, _, err := openRotatingFile(opts)
	if err != nil {
		t.Fatalf("reopen after Close: %v", err)
	}

	_ = rf.Close()
}

func TestSegmentPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes are not enforceable on Windows")
	}

	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), nil)
	h.create(segToday, []byte("{}\n"))

	if err := os.Chmod(filepath.Join(h.dir, segToday), 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	h.open()
	h.clock.Set(rfDate(2026, 10, 1, 10, 0))
	h.write("x")

	for _, n := range []string{segToday, "ready-2026-10-01.log", lockName} {
		info, err := os.Stat(filepath.Join(h.dir, n))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, %v; want 0600", n, info, err)
		}
	}
}

// --- 4.7 concurrency ---------------------------------------------------------

func TestConcurrentWritersWithRotationAndRetention(t *testing.T) {
	h := newHarness(t, rfDate(2026, 9, 30, 10, 0), nil)

	opts := rotateOptions{
		Path: h.path, MaxSegmentSize: 2048, MaxTotalSize: 1 << 30, RetentionDays: 7,
		Now: h.clock.Now, Location: time.UTC,
	}

	rf, _, err := openRotatingFile(opts)
	if err != nil {
		t.Fatalf("openRotatingFile: %v", err)
	}

	const writers, perWriter = 8, 300

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range perWriter {
				_, _ = fmt.Fprintf(rf, `{"w":%d,"i":%d}`+"\n", w, i)

				if i%100 == 0 {
					h.clock.Advance(time.Hour)
				}
			}
		}()
	}

	wg.Wait()

	if err = rf.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := 0

	for _, n := range h.files() {
		if !strings.HasPrefix(n, "ready-") {
			continue
		}

		for _, l := range strings.Split(strings.TrimSuffix(h.read(n), "\n"), "\n") {
			if !json.Valid([]byte(l)) {
				t.Fatalf("%s: invalid line %q", n, l)
			}

			lines++
		}
	}

	if lines != writers*perWriter {
		t.Errorf("lines = %d, want %d", lines, writers*perWriter)
	}
}
