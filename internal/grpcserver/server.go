// Package grpcserver implements the API side of the Souslet gRPC service:
// accepts each node's single long-lived Connect stream, feeds NodeSnapshot
// messages into nodecatalog, and lets the rest of sous-api (deploy/undeploy/
// plan handlers, the gateway proxy) send commands to a specific connected
// node and wait for the correlated reply.
package grpcserver

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/google/uuid"
)

type nodeConn struct {
	send    chan *pb.Envelope
	mu      sync.Mutex
	pending map[string]chan *pb.Envelope // stream_id -> waiter, single-shot (Send): deleted the moment its one reply arrives

	// proxyStreams is pending's sibling for OpenProxyStream: a stream_id
	// registered here expects MANY replies (one HTTPResponseHead, then N
	// HTTPResponseChunks), so - unlike pending - the read loop never
	// deletes an entry here just because a message arrived on it. Only
	// ProxyStream.Close removes it (on normal completion, on an Error
	// payload, or when the caller gives up), which is what keeps this map
	// from growing forever across a long-lived connection serving many
	// sequential proxied requests.
	proxyStreams map[string]chan *pb.Envelope

	// done is closed exactly once, by Connect's cleanup, when this
	// connection is torn down. It exists so the write-loop goroutine (and
	// Send, if it's racing teardown) has something to select on besides
	// nc.send - closing nc.send itself would be unsafe, since Send can be
	// writing to it concurrently from another goroutine and a send on a
	// closed channel panics.
	done      chan struct{}
	closeOnce sync.Once
}

type Server struct {
	pb.UnimplementedSousletServer
	cat *nodecatalog.Catalog

	mu    sync.RWMutex
	conns map[string]*nodeConn // node_id -> its live connection
}

func New(cat *nodecatalog.Catalog) *Server {
	return &Server{cat: cat, conns: make(map[string]*nodeConn)}
}

// Catalog returns the nodecatalog.Catalog this Server feeds NodeSnapshot
// updates into - the same instance the caller passed to New. It exists for
// callers (and test helpers) that need to read a node's last-known state
// (e.g. CachedWeightRepos) alongside sending it a command, without having to
// separately thread the same *nodecatalog.Catalog pointer through on their
// own.
func (s *Server) Catalog() *nodecatalog.Catalog {
	return s.cat
}

// Connected reports whether nodeID currently has a live connection
// registered - i.e. whether Send(ctx, nodeID, ...) would proceed past its
// initial "not connected" check right now, rather than fail immediately.
//
// This is a narrower and more precise question than the catalog's own
// Connected flag: Connect's handshake updates the catalog via
// s.cat.ReplaceSnapshot a few instructions BEFORE this connection's entry is
// added to s.conns (see Connect's body), so a caller polling
// Catalog().Node(nodeID).Connected alone can observe a false positive during
// that narrow window and then hit a spurious "not connected" from Send
// immediately after. Callers that need to wait for a fake/real node to be
// actually ready for Send (test helpers, mainly) should poll this instead.
func (s *Server) Connected(nodeID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.conns[nodeID]
	return ok
}

// Connect is the Souslet service's one RPC. It blocks for the life of the
// connection: read loop demuxes incoming Envelopes (snapshots update the
// catalog directly; everything else is routed to whichever Send call is
// waiting on that stream_id), write loop drains the outgoing channel Send
// publishes to.
func (s *Server) Connect(stream pb.Souslet_ConnectServer) error {
	// The first message on a new connection must be a snapshot - that's
	// how this node's ID is learned (see VerifiedNodeID note in Task 2;
	// full peer-cert-based identity wiring happens in Task 6's server
	// setup, this handler trusts NodeSnapshot.node_id for now since the
	// TLS layer already only accepted a cert signed by this CA).
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("read initial snapshot: %w", err)
	}
	snap := first.GetSnapshot()
	if snap == nil {
		return fmt.Errorf("first message on Connect must be a NodeSnapshot")
	}
	nodeID := snap.NodeId
	s.cat.ReplaceSnapshot(nodeID, snap)

	nc := &nodeConn{
		send:         make(chan *pb.Envelope, 32),
		pending:      make(map[string]chan *pb.Envelope),
		proxyStreams: make(map[string]chan *pb.Envelope),
		done:         make(chan struct{}),
	}
	s.mu.Lock()
	s.conns[nodeID] = nc
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, nodeID)
		s.mu.Unlock()
		// Unblock the write loop (and any Send call racing this teardown)
		// without ever closing nc.send itself - see the done field's doc.
		nc.closeOnce.Do(func() { close(nc.done) })
		s.cat.MarkDisconnected(nodeID)
	}()

	errCh := make(chan error, 2)
	go func() {
		for {
			select {
			case env := <-nc.send:
				if err := stream.Send(env); err != nil {
					errCh <- err
					return
				}
			case <-nc.done:
				return
			}
		}
	}()
	go func() {
		for {
			env, err := stream.Recv()
			if err == io.EOF {
				errCh <- nil
				return
			}
			if err != nil {
				errCh <- err
				return
			}
			if snap := env.GetSnapshot(); snap != nil {
				snap.NodeId = nodeID // defensive: trust the connection's identity, not a resend
				s.cat.ReplaceSnapshot(nodeID, snap)
				continue
			}
			// pending (Send's single-shot waiters) and proxyStreams
			// (OpenProxyStream's multi-message channels) are DIFFERENT maps
			// precisely because they have different reply cardinalities: a
			// stream_id lives in exactly one of the two, never both, so
			// checking pending first and falling through to proxyStreams is
			// unambiguous - not a priority order, just "whichever map
			// actually has this stream_id registered."
			nc.mu.Lock()
			waiter, ok := nc.pending[env.StreamId]
			if ok {
				delete(nc.pending, env.StreamId)
			}
			var proxyCh chan *pb.Envelope
			if !ok {
				proxyCh, ok = nc.proxyStreams[env.StreamId]
			}
			nc.mu.Unlock()
			if !ok {
				continue // no waiter registered for this stream_id - drop defensively
			}
			if waiter != nil {
				waiter <- env
				continue
			}
			// Unlike waiter (a fresh, unshared size-1 channel Send alone
			// holds), proxyCh is read concurrently by ProxyStream.RecvHead/
			// RecvChunk, which also select on nc.done - so this send must
			// too, or a proxy consumer that has already given up (node
			// disconnected, stream closed) could leave this goroutine
			// blocked here forever once proxyCh's buffer fills.
			select {
			case proxyCh <- env:
			case <-nc.done:
			}
		}
	}()
	return <-errCh
}

