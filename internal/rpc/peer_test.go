package rpc

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/cc-hchi/browser-control/internal/protocol"
)

func TestClosingPeerCancelsActiveRequestContext(t *testing.T) {
	client, serverConn := net.Pipe()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	peer := NewPeer(serverConn, func(ctx context.Context, _ *Peer, _ protocol.Request) (any, *protocol.RPCError) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, protocol.NewError(protocol.CodeCancelled, "CANCELLED", "closed", true, nil)
	})
	go func() { _ = peer.Serve(context.Background()) }()
	request := protocol.Request{JSONRPC: "2.0", ID: json.RawMessage(`"request"`), Method: "wait", Params: json.RawMessage(`{}`)}
	if err := json.NewEncoder(client).Encode(request); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	_ = client.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("closing peer did not cancel request context")
	}
}
