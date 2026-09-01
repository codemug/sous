package grpcclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"

	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"google.golang.org/grpc"
)

type Client struct {
	Addr        string
	DialOptions []grpc.DialOption
	NodeID      string
	Handlers    *Handlers
	PoolGiB     float64
	ReserveGiB  float64

	// SnapshotInterval is how often this node re-reports its full state
	// while a connection stays up. Zero means defaultSnapshotInterval.
	//
	// Not optional polish: sous-api's whole view of a node's residency
	// (capacity planning for the next deploy, the fleet cards, MarginGiB,
	// the "weights cached" chips) comes from these snapshots, and with a
	// connect-time snapshot alone that view was accurate only at the
	// instant a node connected. The concrete failure it caused: planOnNode
	// (internal/httpapi/deploy_grpc.go) sizes a deploy against
	// view.Deployments, so a second, third and fourth deploy onto an
	// already-full node all passed the capacity gate against a snapshot
	// taken when the node was empty - precisely the over-commitment this
	// codebase's capacity planning exists to prevent.
	SnapshotInterval time.Duration

	// proxyReqs assembles a proxied HTTP request's HTTPRequestHead + its
	// HTTPRequestChunk(s) - which dispatch below receives as separate,
	// individually-routed Envelopes correlated only by stream_id - back
	// into one (*pb.Envelope head, []byte body) pair before
	// handleProxyRequest is ever spawned. Keyed by stream_id, which
	// grpcserver mints as a UUID, so entries from a previous connection
	// generation can never collide with a new one; a value is written and
	// deleted entirely within dispatch's own synchronous (non-goroutine)
	// path (see connectOnce), so no additional locking is needed beyond
	// what sync.Map already gives its Store/Load/Delete calls.
	proxyReqs sync.Map // stream_id -> *pendingProxyReq
}

// pendingProxyReq accumulates one proxied request's body across however
// many HTTPRequestChunk messages arrive before Eof.
type pendingProxyReq struct {
	head *pb.Envelope
	body []byte
}

