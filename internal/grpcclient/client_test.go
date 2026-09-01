package grpcclient

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codemug/sous/internal/engine"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// fakeServer records every DeployCommand it receives and replies
// immediately - enough to prove Client dispatches incoming commands to
// Handlers and sends the result back, without needing a real sous-api.
type fakeServer struct {
	pb.UnimplementedSousletServer
	received chan *pb.DeployCommand
}

func (f *fakeServer) Connect(stream pb.Souslet_ConnectServer) error {
	first, err := stream.Recv()
	if err != nil || first.GetSnapshot() == nil {
		return err
	}
	if err := stream.Send(&pb.Envelope{StreamId: "cmd-1", Payload: &pb.Envelope_Deploy{
		Deploy: &pb.DeployCommand{RecipeId: "dflash2", RecipeYaml: "id: dflash2\nkind: vllm\n"},
	}}); err != nil {
		return err
	}
	env, err := stream.Recv()
	if err != nil {
		return err
	}
	if res := env.GetDeployResult(); res != nil {
		f.received <- &pb.DeployCommand{RecipeId: res.RecipeId}
	}
	<-stream.Context().Done()
	return nil
}

func TestClientDispatchesIncomingDeployCommandsAndRepliesOnTheSameStreamID(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	fs := &fakeServer{received: make(chan *pb.DeployCommand, 1)}
	s := grpc.NewServer()
	pb.RegisterSousletServer(s, fs)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	c := &Client{
		// grpc.NewClient (unlike the deprecated Dial/DialContext) defaults to
		// the "dns" resolver scheme, which rejects an empty/bare target with
		// "missing address" before the custom dialer ever runs. The
		// "passthrough" scheme skips resolution entirely and hands the
		// target straight to WithContextDialer, which is what a bufconn
		// target needs - the dialer below ignores the address string anyway.
		Addr: "passthrough:///bufnet",
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
		NodeID: "asus-gx10",
		// Handlers.Snapshot (Task 5, unchanged here) unconditionally calls
		// Runtime.States, so a zero-value Handlers{} would panic on the nil
		// interface before the client ever reaches the dispatch loop this
		// test is actually about. fakeRuntime (handlers_test.go, same
		// package) is the existing in-memory double for deploy.Runtime.
		Handlers: &Handlers{Runtime: &fakeRuntime{}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	select {
	case got := <-fs.received:
		if got.RecipeId != "dflash2" {
			t.Fatalf("RecipeId = %q, want dflash2", got.RecipeId)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the client to dispatch and reply to a command")
	}
}

// twoCommandServer sends its configured Deploy commands back to back,
// without waiting for a reply to the first - exactly the situation that
// makes dispatch spawn two concurrent goroutines racing to call stream.Send
// on the same stream. commands must carry recipe YAML that passes
// engine.BuildSpec's validation (see validRecipeYAML in handlers_test.go),
// or HandleDeploy returns before ever calling Runtime.Start and the caller's
// synchronization on Start (e.g. barrierRuntime below) never engages.
type twoCommandServer struct {
	pb.UnimplementedSousletServer
	commands []*pb.DeployCommand
	received chan *pb.DeployResult
}

func (f *twoCommandServer) Connect(stream pb.Souslet_ConnectServer) error {
	if _, err := stream.Recv(); err != nil { // initial snapshot
		return err
	}
	for _, cmd := range f.commands {
		if err := stream.Send(&pb.Envelope{StreamId: cmd.RecipeId, Payload: &pb.Envelope_Deploy{
			Deploy: cmd,
		}}); err != nil {
			return err
		}
	}
	for range f.commands {
		env, err := stream.Recv()
		if err != nil {
			return err
		}
		if res := env.GetDeployResult(); res != nil {
			f.received <- res
		}
	}
	<-stream.Context().Done()
	return nil
}

// barrierRuntime makes two Handlers.HandleDeploy calls - each driven by a
// separately-dispatched Envelope - return at nearly the same instant, so
// their subsequent stream.Send calls are as likely as possible to overlap.
// That overlap is exactly what `go test -race` needs to see in order to
// prove client.go's sendMu actually serializes concurrent sends against the
// same stream, rather than the test merely passing because the two sends
// happened not to collide.
type barrierRuntime struct {
	fakeRuntime
	wg *sync.WaitGroup
}

func (r *barrierRuntime) Start(ctx context.Context, spec engine.Spec) (string, error) {
	r.wg.Done()
	r.wg.Wait()
	return r.fakeRuntime.Start(ctx, spec)
}

// TestClientSerializesConcurrentSendsWhenTwoCommandsArriveTogether proves
// that dispatch's one-goroutine-per-Envelope design (client.go) does not
// violate ClientStream's documented contract - "it is not safe to call
// SendMsg on the same stream in different goroutines" - when two commands
// arrive close enough together that their Handlers calls finish around the
// same time. Run with -race: without client.go's sendMu serializing these
// sends, this reliably trips the race detector because barrierRuntime forces
// maximum overlap between the two goroutines' stream.Send calls.
func TestClientSerializesConcurrentSendsWhenTwoCommandsArriveTogether(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	fs := &twoCommandServer{
		commands: []*pb.DeployCommand{
			{RecipeId: "race-a", RecipeYaml: validRecipeYAML(t, "race-a")},
			{RecipeId: "race-b", RecipeYaml: validRecipeYAML(t, "race-b")},
		},
		received: make(chan *pb.DeployResult, 2),
	}
	s := grpc.NewServer()
	pb.RegisterSousletServer(s, fs)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	var wg sync.WaitGroup
	wg.Add(2)
	c := &Client{
		Addr: "passthrough:///bufnet-race",
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
		NodeID:   "asus-gx10",
		Handlers: &Handlers{Runtime: &barrierRuntime{wg: &wg}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	got := map[string]bool{}
	for len(got) < 2 {
		select {
		case res := <-fs.received:
			got[res.RecipeId] = true
		case <-ctx.Done():
			t.Fatalf("timed out; only got replies for %v", got)
		}
	}
	if !got["race-a"] || !got["race-b"] {
		t.Fatalf("got replies for %v, want both race-a and race-b", got)
	}
}

// flakyServer sends its configured Deploy command and then immediately ends
// the RPC with an error, simulating the stream dying while the client's
// Handlers call for that command is still in flight. cmd must carry recipe
// YAML that passes engine.BuildSpec's validation (see validRecipeYAML in
// handlers_test.go), or HandleDeploy returns before ever calling
// Runtime.Start and slowRuntime's blocking below never engages.
type flakyServer struct {
	pb.UnimplementedSousletServer
	cmd *pb.DeployCommand
}

func (f *flakyServer) Connect(stream pb.Souslet_ConnectServer) error {
	if _, err := stream.Recv(); err != nil { // initial snapshot
		return err
	}
	if err := stream.Send(&pb.Envelope{StreamId: "cmd-slow", Payload: &pb.Envelope_Deploy{
		Deploy: f.cmd,
	}}); err != nil {
		return err
	}
	return errors.New("simulated stream failure")
}

// slowRuntime blocks Start until the test closes unblock, and closes entered
// the moment it does - proving to the test that the Handlers call is
// genuinely still in flight at the moment it decides the stream has died.
type slowRuntime struct {
	fakeRuntime
	entered chan struct{}
	unblock chan struct{}
}

func (r *slowRuntime) Start(ctx context.Context, spec engine.Spec) (string, error) {
	close(r.entered)
	<-r.unblock
	return r.fakeRuntime.Start(ctx, spec)
}

// syncWriter lets the test safely read log output that client.go's Run
// goroutine is concurrently writing to via the standard logger.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) Contains(substr string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Contains(w.buf.String(), substr)
}

func waitForLog(t *testing.T, w *syncWriter, substr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if w.Contains(substr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for a log line containing %q", substr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStaleDispatchGoroutineDoesNotPanicOrHangWhenItsStreamHasAlreadyDied
// answers the task's concurrency questions directly: when connectOnce
// returns because stream.Recv errored, a dispatch goroutine spawned for an
// earlier message can still be mid-flight inside a Handlers call. This test
// proves that goroutine's eventual stream.Send on the now-dead stream (a)
// does not panic (a panic here would crash the whole test binary, not just
// fail an assertion), (b) does not block forever (it errors and gets
// logged), and (c) does not stop Run from shutting down promptly once ctx is
// cancelled.
func TestStaleDispatchGoroutineDoesNotPanicOrHangWhenItsStreamHasAlreadyDied(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	fs := &flakyServer{cmd: &pb.DeployCommand{RecipeId: "slow", RecipeYaml: validRecipeYAML(t, "slow")}}
	s := grpc.NewServer()
	pb.RegisterSousletServer(s, fs)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	logW := &syncWriter{}
	prev := log.Writer()
	log.SetOutput(logW)
	t.Cleanup(func() { log.SetOutput(prev) })

	rt := &slowRuntime{entered: make(chan struct{}), unblock: make(chan struct{})}
	c := &Client{
		Addr: "passthrough:///bufnet-stale",
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
		NodeID:   "asus-gx10",
		Handlers: &Handlers{Runtime: rt},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	// Wait until the Handlers call for the Deploy command is genuinely in
	// flight (blocked inside Start).
	select {
	case <-rt.entered:
	case <-ctx.Done():
		t.Fatal("Handlers.HandleDeploy never started")
	}

	// Wait until Run has logged that connectOnce returned - i.e. the stream
	// and its underlying connection are already gone - while the dispatch
	// goroutine above is still blocked inside Start. Matched on this
	// client's own Addr (unique to this test, "bufnet-stale"), not the
	// generic "connection to" substring: log.SetOutput is process-global, so
	// a goroutine left running past a *different*, already-returned test
	// (e.g. the race test above, whose own reconnect-after-cancel also logs
	// "connection to ...") could otherwise satisfy this wait before this
	// test's own stream has actually died.
	waitForLog(t, logW, "connection to passthrough:///bufnet-stale")

	// Now let the stale goroutine finish its Handlers call and attempt
	// stream.Send on the dead stream. Matched on this command's own
	// StreamId ("cmd-slow"), for the same cross-test-pollution reason.
	close(rt.unblock)
	waitForLog(t, logW, "failed to send reply for stream cmd-slow")

	// The stale Send must not have hung (or the process would have crashed
	// above if it had panicked): Run must still shut down promptly once ctx
	// is cancelled.
	cancel()
	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after ctx cancellation - possible goroutine deadlock")
	}
}

// Lines splits the captured output into whole log lines, for tests that need
// to inspect a specific occurrence of a repeated message (e.g. the Nth
// "connection lost" line) rather than merely whether a substring ever
// appeared anywhere in the buffer.
func (w *syncWriter) Lines() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.buf.String()
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// waitForLogLines waits until at least n lines containing substr have been
// captured, then returns all of them in order. Used instead of a single
// Contains check so a test can assert on the content of a specific
// occurrence (e.g. "the 3rd retry log, not just any retry log").
func waitForLogLines(t *testing.T, w *syncWriter, substr string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		var matches []string
		for _, line := range w.Lines() {
			if strings.Contains(line, substr) {
				matches = append(matches, line)
			}
		}
		if len(matches) >= n {
			return matches
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d log lines containing %q; got %d: %v", n, substr, len(matches), matches)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stableThenDropServer completes the handshake (reads the initial snapshot)
// and then holds the stream open - closing stable the moment it does - until
// the test closes dropStable, at which point it ends the RPC with an error.
// Used to simulate a connection that became genuinely healthy after an
// earlier failure streak, and later disconnects again.
type stableThenDropServer struct {
	pb.UnimplementedSousletServer
	stable     chan struct{}
	dropStable chan struct{}
}

func (f *stableThenDropServer) Connect(stream pb.Souslet_ConnectServer) error {
	if _, err := stream.Recv(); err != nil { // the initial snapshot
		return err
	}
	close(f.stable)
	select {
	case <-f.dropStable:
		return errors.New("simulated later failure")
	case <-stream.Context().Done():
		return nil
	}
}

// TestRunResetsBackoffAfterAConnectionBecomesHealthyAgain is the regression
// test for the dead-code bug in the brief's own Run: connectOnce blocks
// inside its receive loop for as long as the stream is healthy and only
// ever returns on error, so "backoff = time.Second" on connectOnce's
// (unreachable) success path never ran - every subsequent disconnect's
// first retry inherited whatever backoff level the last failure streak had
// reached, even after the connection had been perfectly stable in between.
//
// This forces two dial failures first (ratcheting backoff 1s -> 2s -> 4s
// with no chance for a handshake to occur, let alone succeed), then a third
// connection that completes its handshake and stays open for a while, then
// drops. Without the fix, that final drop's retry log reports "4s" - the
// stale ratchet, never reset by the intervening healthy period. With the
// fix, it reports "1s".
func TestRunResetsBackoffAfterAConnectionBecomesHealthyAgain(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	fs := &stableThenDropServer{stable: make(chan struct{}), dropStable: make(chan struct{})}
	s := grpc.NewServer()
	pb.RegisterSousletServer(s, fs)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	// The first two dial attempts fail outright, before any gRPC stream (and
	// so before any handshake) is even attempted - a decisive way to
	// guarantee resetBackoff cannot fire for these two cycles, regardless of
	// any timing race between a client-side Send succeeding and a
	// server-side handler returning.
	var dialAttempts int32
	dial := func(ctx context.Context, _ string) (net.Conn, error) {
		if atomic.AddInt32(&dialAttempts, 1) <= 2 {
			return nil, errors.New("simulated dial failure")
		}
		return lis.DialContext(ctx)
	}

	logW := &syncWriter{}
	prev := log.Writer()
	log.SetOutput(logW)
	t.Cleanup(func() { log.SetOutput(prev) })

	c := &Client{
		Addr: "passthrough:///bufnet-flappy",
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(dial),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
		NodeID:   "asus-gx10",
		Handlers: &Handlers{Runtime: &fakeRuntime{}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	// Matched on this client's own Addr, for the same cross-test-pollution
	// reason as the stale-goroutine test above.
	const marker = "connection to passthrough:///bufnet-flappy lost"

	// Cycles 1 and 2: both dial failures, so backoff ratchets 1s -> 2s
	// without ever being reset.
	lines := waitForLogLines(t, logW, marker, 2)
	if !strings.Contains(lines[0], "retrying in 1s") {
		t.Fatalf("1st retry log = %q, want it to mention retrying in 1s", lines[0])
	}
	if !strings.Contains(lines[1], "retrying in 2s") {
		t.Fatalf("2nd retry log = %q, want it to mention retrying in 2s", lines[1])
	}

	// Cycle 3 connects and completes the handshake: this is the moment
	// resetBackoff fires inside connectOnce, well before connectOnce itself
	// returns (it won't return until the connection is later dropped below).
	select {
	case <-fs.stable:
	case <-ctx.Done():
		t.Fatal("the 3rd connection attempt never completed its handshake")
	}

	// Drop the now-stable connection and inspect what backoff its retry log
	// reports. Without the fix this is "4s" (the ratchet left over from
	// cycles 1-2, carried through the healthy period untouched); with the
	// fix, "1s" (reset the moment cycle 3's handshake succeeded).
	close(fs.dropStable)
	lines = waitForLogLines(t, logW, marker, 3)
	if !strings.Contains(lines[2], "retrying in 1s") {
		t.Fatalf("retry log after a stable connection dropped = %q, want it to mention retrying in 1s (backoff should have reset)", lines[2])
	}
}
