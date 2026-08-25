package reqlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogAppendsOneJSONLineWithSenderAndPayload(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Dir: dir}
	body := json.RawMessage(`{"model":"dflash2","messages":[{"role":"user","content":"hi"}]}`)
	when := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	if err := w.Log(Entry{Time: when, Sender: "voice demo", RemoteAddr: "10.0.0.5:1234",
		Model: "dflash2", Body: body}); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "requests-2026-08-26.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	var got Entry
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got.Sender != "voice demo" {
		t.Errorf("Sender = %q", got.Sender)
	}
	if string(got.Body) != string(body) {
		t.Errorf("Body = %s, want %s", got.Body, body)
	}
}

// EVERY LINE FLUSHED. A crash between requests must not lose entries sitting
// in a buffer - the whole point of an audit log is that it is there when
// something is being investigated after the fact.
func TestEachLineIsFlushedImmediately(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Dir: dir}
	if err := w.Log(Entry{Sender: "x", Body: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	// Read via a SEPARATE handle, proving the write reached the OS rather
	// than sitting in this process's own buffer.
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("files = %d", len(files))
	}
	b, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil || len(b) == 0 {
		t.Fatalf("nothing on disk after Log: err=%v len=%d", err, len(b))
	}
}

// ONE FILE PER UTC DAY, and a day boundary rolls to a new file without losing
// the old one.
func TestRotatesAtTheDayBoundary(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Dir: dir}
	d1 := time.Date(2026, 8, 25, 23, 59, 0, 0, time.UTC)
	d2 := time.Date(2026, 8, 26, 0, 1, 0, 0, time.UTC)
	if err := w.Log(Entry{Time: d1, Sender: "a", Body: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := w.Log(Entry{Time: d2, Sender: "b", Body: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"requests-2026-08-25.jsonl", "requests-2026-08-26.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s missing: %v", want, err)
		}
	}
}

// FILE PERMISSIONS. This log holds raw prompts; it must not be
// world-or-group readable.
func TestFilesAreNotReadableByAnyoneElse(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Dir: filepath.Join(dir, "reqlogs")}
	if err := w.Log(Entry{Sender: "x", Body: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(w.Dir)
	fi, err := os.Stat(filepath.Join(w.Dir, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}
	di, err := os.Stat(w.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o, want 700", perm)
	}
}

func mkFile(t *testing.T, dir, day string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filePrefix+day+fileSuffix), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// CLEANUP DELETES WHOLE FILES BY NAME, never touches one still in the window.
func TestCleanupRemovesOnlyFilesOlderThanTheWindow(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Dir: dir}
	for _, d := range []string{"2026-07-01", "2026-08-01", "2026-08-20", "2026-08-25", "2026-08-26"} {
		mkFile(t, dir, d)
	}
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	// 5-day window: keep 08-22 through 08-26.
	n, err := w.Cleanup(5, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("removed = %d, want 3", n)
	}
	remaining, _ := os.ReadDir(dir)
	got := map[string]bool{}
	for _, e := range remaining {
		got[e.Name()] = true
	}
	for _, want := range []string{"requests-2026-08-25.jsonl", "requests-2026-08-26.jsonl"} {
		if !got[filePrefix+want[len(filePrefix):]] && !got[want] {
			t.Errorf("%s should have survived a 5-day window", want)
		}
	}
	for _, gone := range []string{"requests-2026-07-01.jsonl", "requests-2026-08-01.jsonl", "requests-2026-08-20.jsonl"} {
		if got[gone] {
			t.Errorf("%s should have been removed", gone)
		}
	}
}

// A ZERO-OR-NEGATIVE WINDOW MEANS FOREVER, not "delete everything". Cleanup
// is opt-in via a positive number; the safe default when nothing is
// configured yet is to keep data, not silently discard it.
func TestNonPositiveWindowKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Dir: dir}
	mkFile(t, dir, "2020-01-01")
	if n, err := w.Cleanup(0, time.Now()); err != nil || n != 0 {
		t.Errorf("Cleanup(0) removed %d, err %v", n, err)
	}
	if n, err := w.Cleanup(-1, time.Now()); err != nil || n != 0 {
		t.Errorf("Cleanup(-1) removed %d, err %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "requests-2020-01-01.jsonl")); err != nil {
		t.Error("the file was removed despite a non-positive window")
	}
}

// TODAY'S FILE SURVIVES EVEN A ZERO-WIDTH WINDOW, because it is the one file
// with an active writer and closing it out from under a buffered append is a
// bug, not a feature.
func TestTodaysOpenFileIsNeverRemoved(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Dir: dir}
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	if err := w.Log(Entry{Time: now, Sender: "x", Body: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Cleanup(1, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "requests-2026-08-26.jsonl")); err != nil {
		t.Error("today's open file was removed by its own day's cleanup")
	}
}

func TestCleanupOnAMissingDirectoryIsNotAnError(t *testing.T) {
	w := &Writer{Dir: filepath.Join(t.TempDir(), "never-created")}
	n, err := w.Cleanup(30, time.Now())
	if err != nil || n != 0 {
		t.Errorf("Cleanup on a missing dir: n=%d err=%v", n, err)
	}
}

func TestRetentionStoreDefaultsWhenUnset(t *testing.T) {
	s, err := NewRetentionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Days(); got != DefaultRetentionDays {
		t.Errorf("Days() = %d, want default %d", got, DefaultRetentionDays)
	}
}

func TestRetentionStoreRoundTrips(t *testing.T) {
	s, err := NewRetentionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDays(14); err != nil {
		t.Fatal(err)
	}
	if got := s.Days(); got != 14 {
		t.Errorf("Days() = %d, want 14", got)
	}
}

// ZERO IS A VALID, DELIBERATE CHOICE ("keep only today"), not a sentinel for
// "unset" - only a MISSING file falls back to the default.
func TestRetentionStoreZeroIsDeliberate(t *testing.T) {
	s, err := NewRetentionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDays(0); err != nil {
		t.Fatal(err)
	}
	if got := s.Days(); got != 0 {
		t.Errorf("Days() = %d, want 0", got)
	}
}

func TestRetentionStoreRejectsNegative(t *testing.T) {
	s, err := NewRetentionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDays(-1); err == nil {
		t.Error("a negative retention was accepted")
	}
}

func TestFilesListsWhatExists(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Dir: dir}
	mkFile(t, dir, "2026-08-01")
	mkFile(t, dir, "2026-08-26")
	got, err := w.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Day != "2026-08-01" || got[1].Day != "2026-08-26" {
		t.Errorf("Files() = %+v", got)
	}
}