// Run dials sous-api and stays connected until ctx is cancelled,
// reconnecting with capped exponential backoff on any stream error. Every
// (re)connect sends one full NodeSnapshot before anything else - the
// level-triggered reconciliation the design calls for, with no attempt to
// carry state across a disconnect.
func (c *Client) Run(ctx context.Context) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// connectOnce blocks inside its receive loop for as long as the
		// stream stays healthy and only ever returns on error - a
		// connection that ran cleanly for days and then dropped must still
		// retry from the base backoff, not from wherever a much earlier
		// failure streak had ratcheted it to. That reset can't wait for
		// connectOnce to return (it never returns "successfully"), so it's
		// threaded in as a callback connectOnce invokes the moment the
		// connection is actually confirmed healthy - see its own comment.
		err := c.connectOnce(ctx, func() { backoff = time.Second })
		log.Printf("souslet: connection to %s lost: %v (retrying in %s)", c.Addr, err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		// Double, but clamp to maxBackoff rather than merely gating the
		// doubling on the pre-multiply value: backoff < maxBackoff is true
		// at 16s (16 < 30), so an unclamped `backoff *= 2` there lands on
		// 32s - a cap that's effectively ~32s, not the intended 30s.
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (c *Client) connectOnce(ctx context.Context, resetBackoff func()) error {
	conn, err := grpc.NewClient(c.Addr, c.DialOptions...)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := pb.NewSousletClient(conn)
	stream, err := client.Connect(ctx)
	if err != nil {
		return err
	}

	// sendMu serializes every SendMsg call made against this one stream.
	// ClientStream.SendMsg is explicitly documented as unsafe to call on the
	// same stream from different goroutines - and dispatch below runs one
	// goroutine per incoming Envelope, so two commands arriving close
	// together would otherwise both call stream.Send at once. Scoped to this
	// connectOnce call (one mutex per connection generation, not per
	// Client), so a goroutine left over from an older, already-dead stream
	// can never contend with - or block - a fresh reconnect's sends.
	var sendMu sync.Mutex

	if err := c.sendSnapshot(ctx, stream, &sendMu); err != nil {
		return err
	}

	// The initial snapshot went through: this connection generation is live
	// and the handshake with sous-api succeeded, independent of how long
	// the receive loop below ends up running before it eventually errors
	// out. This - not "connectOnce returned nil", which never happens - is
	// what Run treats as "the connection recovered", so it can reset its
	// backoff here rather than carrying a stale, ratcheted-up value into
	// this connection's eventual failure.
	if resetBackoff != nil {
		resetBackoff()
	}

	// Keep re-reporting this node's state for as long as the connection
	// lives. The design is level-triggered by intent - a full snapshot,
	// never a diff, "so a node's last snapshot is always exactly what that
	// node itself reported" (nodecatalog's package doc) - but level-
	// triggered only converges if the level is actually re-read. This makes
	// it periodic rather than once-per-connection. Scoped to this connection
	// generation: closing connDone stops this loop when connectOnce returns,
	// so a reconnect never leaves an older generation's ticker writing to a
	// dead stream.
	connDone := make(chan struct{})
	defer close(connDone)
	go c.snapshotLoop(ctx, stream, &sendMu, connDone)

	for {
		env, err := stream.Recv()
		if err != nil {
			return err
		}
		// HTTPRequestHead/Chunk are deliberately dispatched SYNCHRONOUSLY
		// (not via `go`, unlike every other envelope kind below), and
		// dispatch's own handling of them does only cheap, non-blocking map
		// bookkeeping before returning. This matters: stream.Recv() returns
		// envelopes for one stream_id in the order they were sent (a head,
		// then its chunk(s)), but two separately-spawned goroutines have no
		// such ordering guarantee between each other - a `go`-dispatched
		// chunk handler could in principle run before its own head handler
		// finished registering. Doing the registration/accumulation inline,
		// in this single loop, makes correctness independent of goroutine
		// scheduling; only the actual (potentially slow) forwarding work is
		// handed to its own goroutine, from inside dispatch, once a request
		// is fully assembled.
		if env.GetHttpReqHead() != nil || env.GetHttpReqChunk() != nil {
			c.dispatch(ctx, stream, &sendMu, env)
			continue
		}
		go c.dispatch(ctx, stream, &sendMu, env)
	}
}

// defaultSnapshotInterval is how often a connected node re-reports its full
// state when SnapshotInterval is left at zero. Short enough that sous-api's
// view of a fleet - and therefore the capacity gate on the next deploy -
// converges within seconds of anything changing on a node; long enough that
// a node with nothing happening on it costs one cheap Docker query and one
// small message every few seconds, not a stream of chatter.
const defaultSnapshotInterval = 15 * time.Second

func (c *Client) snapshotInterval() time.Duration {
	if c.SnapshotInterval > 0 {
		return c.SnapshotInterval
	}
	return defaultSnapshotInterval
}

// sendSnapshot builds this node's current state from Docker and sends it.
// Every snapshot on a connection goes through here - the connect-time one,
// the ticker's, and the post-command pushes - so they cannot drift apart in
// how they are built or locked.
func (c *Client) sendSnapshot(ctx context.Context, stream pb.Souslet_ConnectClient, sendMu *sync.Mutex) error {
	snap := c.Handlers.Snapshot(ctx, c.NodeID, c.PoolGiB, c.ReserveGiB)
	sendMu.Lock()
	defer sendMu.Unlock()
	return stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: snap}})
}