// Send delivers env to nodeID's live connection and blocks until the
// correlated reply arrives, the connection tears down, or ctx is done -
// whichever happens first. Returns an error immediately if nodeID has no
// live connection - callers must not queue against a disconnected node
// (the design's explicit "fail fast, don't buffer" reconciliation choice).
//
// ctx is the caller's to size: a plain deploy/undeploy/plan round trip is a
// simple in-memory dispatch-and-reply exchange and should use a short
// timeout, while a FetchCommand blocks on souslet actually downloading a
// model's weights and needs a long one. Send itself has no opinion on the
// value - see internal/httpapi/deploy_grpc.go's sendTimeout/fetchTimeout for
// the two bounds this codebase actually uses.
func (s *Server) Send(ctx context.Context, nodeID string, env *pb.Envelope) (*pb.Envelope, error) {
	s.mu.RLock()
	nc, ok := s.conns[nodeID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("node %q is not connected", nodeID)
	}
	env.StreamId = uuid.NewString()
	waiter := make(chan *pb.Envelope, 1)
	nc.mu.Lock()
	nc.pending[env.StreamId] = waiter
	nc.mu.Unlock()

	select {
	case nc.send <- env:
	case <-nc.done:
		// Lost the race with teardown: the write loop that would have
		// drained this envelope has already exited (or is exiting), so
		// nothing will ever consume it or fulfill the waiter. Fail fast
		// instead of leaving env stuck in the buffer and the caller
		// blocked on a reply that can never arrive.
		nc.mu.Lock()
		delete(nc.pending, env.StreamId)
		nc.mu.Unlock()
		return nil, fmt.Errorf("node %q disconnected while sending", nodeID)
	default:
		nc.mu.Lock()
		delete(nc.pending, env.StreamId)
		nc.mu.Unlock()
		return nil, fmt.Errorf("node %q's send queue is full", nodeID)
	}

	select {
	case reply := <-waiter:
		return reply, nil
	case <-nc.done:
		// The connection tore down while this call was waiting for its
		// reply - nothing will ever fulfill waiter now (the read loop
		// that would deliver it has stopped). Clean up the registration
		// so nc.pending doesn't hold a stale entry (and the waiter
		// channel) forever; without this, a node dropping mid-command
		// would leak both the calling goroutine and everything it
		// reaches through nc.
		nc.mu.Lock()
		delete(nc.pending, env.StreamId)
		nc.mu.Unlock()
		return nil, fmt.Errorf("node %q disconnected while waiting for reply", nodeID)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ProxyStream is one proxied HTTP request/response pair, tunnelled over its
// node's single Connect stream and correlated by its own stream_id - the
// gateway's replacement for dialing a model's container port directly. Open
// one with (*Server).OpenProxyStream, send exactly one HTTPRequestHead
// followed by one or more HTTPRequestChunks (Send/SendChunk), then read
// exactly one HTTPResponseHead followed by one or more HTTPResponseChunks
// (RecvHead/RecvChunk) until a chunk reports Eof.
//
// UNLIKE Send, which correlates exactly one reply per stream_id and deletes
// its waiter the instant that reply arrives, a ProxyStream's stream_id stays
// registered (in nc.proxyStreams, not nc.pending) across every message of
// the response - that's the entire reason it has its own registration map
// rather than reusing Send's.
type ProxyStream struct {
	nc       *nodeConn
	streamID string
	replies  chan *pb.Envelope

	closeOnce sync.Once
}

// OpenProxyStream registers a new proxy stream against nodeID's live
// connection. Like Send, it fails fast if nodeID has no live connection -
// the same "fail fast, don't buffer" choice, so a caller (the gateway)
// never queues an HTTP request against a node that cannot possibly answer
// it.
func (s *Server) OpenProxyStream(nodeID string) (*ProxyStream, error) {
	s.mu.RLock()
	nc, ok := s.conns[nodeID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("node %q is not connected", nodeID)
	}
	streamID := uuid.NewString()
	// Buffered so the read loop's routing select (above) doesn't have to
	// wait for RecvHead/RecvChunk to be actively reading before it can
	// deliver the next message - matches Send's waiter sizing philosophy,
	// just larger since a response can be many chunks, not one reply.
	replies := make(chan *pb.Envelope, 16)
	nc.mu.Lock()
	nc.proxyStreams[streamID] = replies
	nc.mu.Unlock()
	return &ProxyStream{nc: nc, streamID: streamID, replies: replies}, nil
}

// Send delivers the request head. Must be called before any SendChunk.
func (p *ProxyStream) Send(head *pb.HTTPRequestHead) error {
	env := &pb.Envelope{StreamId: p.streamID, Payload: &pb.Envelope_HttpReqHead{HttpReqHead: head}}
	select {
	case p.nc.send <- env:
		return nil
	case <-p.nc.done:
		p.Close()
		return fmt.Errorf("node disconnected while opening a proxy stream")
	}
}

// SendChunk delivers one piece of the request body. Call it at least once
// (with eof true on the last call, or immediately with eof true and no data
// for an empty body) after Send.
func (p *ProxyStream) SendChunk(data []byte, eof bool) error {
	env := &pb.Envelope{StreamId: p.streamID, Payload: &pb.Envelope_HttpReqChunk{
		HttpReqChunk: &pb.HTTPRequestChunk{Data: data, Eof: eof},
	}}
	select {
	case p.nc.send <- env:
		return nil
	case <-p.nc.done:
		p.Close()
		return fmt.Errorf("node disconnected while sending a proxied request body")
	}
}

// RecvHead blocks for the response head. It returns an error - never hangs
// forever - if the node's connection tears down first (nc.done firing) or
// if souslet reported a failure instead of a head (an Error payload, e.g.
// its local container was unreachable).
func (p *ProxyStream) RecvHead() (*pb.HTTPResponseHead, error) {
	select {
	case env, ok := <-p.replies:
		if !ok {
			return nil, io.EOF
		}
		if e := env.GetError(); e != nil {
			p.Close()
			return nil, fmt.Errorf("node reported an error: %s", e.Message)
		}
		head := env.GetHttpRespHead()
		if head == nil {
			p.Close()
			return nil, fmt.Errorf("expected an HTTPResponseHead, got a different message shape")
		}
		return head, nil
	case <-p.nc.done:
		p.Close()
		return nil, fmt.Errorf("node disconnected while waiting for the response head")
	}
}

// RecvChunk blocks for the next piece of the response body. Like RecvHead,
// it is bounded: a node that disconnects mid-response (or never sends a
// final Eof chunk at all) unblocks this call with an error rather than
// hanging the caller - and cleanly ("Close-s") this stream's registration
// either way, so a dead/disconnected node cannot leak nc.proxyStreams[id]
// forever.
func (p *ProxyStream) RecvChunk() (*pb.HTTPResponseChunk, error) {
	select {
	case env, ok := <-p.replies:
		if !ok {
			return nil, io.EOF
		}
		if e := env.GetError(); e != nil {
			p.Close()
			return nil, fmt.Errorf("node reported an error mid-stream: %s", e.Message)
		}
		chunk := env.GetHttpRespChunk()
		if chunk == nil {
			p.Close()
			return nil, fmt.Errorf("expected an HTTPResponseChunk, got a different message shape")
		}
		if chunk.Eof {
			p.Close()
		}
		return chunk, nil
	case <-p.nc.done:
		p.Close()
		return nil, fmt.Errorf("node disconnected while streaming the response")
	}
}

// Close unregisters this stream from its node's proxyStreams map. Safe to
// call more than once (RecvHead/RecvChunk already call it internally on any
// terminal condition) and safe to call from a caller's defer as a blanket
// safety net for any exit path that doesn't reach a terminal Recv - e.g. the
// original HTTP client hanging up before the response finished. Without
// this, a stream_id whose caller stopped reading early would sit in
// nc.proxyStreams forever, a slow leak across a long-lived connection
// serving many sequential proxied requests.
func (p *ProxyStream) Close() {
	p.closeOnce.Do(func() {
		p.nc.mu.Lock()
		delete(p.nc.proxyStreams, p.streamID)
		p.nc.mu.Unlock()
	})
}
