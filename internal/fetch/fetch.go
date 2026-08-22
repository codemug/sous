// Package fetch downloads model weights from HuggingFace into the model cache.
//
// WHY SOUS DOES THIS AT ALL. Nothing here downloaded anything before: vLLM
// pulled weights itself on first start, inside the serving container, silently.
// On a node where a large model usually needs another one STOPPED to fit, that
// turns a deploy into "stop the chat model, then wait twenty minutes for 37 GiB
// to arrive, then start" - with the dashboard reporting `starting` throughout
// and no indication a download is even happening.
//
// Fetching separately makes the download visible and, more importantly, moves
// it OUT of the window where something else is stopped.
//
// STATE IS THE CONTAINER, not a record. A job's status, exit code and progress
// all come from Docker and its logs, so there is nothing to drift: the same
// reasoning that makes deployment phases derived rather than stored. A fetch
// that dies with Sous leaves a container whose state still says what happened.
package fetch

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/codemug/sous/internal/engine"
)

// Runtime is the slice of the engine a fetch needs.
type Runtime interface {
	StartJob(ctx context.Context, s engine.JobSpec) (string, error)
	JobStates(ctx context.Context) (map[string]engine.ContainerState, error)
	RemoveJob(ctx context.Context, name string) error
	Logs(ctx context.Context, name string) (io.ReadCloser, error)
}

type Manager struct {
	Runtime Runtime
	// ModelDir is the host directory bound as the HuggingFace cache, the same
	// one deployments mount - so weights fetched here are exactly the weights a
	// model later reads, rather than a second copy in a different layout.
	ModelDir string
	// Image carries huggingface_hub. The vLLM image is the sane default: it is
	// already on the node, and it is the same client that will read what it
	// writes. The host itself has neither `hf` nor an importable
	// huggingface_hub, so doing this outside a container is not an option.
	Image string
	// Secrets supplies the HuggingFace token, when one is configured.
	//
	// THE WHOLE POINT OF A GATED REPO. Accepting the licence on the Hub is tied
	// to an ACCOUNT, so an anonymous download of a gated repo returns 401 no
	// matter how many times the agreement was accepted in a browser. Without a
	// token this manager can only fetch public weights.
	Secrets interface{ Env() []string }
}

// secretEnv is nil-safe so a Manager built without secrets still works, which
// is what every public-repo download is.
func secretEnv(s interface{ Env() []string }) []string {
	if s == nil {
		return nil
	}
	return s.Env()
}

// downloadScript is what runs inside the fetch container.
//
// IT REPORTS ITS OWN PROGRESS. The previous version relied on parsing tqdm out
// of the container log, and on a real download that produced nothing at all -
// the Detail column read "—" for the entire transfer, which is how a 29 GiB
// pull that was stalling for minutes at a time looked identical to one running
// at full speed. tqdm's output depends on a TTY and on huggingface_hub's own
// progress-bar settings; a line this script prints itself does not.
//
// The progress thread measures BYTES ON DISK rather than asking the library,
// because that is the number that is true regardless of which download backend
// is in use, and it counts .incomplete files - which is where nearly all of a
// transfer lives until each shard finishes.
//
// hf_transfer is enabled ONLY IF IMPORTABLE. Setting the environment variable
// without the package installed makes huggingface_hub raise on import, which
// would turn a slow download into no download at all. The env var must also be
// set BEFORE huggingface_hub is imported - it is read at import time - which is
// why this happens at the top rather than next to the call.
const downloadScript = `import os, sys, threading, time, importlib.util

if importlib.util.find_spec("hf_transfer") is not None:
    os.environ["HF_HUB_ENABLE_HF_TRANSFER"] = "1"
    print("sous-note hf_transfer enabled", flush=True)
else:
    print("sous-note hf_transfer not installed; using the python downloader", flush=True)

from huggingface_hub import snapshot_download

repo = sys.argv[1]
patterns = ["*.json", "*.safetensors", "*.jinja", "*.txt", "*.model"]

home = os.environ.get("HF_HOME", os.path.expanduser("~/.cache/huggingface"))
cache = os.path.join(home, "hub", "models--" + repo.replace("/", "--"))

total = 0
try:
    from huggingface_hub import HfApi
    import fnmatch
    info = HfApi().model_info(repo, files_metadata=True)
    for f in info.siblings:
        if any(fnmatch.fnmatch(f.rfilename, g) for g in patterns):
            total += f.size or 0
except Exception as e:
    print("sous-note could not size the repo:", e, flush=True)
print("sous-total", total, flush=True)

def on_disk():
    # SKIP SYMLINKS. The hub cache keeps real bytes in blobs/ and symlinks to
    # them in snapshots/, and getsize follows a symlink - counting every
    # finished file twice, which reported 975 KB against a 487 KB repo and
    # would have put every download past 100%.
    n = 0
    for root, _, files in os.walk(cache):
        for f in files:
            p = os.path.join(root, f)
            try:
                if os.path.islink(p):
                    continue
                n += os.path.getsize(p)
            except OSError:
                pass
    return n

stop = threading.Event()

def report():
    # A rate measured against the START would hide a stall behind a good
    # average. This one is over the last interval, so a stall reads as a stall.
    last, last_t = on_disk(), time.time()
    while not stop.wait(5):
        try:
            now, t = on_disk(), time.time()
            rate = (now - last) / max(t - last_t, 0.001)
            pct = (" %.1f%%" % (now * 100.0 / total)) if total else ""
            print("sous-progress %d %d %.0f%s" % (now, total, rate, pct), flush=True)
            last, last_t = now, t
        except Exception:
            pass

th = threading.Thread(target=report, daemon=True)
th.start()
try:
    # max_workers above the default 8: this uplink drops roughly one connection
    # in ten, and more parallel transfers ride out an individual drop instead of
    # the whole download pausing on one stalled connection.
    p = snapshot_download(repo, allow_patterns=patterns, max_workers=16)
finally:
    stop.set()
print("sous-progress %d %d 0" % (on_disk(), total), flush=True)
print("downloaded to", p, flush=True)`