// snapshotLoop re-reports this node's state on a ticker until the connection
// it belongs to ends. A send failure means the stream is already gone (the
// receive loop in connectOnce is about to return the same failure and
// trigger a reconnect), so it just stops rather than retrying against a dead
// stream.
func (c *Client) snapshotLoop(ctx context.Context, stream pb.Souslet_ConnectClient, sendMu *sync.Mutex, connDone <-chan struct{}) {
	t := time.NewTicker(c.snapshotInterval())
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := c.sendSnapshot(ctx, stream, sendMu); err != nil {
				return
			}
		case <-connDone:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (c *Client) dispatch(ctx context.Context, stream pb.Souslet_ConnectClient, sendMu *sync.Mutex, env *pb.Envelope) {
	var reply *pb.Envelope
	// resnapshot marks the commands that CHANGE this node's state. The
	// ticker above would pick those changes up on its own within a few
	// seconds, but a deploy is exactly when sous-api most needs an accurate
	// picture (the operator's very next action is often another deploy onto
	// the same node, planned against this data), so these push a fresh
	// snapshot the moment the work is done instead of waiting for the next
	// tick.
	var resnapshot bool
	switch {
	case env.GetHttpReqHead() != nil:
		c.proxyReqs.Store(env.StreamId, &pendingProxyReq{head: env})
		return
	case env.GetHttpReqChunk() != nil:
		v, ok := c.proxyReqs.Load(env.StreamId)
		if !ok {
			return // a chunk for a stream_id with no registered head - drop defensively
		}
		pr := v.(*pendingProxyReq)
		chunk := env.GetHttpReqChunk()
		pr.body = append(pr.body, chunk.Data...)
		if !chunk.Eof {
			return
		}
		c.proxyReqs.Delete(env.StreamId)
		go c.handleProxyRequest(ctx, stream, sendMu, pr.head, pr.body)
		return
	case env.GetDeploy() != nil:
		reply = &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_DeployResult{
			DeployResult: c.Handlers.HandleDeploy(ctx, env.GetDeploy()),
		}}
		resnapshot = true
	case env.GetUndeploy() != nil:
		reply = &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_UndeployResult{
			UndeployResult: c.Handlers.HandleUndeploy(ctx, env.GetUndeploy()),
		}}
		resnapshot = true
	case env.GetFetch() != nil:
		reply = &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_FetchProgress{
			FetchProgress: c.Handlers.HandleFetch(ctx, env.GetFetch()),
		}}
	case env.GetDeleteWeights() != nil:
		reply = &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_DeleteWeightsResult{
			DeleteWeightsResult: c.Handlers.HandleDeleteWeights(ctx, env.GetDeleteWeights()),
		}}
		resnapshot = true
	default:
		return // snapshot/heartbeat/error - nothing this side needs to reply to
	}
	sendMu.Lock()
	err := stream.Send(reply)
	sendMu.Unlock()
	if err != nil {
		log.Printf("souslet: failed to send reply for stream %s: %v", env.StreamId, err)
		return
	}
	// AFTER the reply, never before: sous-api's caller is blocked on that
	// reply (grpcserver.Send correlates exactly one), and a snapshot is
	// worth nothing to it if it arrives at the cost of delaying the answer
	// it is waiting on.
	if resnapshot {
		if err := c.sendSnapshot(ctx, stream, sendMu); err != nil {
			log.Printf("souslet: failed to push a post-command snapshot: %v", err)
		}
	}
}

