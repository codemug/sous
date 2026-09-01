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
	pending map[string]chan *pb.Envelope // stream_id -> waiter

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

	nc := &nodeConn{send: make(chan *pb.Envelope, 32), pending: make(map[string]chan *pb.Envelope), done: make(chan struct{})}
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
			nc.mu.Lock()
			waiter, ok := nc.pending[env.StreamId]
			if ok {
				delete(nc.pending, env.StreamId)
			}
			nc.mu.Unlock()
			if ok {
				waiter <- env
			}
		}
	}()
	return <-errCh
}

// Send delivers env to nodeID's live connection and blocks until the
// correlated reply arrives. Returns an error immediately if nodeID has no
// live connection - callers must not queue against a disconnected node
// (the design's explicit "fail fast, don't buffer" reconciliation choice).
func (s *Server) Send(nodeID string, env *pb.Envelope) (*pb.Envelope, error) {
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
	case <-context.Background().Done():
		return nil, context.Canceled
	}
}
