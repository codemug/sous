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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type nodeConn struct {
	// send carries PROXY traffic only (request heads and body chunks).
	// ProxyStream.Send/SendChunk block on it when it is full, which is
	// deliberate: that is the backpressure that keeps a 32MB upload from
	// buffering without bound inside sous-api.
	send chan *pb.Envelope

	// controlSend carries COMMANDS only (deploy/undeploy/fetch/
	// delete-weights) and exists because sharing one channel with proxy
	// traffic made the two interfere. sendChunkedProxyBody emits 4096-byte
	// frames, so a single real upload at this gateway's own 32MB
	// maxRequestBytes limit is over 8000 envelopes - enough to keep a shared
	// 32-deep buffer saturated for the whole transfer. Send's enqueue fails
	// fast rather than waiting (see its doc), so every deploy/undeploy/
	// fetch/weight-delete issued during that window failed spuriously with a
	// "send queue is full" error that had nothing to do with the command.
	// Two channels drained by the same write loop removes the interference
	// entirely rather than merely making it less likely: a control command's
	// admission no longer depends on how much proxy body is in flight.
	controlSend chan *pb.Envelope

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

// NodeAuthority is the slice of *mtls.CA this package needs to decide
// whether a connecting node is allowed in at all: is this node ID one the
// operator actually registered (and has not since revoked)? Narrow on
// purpose, and an interface rather than *mtls.CA directly, so a test can
// supply its own registration set without standing up certificate
// machinery it isn't testing.
type NodeAuthority interface {
	IsKnown(nodeID string) bool
}

type Server struct {
	pb.UnimplementedSousletServer
	cat *nodecatalog.Catalog

	// ca decides node identity. Connect matches the node ID a client claims
	// in its first snapshot against the CommonName on its VERIFIED peer
	// certificate and against ca's registration set - without it, a node's
	// identity is whatever it says it is, revocation is a no-op, and any
	// holder of any valid cert can evict or impersonate any other node.
	//
	// nil disables that enforcement, for a Server that is not fronting a
	// real mTLS listener (this package's own bufconn tests, which cannot
	// present a peer certificate at all). cmd/sous-api - the only
	// production caller - always passes the real CA.
	ca NodeAuthority

	mu    sync.RWMutex
	conns map[string]*nodeConn // node_id -> its live connection
}

// New builds the server side of the Souslet service. ca may be nil only for
// a Server not fronting an mTLS listener - see the ca field's doc comment.
func New(cat *nodecatalog.Catalog, ca NodeAuthority) *Server {
	return &Server{cat: cat, ca: ca, conns: make(map[string]*nodeConn)}
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
// Connect registers a new connection in s.conns BEFORE its handshake ever
// makes the catalog report the node as connected (see the ordering comment
// in Connect's body), so by the time Catalog().Node(nodeID).Connected is
// true, Connected(nodeID) is already true too - this is here mainly so
// tests (and any other caller that wants the more direct question, without
// going through the catalog) don't have to reach into unexported state to
// ask it.
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
	// The first message on a new connection must be a snapshot - that's how
	// this node announces which node it claims to be. That claim is then
	// checked against the connection's VERIFIED peer certificate by
	// authorize below; it is not taken on trust.
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("read initial snapshot: %w", err)
	}
	snap := first.GetSnapshot()
	if snap == nil {
		return fmt.Errorf("first message on Connect must be a NodeSnapshot")
	}
	nodeID := snap.NodeId
	if err := s.authorize(stream.Context(), nodeID); err != nil {
		return err
	}

	nc := &nodeConn{
		send:         make(chan *pb.Envelope, 32),
		controlSend:  make(chan *pb.Envelope, 32),
		pending:      make(map[string]chan *pb.Envelope),
		proxyStreams: make(map[string]chan *pb.Envelope),
		done:         make(chan struct{}),
	}
	// Register the connection BEFORE the catalog ever reflects this node as
	// connected - not after. s.conns is what Send/OpenProxyStream actually
	// check; s.cat is what callers like deployToNode and the gateway's
	// proxyOverGRPC read first to decide whether it's even worth trying a
	// live call. If the catalog said "connected" while s.conns was still
	// empty, a caller reading the catalog and immediately calling Send could
	// observe a spurious "not connected" - a real, reachable race (not just a
	// test-timing artifact), since both of those callers do exactly this
	// read-catalog-then-Send sequence. Registering conns first closes that
	// window: nothing can observe this node as connected in the catalog
	// before Send/OpenProxyStream would actually find it.
	s.mu.Lock()
	s.conns[nodeID] = nc
	s.mu.Unlock()
	s.cat.ReplaceSnapshot(nodeID, snap)

	defer func() {
		// IDENTITY-CHECKED TEARDOWN. Only unregister if the map still points
		// at THIS connection. A node reboot or a network partition can leave
		// this goroutine's stream.Recv blocked long after the node has
		// reconnected and registered a NEW nodeConn under the same node ID;
		// when the old stream finally errors out, an unconditional
		// delete/MarkDisconnected here would tear down that live, working
		// connection - leaving the node shown as disconnected and
		// unreachable via Send/OpenProxyStream until a souslet or sous-api
		// restart, despite nothing actually being wrong with it.
		s.mu.Lock()
		stale := true
		if cur, ok := s.conns[nodeID]; ok && cur == nc {
			delete(s.conns, nodeID)
			stale = false
		}
		s.mu.Unlock()
		// Unblock the write loop (and any Send call racing this teardown)
		// without ever closing nc.send itself - see the done field's doc.
		// Always done, superseded or not: this connection's own goroutines
		// and callers must still be released.
		nc.closeOnce.Do(func() { close(nc.done) })
		if !stale {
			s.cat.MarkDisconnected(nodeID)
		}
	}()

	errCh := make(chan error, 2)
	go func() {
		for {
			// Control commands are drained BEFORE proxy traffic on every
			// iteration, not merely alongside it: a plain three-way select
			// picks uniformly among ready cases, so a deploy could still
			// queue behind thousands of already-buffered body frames. This
			// non-blocking pre-check gives commands strict priority while
			// still costing nothing when no command is waiting.
			select {
			case env := <-nc.controlSend:
				if err := stream.Send(env); err != nil {
					errCh <- err
					return
				}
				continue
			default:
			}
			select {
			case env := <-nc.controlSend:
				if err := stream.Send(env); err != nil {
					errCh <- err
					return
				}
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

// authorize decides whether a connection claiming to be claimedID is
// allowed to be that node.
//
// The claim arrives in the client's OWN first message, so on its own it is
// worth nothing: mTLS proves only that the peer holds SOME certificate this
// CA signed, not which node it is. Without this check any node's cert could
// claim any other node's ID - evicting the real node from s.conns and
// receiving its deploys and proxied inference - and CA.Revoke would be a
// pure no-op, leaving a decommissioned node full control-plane access
// forever. Both are exactly what the verified peer certificate's CommonName
// (which IssueNodeCert sets to the node ID) and the CA's registration set
// are for.
func (s *Server) authorize(ctx context.Context, claimedID string) error {
	if s.ca == nil {
		return nil // no authority configured - see the ca field's doc comment
	}
	if claimedID == "" {
		return status.Error(codes.InvalidArgument, "the initial NodeSnapshot named no node_id")
	}
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return status.Error(codes.Unauthenticated, "connection carries no peer authentication information")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return status.Error(codes.Unauthenticated, "connection is not mTLS; node identity cannot be verified")
	}
	chains := tlsInfo.State.VerifiedChains
	if len(chains) == 0 || len(chains[0]) == 0 {
		return status.Error(codes.Unauthenticated, "connection presented no verified client certificate")
	}
	cn := chains[0][0].Subject.CommonName
	if cn != claimedID {
		return status.Errorf(codes.PermissionDenied,
			"certificate is issued for node %q but the connection claims to be %q", cn, claimedID)
	}
	if !s.ca.IsKnown(cn) {
		return status.Errorf(codes.PermissionDenied,
			"node %q is not registered with this control plane (revoked, or registered after this process started - see `sous-api node add`)", cn)
	}
	return nil
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

	// controlSend, NOT send: a command must not queue behind (or be refused
	// because of) proxy body frames - see nodeConn.controlSend's doc.
	select {
	case nc.controlSend <- env:
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
		// Still fail fast rather than wait, but now this means what it
		// says: 32 control commands are genuinely outstanding to this one
		// node, not "somebody is uploading a large audio file".
		nc.mu.Lock()
		delete(nc.pending, env.StreamId)
		nc.mu.Unlock()
		return nil, fmt.Errorf("node %q's command queue is full", nodeID)
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
