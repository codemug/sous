// Package reqlog is an append-only audit log of chat completion requests:
// who sent it and exactly what they sent, one line per request.
//
// ONE FILE PER DAY, never rewritten. Retention is enforced by deleting whole
// files past the cutoff, not by rewriting or truncating one that is still
// within it - which is what makes "append-only" a fact about the files that
// are live rather than a promise that stops being true the moment cleanup
// runs. It also makes cleanup itself trivial: a filename IS the cutoff test,
// nothing has to be parsed or rewound to apply it.
//
// PLAINTEXT, ON PURPOSE, AND THAT IS A REAL COST. The payload is a chat
// request verbatim - whatever the caller sent, prompts included - because an
// audit log that redacts the thing being audited answers nothing. Every file
// is written 0600 in a 0700 directory, the same posture as internal/hf's
// token, but unlike a token this is unbounded in volume and content is
// exactly what a client chose to send. Retention is the only thing standing
// between "recent" and "forever"; there is no size cap and no redaction.
package reqlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entry is one logged request.
type Entry struct {
	Time       time.Time       `json:"time"`
	Sender     string          `json:"sender"`
	RemoteAddr string          `json:"remote_addr,omitempty"`
	Model      string          `json:"model,omitempty"`
	Body       json.RawMessage `json:"body"`
}

// Writer appends entries under Dir, one file per UTC day.
type Writer struct {
	Dir string

	mu   sync.Mutex
	day  string
	file *os.File
	buf  *bufio.Writer
}

const filePrefix = "requests-"
const fileSuffix = ".jsonl"

func nameFor(t time.Time) string {
	return filePrefix + t.UTC().Format("2006-01-02") + fileSuffix
}

// Log appends one entry. Errors are returned rather than swallowed, but a
// caller on the request path should log-and-continue rather than fail the
// request over it: a client's completion should not break because an audit
// write failed.
func (w *Writer) Log(e Entry) error {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("reqlog: encode: %w", err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	day := e.Time.UTC().Format("2006-01-02")
	if w.file == nil || w.day != day {
		if w.buf != nil {
			_ = w.buf.Flush()
		}
		if w.file != nil {
			_ = w.file.Close()
		}
		if err := os.MkdirAll(w.Dir, 0o700); err != nil {
			return fmt.Errorf("reqlog: mkdir: %w", err)
		}
		f, err := os.OpenFile(filepath.Join(w.Dir, filePrefix+day+fileSuffix),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("reqlog: open: %w", err)
		}
		w.file, w.day, w.buf = f, day, bufio.NewWriter(f)
	}

	if _, err := w.buf.Write(line); err != nil {
		return fmt.Errorf("reqlog: write: %w", err)
	}
	if err := w.buf.WriteByte('\n'); err != nil {
		return fmt.Errorf("reqlog: write: %w", err)
	}
	// Flushed every line rather than left to the buffer's own threshold: an
	// audit log that loses its last few entries on a crash because they were
	// still sitting in a userspace buffer has quietly failed at the one thing
	// it exists for.
	return w.buf.Flush()
}

// Cleanup removes daily files whose date is older than days before now.
// days <= 0 means "keep forever" - nothing is removed. Returns how many files
// were deleted, so a caller can report or log it.
func (w *Writer) Cleanup(days int, now time.Time) (int, error) {
	if days <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(w.Dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reqlog: list: %w", err)
	}
	cutoff := now.UTC().AddDate(0, 0, -days).Format("2006-01-02")

	w.mu.Lock()
	openDay := w.day
	w.mu.Unlock()

	removed := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
			continue
		}
		day := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileSuffix)
		if day >= cutoff {
			continue
		}
		// The day currently open for writing is never removed even if the
		// retention window is 0 days wide - closing a file out from under an
		// in-flight buffered writer is a bug, not a feature, and "keep
		// today's traffic" is what every operator means by a short window.
		if day == openDay {
			continue
		}
		if err := os.Remove(filepath.Join(w.Dir, name)); err != nil {
			return removed, fmt.Errorf("reqlog: remove %s: %w", name, err)
		}
		removed++
	}
	return removed, nil
}

// RetentionStore holds the configured retention window, days, on disk.
//
// A SINGLE PLAIN INT, not YAML. There is exactly one value; the file format
// only needs to round-trip an integer, and a human who greps the data
// directory should not need to parse YAML to see what it says.
type RetentionStore struct {
	path string
	mu   sync.RWMutex
}

// DefaultRetentionDays is what a freshly configured node keeps before this is
// ever touched. Long enough to investigate a report of bad output made days
// after the fact; short enough that a node nobody prunes does not accumulate
// prompts indefinitely.
const DefaultRetentionDays = 30

func NewRetentionStore(dataDir string) (*RetentionStore, error) {
	dir := filepath.Join(dataDir, "reqlogs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &RetentionStore{path: filepath.Join(dir, "retention-days")}, nil
}

// Days returns the configured retention, or DefaultRetentionDays if unset or
// unreadable - a corrupt or missing setting must not silently mean "forever".
func (s *RetentionStore) Days() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		return DefaultRetentionDays
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 {
		return DefaultRetentionDays
	}
	return n
}

// SetDays stores the retention window. 0 is accepted and means "delete
// everything not from today" - a deliberate operator choice, not a mistake to
// guard against.
func (s *RetentionStore) SetDays(n int) error {
	if n < 0 {
		return fmt.Errorf("reqlog: retention cannot be negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".retention-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(strconv.Itoa(n) + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// Files lists the daily log files present, oldest first, for the admin page
// to show what actually exists rather than just the configured policy.
func (w *Writer) Files() ([]FileInfo, error) {
	entries, err := os.ReadDir(w.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, FileInfo{
			Day:   strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileSuffix),
			Bytes: info.Size(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out, nil
}

type FileInfo struct {
	Day   string
	Bytes int64
}