// repoRe matches a HuggingFace repo id: owner/name, with the characters the Hub
// actually permits. Anchored, because this string reaches a container command
// line and a shell-shaped value must never get that far.
var repoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidRepo reports whether a string is a well-formed repo id.
func ValidRepo(s string) bool { return repoRe.MatchString(s) }

// slug turns a repo id into a container-name-safe token.
//
// Docker names allow a narrow set, and "/" is not in it. The mapping is
// reversible enough to read at a glance, which matters when the only place a
// running fetch is visible is `docker ps`.
func slug(repo string) string {
	s := strings.ToLower(repo)
	s = strings.ReplaceAll(s, "/", "--")
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
	return s
}

// Name is the job container's name for a repo.
func Name(repo string) string { return engine.JobName(slug(repo)) }

// RepoLabel holds the repo id exactly as given.
//
// The container NAME cannot: Docker requires lowercase, and HuggingFace ids are
// case-sensitive - Qwen/Qwen3.6-35B-A3B-FP8 is not qwen/qwen3.6-35b-a3b-fp8.
// Recovering an id from a name produced something that looked right, listed
// wrong, and 404s if anyone copies it.
const RepoLabel = "sous.repo"

// Phase mirrors the deployment phases, deliberately: an operator reading the
// dashboard should not have to learn a second vocabulary for a second kind of
// slow operation.
type Phase string

const (
	PhaseDownloading Phase = "downloading"
	PhaseDone        Phase = "done"
	PhaseFailed      Phase = "failed"
	// PhaseAbsent - no job container, which says nothing about whether the
	// weights are present. The larder answers that.
	PhaseAbsent Phase = "absent"
)

// Job is one fetch as it stands.
type Job struct {
	Repo     string `json:"repo"`
	Phase    Phase  `json:"phase"`
	ExitCode int    `json:"exit_code,omitempty"`
	// Detail is the last meaningful line of progress, or the reason it failed.
	Detail string `json:"detail,omitempty"`
}

