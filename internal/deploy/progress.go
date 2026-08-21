package deploy

import (
	"bufio"
	"context"
	"github.com/codemug/sous/internal/engine"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Stage is one step of a model coming up.
type Stage struct {
	Name    string  `json:"name"`
	State   string  `json:"state"` // pending | skipped | running | done
	Seconds float64 `json:"seconds,omitempty"`
	Note    string  `json:"note,omitempty"`
}

// Progress is what a model is doing during the eight to ten minutes between
// "container is up" and "answers a request".
//
// DERIVED ON READ, never stored - the same reason Phase is. The stages are
// already in the boot log; the only new thing here is reading them.
//
// NO PERCENTAGE AND NO ETA, deliberately. A stage list with elapsed times and
// the duration of the last successful start is honest. A bar filling at a
// guessed rate is not, and on this hardware the guess would be wrong by
// minutes - weights load at disk speed, compilation does not.
type Progress struct {
	Stages     []Stage `json:"stages"`
	ElapsedSec float64 `json:"elapsed_seconds"`
	// LastRunSec is how long the previous successful start took, from the
	// stored observation. It is the only honest thing to say about how much
	// longer this one will be.
	LastRunSec float64 `json:"last_run_seconds,omitempty"`
}

const (
	StagePending = "pending"
	StageSkipped = "skipped"
	StageRunning = "running"
	StageDone    = "done"
)

var (
	reStartLoad  = regexp.MustCompile(`Starting to load model|Loading weights took`)
	reLoadDone   = regexp.MustCompile(`Model loading took ([0-9.]+) GiB memory and ([0-9.]+) seconds`)
	reCompile    = regexp.MustCompile(`torch\.compile took ([0-9.]+) s in total|Compiling a graph for`)
	reCompDone   = regexp.MustCompile(`torch\.compile took ([0-9.]+) s in total`)
	reGraphs     = regexp.MustCompile(`Profiling CUDA graph memory|Capturing CUDA graph`)
	reGraphsDone = regexp.MustCompile(`PIECEWISE=(\d+)`)
	reBackendUse = regexp.MustCompile(`Using ([A-Z_]+) attention backend`)
	reDownload   = regexp.MustCompile(`Fetching \d+ files|Downloading .*safetensors|%\|`)
	// A container that had to pull its image says so before anything else.
	rePulling = regexp.MustCompile(`Pulling from|Pull complete|Downloaded newer image`)
)

// Progress derives the stage list for a starting deployment.
//
// Only meaningful for a kind whose boot log has these stages. A container Sous
// did not generate a command for gets NO stepper rather than an invented one:
// showing five greyed steps for a service that has two is worse than showing
// nothing, because it implies knowledge that is not there.
func (m *Manager) Progress(ctx context.Context, rec Record, kind string) *Progress {
	if kind != "vllm" {
		return nil
	}
	rc, err := m.Runtime.Logs(ctx, engine.ContainerName(rec.RecipeID))
	if err != nil {
		return nil
	}
	defer func() { _ = rc.Close() }()

	p := &Progress{LastRunSec: rec.Observation.LoadSeconds}
	if !rec.StartedAt.IsZero() {
		p.ElapsedSec = time.Since(rec.StartedAt).Seconds()
	}

	var (
		sawPull, sawDownload          bool
		sawLoad, sawCompile, sawGraph bool
		loadSec, compileSec           float64
		loadGiB, backend, pieces      string
	)

	sc := bufio.NewScanner(rc)
	// 4 MiB, matching ParseBootLog. Multimodal profiling lines exceed the 64
	// KiB default and a truncated line silently stops matching.
	sc.Buffer(make([]byte, 0, 256<<10), 4<<20)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case rePulling.MatchString(l):
			sawPull = true
		case reDownload.MatchString(l):
			sawDownload = true
		}
		if mm := reLoadDone.FindStringSubmatch(l); mm != nil {
			sawLoad = true
			loadGiB = mm[1]
			loadSec, _ = strconv.ParseFloat(mm[2], 64)
		} else if reStartLoad.MatchString(l) {
			sawLoad = true
		}
		if mm := reCompDone.FindStringSubmatch(l); mm != nil {
			sawCompile = true
			compileSec, _ = strconv.ParseFloat(mm[1], 64)
		} else if reCompile.MatchString(l) {
			sawCompile = true
		}
		if mm := reGraphsDone.FindStringSubmatch(l); mm != nil {
			sawGraph = true
			pieces = mm[1]
		} else if reGraphs.MatchString(l) {
			sawGraph = true
		}
		if mm := reBackendUse.FindStringSubmatch(l); mm != nil {
			backend = mm[1]
		}
	}

	// The stages are ordered, and a later one being underway proves every
	// earlier one finished - which is how a stage that produced no log line of
	// its own still gets marked done.
	p.Stages = []Stage{
		stage("pull image", sawPull, sawDownload || sawLoad, "already present"),
		stage("download weights", sawDownload, sawLoad, "already cached"),
	}

	load := Stage{Name: "load weights", State: StagePending}
	switch {
	case loadSec > 0:
		load.State, load.Seconds = StageDone, loadSec
		if loadGiB != "" {
			load.Note = loadGiB + " GiB into device memory"
		}
	case sawLoad:
		load.State = StageRunning
	}
	p.Stages = append(p.Stages, load)

	comp := Stage{Name: "compile", State: StagePending}
	switch {
	case compileSec > 0:
		comp.State, comp.Seconds = StageDone, compileSec
		if backend != "" {
			comp.Note = backend
		}
	case sawCompile:
		comp.State = StageRunning
	}
	p.Stages = append(p.Stages, comp)

	graphs := Stage{Name: "capture CUDA graphs", State: StagePending}
	switch {
	case pieces != "":
		graphs.State = StageDone
		graphs.Note = "PIECEWISE=" + pieces
	case sawGraph:
		graphs.State = StageRunning
	}
	p.Stages = append(p.Stages, graphs)

	return p
}

// stage marks a step that leaves no completion line of its own.
//
// A pull and a download are both invisible in the happy case: the image was
// already there, the weights were already cached, and nothing is logged. What
// proves they are finished is a LATER stage having started, which is why
// "skipped" and "done" are different words here - one means it never had to
// happen, the other that it did.
func stage(name string, saw, laterStarted bool, skipNote string) Stage {
	switch {
	case saw && laterStarted:
		return Stage{Name: name, State: StageDone}
	case saw:
		return Stage{Name: name, State: StageRunning}
	case laterStarted:
		return Stage{Name: name, State: StageSkipped, Note: skipNote}
	default:
		return Stage{Name: name, State: StagePending}
	}
}

// Current returns the stage underway, for a one-line summary.
func (p *Progress) Current() string {
	if p == nil {
		return ""
	}
	for _, s := range p.Stages {
		if s.State == StageRunning {
			return s.Name
		}
	}
	return ""
}

// Summary is the sentence under a starting model.
//
// It says what is happening and how long the last successful start took, and
// nothing about how long this one will be - because nothing here knows.
func (p *Progress) Summary() string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	if c := p.Current(); c != "" {
		b.WriteString(c)
	} else {
		b.WriteString("starting")
	}
	if p.LastRunSec > 0 {
		b.WriteString(" · last start took ")
		b.WriteString(dur(p.LastRunSec))
	}
	return b.String()
}

func dur(sec float64) string {
	if sec < 60 {
		return strconv.FormatFloat(sec, 'f', 0, 64) + "s"
	}
	m := int(sec) / 60
	s := int(sec) % 60
	return strconv.Itoa(m) + "m " + strconv.Itoa(s) + "s"
}