// handleProxyRequest forwards one fully-assembled proxied HTTP request (head
// + body, reassembled from possibly-many HTTPRequestChunk messages by
// dispatch above) to whichever local model container is currently serving
// the declared model, then streams the response back chunk by chunk AS IT
// ARRIVES - not buffered until the whole response completes - so SSE/
// chunked responses (token-by-token inference streaming) forward live.
//
// Every stream.Send call here goes through the send helper below, which
// takes sendMu - the same guard Task 6 already established for dispatch's
// own reply sends, because ClientStream.SendMsg is documented as unsafe to
// call concurrently from different goroutines on the same stream. This
// function runs in its own goroutine (spawned by dispatch once EOF is seen)
// alongside every other in-flight dispatch/handleProxyRequest goroutine on
// this same connection, so skipping sendMu here would reintroduce exactly
// the race Task 6 already fixed once.
func (c *Client) handleProxyRequest(ctx context.Context, stream pb.Souslet_ConnectClient, sendMu *sync.Mutex, headEnv *pb.Envelope, body []byte) {
	streamID := headEnv.StreamId
	head := headEnv.GetHttpReqHead()

	send := func(env *pb.Envelope) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(env)
	}

	resp, err := c.forwardToLocalContainer(ctx, head, body)
	if err != nil {
		if sendErr := send(&pb.Envelope{StreamId: streamID, Payload: &pb.Envelope_Error{
			Error: &pb.Error{Message: err.Error()},
		}}); sendErr != nil {
			log.Printf("souslet: failed to send proxy error for stream %s: %v", streamID, sendErr)
		}
		return
	}
	defer resp.Body.Close()

	headers := make(map[string]string, len(resp.Header))
	for k := range resp.Header {
		headers[k] = resp.Header.Get(k)
	}
	if err := send(&pb.Envelope{StreamId: streamID, Payload: &pb.Envelope_HttpRespHead{
		HttpRespHead: &pb.HTTPResponseHead{Status: int32(resp.StatusCode), Headers: headers},
	}}); err != nil {
		log.Printf("souslet: failed to send proxy response head for stream %s: %v", streamID, err)
		return
	}

	// Read-and-forward in small pieces, sending each one immediately - this
	// loop IS the streaming: a response held in a buffer until fully read
	// would turn token-by-token generation into one long pause followed by
	// a wall of text on the gateway's side, exactly the failure mode the
	// package doc for internal/gateway already calls out for the old local
	// httputil.ReverseProxy path.
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...) // buf is reused next iteration; the sent copy must not alias it
			if err := send(&pb.Envelope{StreamId: streamID, Payload: &pb.Envelope_HttpRespChunk{
				HttpRespChunk: &pb.HTTPResponseChunk{Data: chunk},
			}}); err != nil {
				log.Printf("souslet: failed to send proxy response chunk for stream %s: %v", streamID, err)
				return
			}
		}
		if rerr != nil {
			// Both a clean io.EOF and a real read error end the response the
			// same way from the gateway's point of view: a final Eof chunk.
			// A genuine mid-read error truncates the body, which is the
			// honest outcome to hand upstream rather than hanging the
			// gateway's RecvChunk forever waiting for one that will never
			// come.
			_ = send(&pb.Envelope{StreamId: streamID, Payload: &pb.Envelope_HttpRespChunk{
				HttpRespChunk: &pb.HTTPResponseChunk{Eof: true},
			}})
			return
		}
	}
}

// forwardToLocalContainer resolves the target local container and issues
// the actual request. The wire's HTTPRequestHead deliberately carries no
// recipe/model identifier (see proto/souslet/v1/souslet.proto - Task 1's
// already-committed, unmodified schema); this task cannot add one, so it
// wires against the SAME "learn the model from the body" mechanism
// internal/gateway/gateway.go's own Proxy already uses locally, and against
// Handlers.portFor (backed by the port state HandleDeploy already tracks -
// see handlers.go's rememberPort/forgetPort) rather than inventing a second
// port-tracking mechanism.
func (c *Client) forwardToLocalContainer(ctx context.Context, head *pb.HTTPRequestHead, body []byte) (*http.Response, error) {
	name := modelNameFromProxiedBody(head, body)
	if name == "" {
		return nil, fmt.Errorf("proxied request named no model")
	}
	port, ok := c.Handlers.portFor(name)
	if !ok {
		return nil, fmt.Errorf("no local deployment for model %q", name)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, head.GetPath())
	req, err := http.NewRequestWithContext(ctx, head.GetMethod(), url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build local request: %w", err)
	}
	for k, v := range head.GetHeaders() {
		switch k {
		case "Content-Length", "Host":
			// The body was reassembled from chunks (a stale length would
			// corrupt framing) and NewRequestWithContext already derives the
			// right Host from url - forwarding the caller's original values
			// for either would only confuse this local hop, exactly why
			// gateway.go's own local-forward path strips the equivalent
			// hop-specific headers before dialing.
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s did not answer: %w", url, err)
	}
	return resp, nil
}

// modelNameFromProxiedBody mirrors gateway.go's own probe/multipartModel
// logic exactly (JSON "model" field first, multipart form field second) -
// souslet has no other way to learn which deployed recipe a proxied request
// is for, since HTTPRequestHead carries no such field.
func modelNameFromProxiedBody(head *pb.HTTPRequestHead, body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	if probe.Model != "" {
		return strings.TrimSpace(probe.Model)
	}

	ct := head.GetHeaders()["Content-Type"]
	if !strings.HasPrefix(ct, "multipart/form-data") {
		return ""
	}
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	boundary := params["boundary"]
	if boundary == "" {
		return ""
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			return ""
		}
		if part.FormName() != "model" {
			_ = part.Close()
			continue
		}
		v, err := io.ReadAll(io.LimitReader(part, 1<<10))
		_ = part.Close()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(v))
	}
}