// Start begins a download and returns immediately.
//
// Idempotent against a fetch already in flight: asking twice for the same repo
// joins the existing job rather than starting a second container writing to the
// same directory, which is a corrupted cache waiting to happen.
func (m *Manager) Start(ctx context.Context, repo string) (Job, error) {
	repo = strings.TrimSpace(repo)
	if !ValidRepo(repo) {
		return Job{}, fmt.Errorf("%q is not a HuggingFace repo id (expected owner/name)", repo)
	}
	name := Name(repo)

	states, err := m.Runtime.JobStates(ctx)
	if err == nil {
		if st, ok := states[name]; ok {
			if st.Running() {
				return Job{Repo: repo, Phase: PhaseDownloading,
					Detail: "already downloading"}, nil
			}
			// A finished or failed job of the same name blocks the new
			// container; clear it so a retry is possible without a manual
			// docker rm.
			_ = m.Runtime.RemoveJob(ctx, name)
		}
	}

	spec := engine.JobSpec{
		Name:   name,
		Image:  m.Image,
		Binds:  []string{m.ModelDir + ":/root/.cache/huggingface"},
		Env:    append([]string{"HF_HOME=/root/.cache/huggingface"}, secretEnv(m.Secrets)...),
		Labels: map[string]string{RepoLabel: repo},
		// The image's own entrypoint is a model server. Without overriding it,
		// the command below arrives as arguments to vLLM, which starts up and
		// dies with "Failed to infer device type" on a container that was only
		// ever meant to download a file.
		Entrypoint: []string{"python3"},
		// The repo id is passed as an ARGUMENT, never interpolated into a shell
		// string. It is validated above as well, but a value that reaches a
		// command line should not depend on that validation being right.
		Cmd: []string{"-c", downloadScript, repo},
	}
	if _, err := m.Runtime.StartJob(ctx, spec); err != nil {
		return Job{}, fmt.Errorf("start fetch for %s: %w", repo, err)
	}
	return Job{Repo: repo, Phase: PhaseDownloading}, nil
}

// Status reports where a fetch has got to.
func (m *Manager) Status(ctx context.Context, repo string) Job {
	name := Name(repo)
	states, err := m.Runtime.JobStates(ctx)
	if err != nil {
		return Job{Repo: repo, Phase: PhaseAbsent}
	}
	st, ok := states[name]
	if !ok {
		return Job{Repo: repo, Phase: PhaseAbsent}
	}
	j := Job{Repo: repo, ExitCode: st.ExitCode}
	switch {
	case st.Running():
		j.Phase = PhaseDownloading
		j.Detail = m.progress(ctx, name)
	case st.Status == "exited" && st.ExitCode == 0:
		j.Phase = PhaseDone
	default:
		j.Phase = PhaseFailed
		j.Detail = m.lastError(ctx, name)
		if j.Detail == "" {
			j.Detail = fmt.Sprintf("exited with code %d", st.ExitCode)
		}
	}
	return j
}

// List reports every fetch Docker still knows about.
func (m *Manager) List(ctx context.Context) []Job {
	states, err := m.Runtime.JobStates(ctx)
	if err != nil {
		return nil
	}
	out := make([]Job, 0, len(states))
	for name, st := range states {
		// The label is the id as typed. Falling back to the name only covers a
		// container created before labels existed, and is knowingly lossy.
		repo := st.Labels[RepoLabel]
		if repo == "" {
			repo = unslug(strings.TrimPrefix(name, engine.JobPrefix))
		}
		out = append(out, m.Status(ctx, repo))
	}
	return out
}

// Forget removes a finished job's container, so the row disappears.
func (m *Manager) Forget(ctx context.Context, repo string) error {
	if !ValidRepo(repo) {
		return fmt.Errorf("%q is not a repo id", repo)
	}
	return m.Runtime.RemoveJob(ctx, Name(repo))
}

// unslug recovers "owner/name" from a container name. LOSSY - the name is
// lowercased, so this cannot restore the original casing. Only used for
// containers with no repo label, which means ones created before labels
// existed; everything since carries the id as typed.
func unslug(s string) string { return strings.Replace(s, "--", "/", 1) }

// progress returns the most recent line that looks like download progress.
//
// huggingface_hub writes tqdm bars to the log; the last one carrying a percent
// is the useful line. This is display only - nothing branches on it - so a
// parse that misses is a missing detail, never a wrong state.
// Tail returns the last lines of a download's log.
//
// THE MISSING VIEW. Diagnosing a slow download meant polling the larder's byte
// count from OUTSIDE Sous, because the job's own output was not reachable from
// anywhere - the container had the answer and nothing exposed it.
func (m *Manager) Tail(ctx context.Context, repo string, n int) []string {
	return m.tail(ctx, Name(repo), n)
}

// reProgress matches the line the download script prints every five seconds:
//
//	sous-progress <bytes-on-disk> <total-bytes> <bytes-per-second> [pct]
var reProgress = regexp.MustCompile(`^sous-progress (\d+) (\d+) (\d+)`)

