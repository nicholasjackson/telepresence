package udpgrpc

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/datawire/dlib/dlog"
	rpc "github.com/telepresenceio/telepresence/rpc/v2/manager"
	"github.com/telepresenceio/telepresence/v2/pkg/connpool"
)

// The idleDuration controls how long a handler remains alive without reading or writing any messages
const idleDuration = time.Second

// The Handler takes care of dispatching messages between gRPC and UDP connections
type Handler struct {
	id        connpool.ConnID
	ctx       context.Context
	cancel    context.CancelFunc
	release   func()
	server    rpc.Manager_ConnTunnelServer
	incoming  chan *rpc.ConnMessage
	conn      *net.UDPConn
	idleTimer *time.Timer
}

// NewHandler creates a new handler that dispatches messages in both directions between the given gRPC server
// and the destination identified by the given connID.
//
// The handler remains active until it's been idle for idleDuration, at which time it will automatically close
// and call the release function it got from the connpool.Pool to ensure that it gets properly released.
func NewHandler(ctx context.Context, connID connpool.ConnID, server rpc.Manager_ConnTunnelServer, release func()) (connpool.Handler, error) {
	destAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", connID.Destination(), connID.DestinationPort()))
	if err != nil {
		return nil, fmt.Errorf("connection %s unable to resolve destination address: %v", connID, err)
	}
	conn, err := net.DialUDP("udp", nil, destAddr)
	if err != nil {
		return nil, fmt.Errorf("connection %s failed: %v", connID, err)
	}
	handler := &Handler{
		id:       connID,
		server:   server,
		release:  release,
		conn:     conn,
		incoming: make(chan *rpc.ConnMessage, 10),
	}

	// Set up the idle timer to close and release this handler when it's been idle for a while.
	handler.ctx, handler.cancel = context.WithCancel(ctx)

	handler.idleTimer = time.AfterFunc(idleDuration, func() {
		handler.release()
		handler.Close(handler.ctx)
	})
	go handler.readLoop()
	go handler.writeLoop()
	return handler, nil
}

func (h *Handler) HandleControl(_ context.Context, _ *connpool.ControlMessage) {
	// UDP handler doesn't do controls
}

// HandleMessage a package to the underlying UDP connection
func (h *Handler) HandleMessage(ctx context.Context, dg *rpc.ConnMessage) {
	select {
	case <-ctx.Done():
		return
	case h.incoming <- dg:
	}
}

// Close will close the underlying UDP connection
func (h *Handler) Close(_ context.Context) {
	_ = h.conn.Close()
}

func (h *Handler) readLoop() {
	b := make([]byte, 0x8000)
	for h.ctx.Err() == nil {
		n, err := h.conn.Read(b)
		if err != nil {
			h.idleTimer.Stop()
			return
		}
		// dlog.Debugf(ctx, "%s read TCP package of size %d", uh.id, n)
		if !h.idleTimer.Reset(idleDuration) {
			// Timer had already fired. Prevent that it fires again. We're done here.
			h.idleTimer.Stop()
			return
		}
		if n > 0 {
			dlog.Debugf(h.ctx, "-> CLI %s, len %d", h.id.ReplyString(), n)
			if err = h.server.Send(&rpc.ConnMessage{ConnId: []byte(h.id), Payload: b[:n]}); err != nil {
				return
			}
		}
	}
}

func (h *Handler) writeLoop() {
	for {
		select {
		case <-h.ctx.Done():
			return
		case dg := <-h.incoming:
			dlog.Debugf(h.ctx, "<- CLI %s, len %d", h.id, len(dg.Payload))
			if !h.idleTimer.Reset(idleDuration) {
				// Timer had already fired. Prevent that it fires again. We're done here.
				h.idleTimer.Stop()
				return
			}

			pn := len(dg.Payload)
			for n := 0; n < pn; {
				wn, err := h.conn.Write(dg.Payload[n:])
				if err != nil && h.ctx.Err() == nil {
					dlog.Errorf(h.ctx, "%s failed to write TCP: %v", h.id, err)
				}
				n += wn
			}
		}
	}
}
