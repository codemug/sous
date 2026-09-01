package grpcclient

import (
	"context"
	"log"
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
		if err := c.connectOnce(ctx); err != nil {
			log.Printf("souslet: connection to %s lost: %v (retrying in %s)", c.Addr, err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

func (c *Client) connectOnce(ctx context.Context) error {
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

	snap := c.Handlers.Snapshot(ctx, c.NodeID, c.PoolGiB, c.ReserveGiB)
	sendMu.Lock()
	err = stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: snap}})
	sendMu.Unlock()
	if err != nil {
		return err
	}

	for {
		env, err := stream.Recv()
		if err != nil {
			return err
		}
		go c.dispatch(ctx, stream, &sendMu, env)
	}
}

func (c *Client) dispatch(ctx context.Context, stream pb.Souslet_ConnectClient, sendMu *sync.Mutex, env *pb.Envelope) {
	var reply *pb.Envelope
	switch {
	case env.GetDeploy() != nil:
		reply = &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_DeployResult{
			DeployResult: c.Handlers.HandleDeploy(ctx, env.GetDeploy()),
		}}
	case env.GetUndeploy() != nil:
		reply = &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_UndeployResult{
			UndeployResult: c.Handlers.HandleUndeploy(ctx, env.GetUndeploy()),
		}}
	case env.GetFetch() != nil:
		reply = &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_FetchProgress{
			FetchProgress: c.Handlers.HandleFetch(ctx, env.GetFetch()),
		}}
	case env.GetDeleteWeights() != nil:
		reply = &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_DeleteWeightsResult{
			DeleteWeightsResult: c.Handlers.HandleDeleteWeights(ctx, env.GetDeleteWeights()),
		}}
	default:
		return // HTTP proxy frames are handled by Task 9's extension of this switch, not here
	}
	sendMu.Lock()
	err := stream.Send(reply)
	sendMu.Unlock()
	if err != nil {
		log.Printf("souslet: failed to send reply for stream %s: %v", env.StreamId, err)
	}
}