// progress is the sentence under a running download.
//
// THE SCRIPT'S OWN LINE FIRST. Parsing tqdm was the previous approach and it
// produced nothing on a real transfer, so the column read "—" for 29 GiB - a
// download stalling for minutes looked exactly like one running at full speed.
// The tqdm heuristics stay as a fallback for a job started by an older Sous,
// whose container is still running and cannot be asked to print anything new.
func (m *Manager) progress(ctx context.Context, name string) string {
	lines := m.tail(ctx, name, 60)
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if mm := reProgress.FindStringSubmatch(l); mm != nil {
			done, _ := strconv.ParseInt(mm[1], 10, 64)
			total, _ := strconv.ParseInt(mm[2], 10, 64)
			rate, _ := strconv.ParseInt(mm[3], 10, 64)
			return describe(done, total, rate)
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.Contains(l, "%|") || strings.Contains(l, "it/s") || strings.Contains(l, "B/s") {
			return trimTo(l, 90)
		}
	}
	if len(lines) > 0 {
		return trimTo(strings.TrimSpace(lines[len(lines)-1]), 90)
	}
	return ""
}

// describe turns three numbers into the sentence an operator needs.
//
// A RATE OF ZERO IS SAID OUT LOUD. "3.3 of 28.8 GiB" beside a stalled transfer
// reads as progress; "stalled" does not. This is the whole reason the figure
// exists - the download that prompted it was moving in 128 MiB bursts with two
// minutes of nothing between them, and no view of it showed that.
//
// NO ETA when the rate is zero, and none from a lifetime average either: on a
// link that stalls, an average is an estimate of a past that is not coming
// back.
func describe(done, total, rate int64) string {
	var b strings.Builder
	b.WriteString(human(done))
	if total > 0 {
		b.WriteString(" of ")
		b.WriteString(human(total))
		// Clamped: a figure over 100% is a measurement bug rather than a
		// download that overachieved, and rendering it as fact sends whoever
		// reads it looking in the wrong place.
		pct := float64(done) * 100 / float64(total)
		if pct > 100 {
			pct = 100
		}
		b.WriteString(fmt.Sprintf(" (%.0f%%)", pct))
	}
	if rate <= 0 {
		b.WriteString(" · stalled")
		return b.String()
	}
	b.WriteString(" · ")
	b.WriteString(human(rate))
	b.WriteString("/s")
	if total > done {
		secs := float64(total-done) / float64(rate)
		b.WriteString(" · ")
		b.WriteString(eta(secs))
		b.WriteString(" left at this rate")
	}
	return b.String()
}

func human(b int64) string {
	const k, m, g = 1024.0, 1024.0 * 1024, 1024.0 * 1024 * 1024
	f := float64(b)
	switch {
	case f >= g:
		return strconv.FormatFloat(f/g, 'f', 1, 64) + " GiB"
	case f >= m:
		return strconv.FormatFloat(f/m, 'f', 0, 64) + " MiB"
	case f >= k:
		return strconv.FormatFloat(f/k, 'f', 0, 64) + " KiB"
	default:
		return strconv.FormatInt(b, 10) + " B"
	}
}

func eta(secs float64) string {
	switch {
	case secs >= 3600:
		return fmt.Sprintf("%.1fh", secs/3600)
	case secs >= 60:
		return fmt.Sprintf("%.0fm", secs/60)
	default:
		return fmt.Sprintf("%.0fs", secs)
	}
}

// lastError returns the exception line from a failed job, which is what an
// operator needs - a repo that does not exist and a disk that filled up fail
// identically at the level of an exit code.
func (m *Manager) lastError(ctx context.Context, name string) string {
	lines := m.tail(ctx, name, 60)
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if strings.Contains(l, "Error") || strings.Contains(l, "error") ||
			strings.Contains(l, "Exception") || strings.Contains(l, "No space") {
			return trimTo(l, 160)
		}
	}
	return ""
}

func (m *Manager) tail(ctx context.Context, name string, n int) []string {
	rc, err := m.Runtime.Logs(ctx, name)
	if err != nil {
		return nil
	}
	defer func() { _ = rc.Close() }()
	// Bounded: a download log is mostly progress bars and there is no reason to
	// hold megabytes of them to read the last line.
	buf := make([]byte, 0, 128<<10)
	tmp := make([]byte, 32<<10)
	for len(buf) < 128<<10 {
		k, err := rc.Read(tmp)
		buf = append(buf, tmp[:k]...)
		if err != nil {
			break
		}
	}
	// tqdm redraws with carriage returns, so a "line" here is either.
	text := strings.ReplaceAll(string(buf), "\r", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func trimTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
