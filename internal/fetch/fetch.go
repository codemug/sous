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
		Cmd: []string{
			"-c",
			`import sys
from huggingface_hub import snapshot_download
p = snapshot_download(sys.argv[1], allow_patterns=["*.json","*.safetensors","*.jinja","*.txt","*.model"])
print("downloaded to", p, flush=True)`,
			repo,
		},
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
func (m *Manager) progress(ctx context.Context, name string) string {
	lines := m.tail(ctx, name, 40)
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
