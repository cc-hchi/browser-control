package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/cc-hchi/browser-control/internal/protocol"
)

type Handler func(context.Context, *Peer, protocol.Request) (any, *protocol.RPCError)

type callResult struct {
	response protocol.Response
	err      error
}

type Peer struct {
	conn      net.Conn
	decoder   *json.Decoder
	handler   Handler
	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan callResult
	nextID    atomic.Uint64
	closed    chan struct{}
	closeOnce sync.Once
	serveErr  error
	serveMu   sync.Mutex
}

func NewPeer(conn net.Conn, handler Handler) *Peer {
	return &Peer{
		conn:    conn,
		decoder: json.NewDecoder(conn),
		handler: handler,
		pending: make(map[string]chan callResult),
		closed:  make(chan struct{}),
	}
}

func (p *Peer) Serve(ctx context.Context) error {
	defer p.Close()
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Close()
		case <-p.closed:
		}
	}()
	for {
		var raw json.RawMessage
		if err := p.decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			p.setServeErr(err)
			_ = p.writeResponse(protocol.Response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: protocol.NewError(protocol.CodeParseError, "INVALID_REQUEST", "invalid JSON", false, nil)})
			return err
		}
		var header struct {
			JSONRPC string             `json:"jsonrpc"`
			ID      json.RawMessage    `json:"id"`
			Method  string             `json:"method"`
			Result  json.RawMessage    `json:"result"`
			Error   *protocol.RPCError `json:"error"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			_ = p.writeResponse(protocol.Response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: protocol.NewError(protocol.CodeInvalidRequest, "INVALID_REQUEST", "invalid JSON-RPC envelope", false, nil)})
			continue
		}
		if header.Method != "" {
			var req protocol.Request
			if err := json.Unmarshal(raw, &req); err != nil || protocol.ValidateRequest(req) != nil {
				if len(header.ID) > 0 {
					_ = p.writeResponse(protocol.Response{JSONRPC: "2.0", ID: header.ID, Error: protocol.NewError(protocol.CodeInvalidRequest, "INVALID_REQUEST", "invalid JSON-RPC request", false, nil)})
				}
				continue
			}
			go p.handleRequest(ctx, req)
			continue
		}
		if len(header.ID) == 0 {
			continue
		}
		response := protocol.Response{JSONRPC: header.JSONRPC, ID: header.ID, Result: header.Result, Error: header.Error}
		key := protocol.IDKey(header.ID)
		p.pendingMu.Lock()
		waiter := p.pending[key]
		if waiter != nil {
			delete(p.pending, key)
		}
		p.pendingMu.Unlock()
		if waiter != nil {
			waiter <- callResult{response: response}
		}
	}
}

func (p *Peer) handleRequest(ctx context.Context, req protocol.Request) {
	if p.handler == nil {
		if !protocol.IsNotification(req) {
			_ = p.writeResponse(protocol.Response{JSONRPC: "2.0", ID: req.ID, Error: protocol.NewError(protocol.CodeMethodNotFound, "METHOD_NOT_FOUND", "peer does not accept requests", false, map[string]any{"method": req.Method})})
		}
		return
	}
	requestCtx, cancel := context.WithCancel(ctx)
	requestDone := make(chan struct{})
	go func() {
		select {
		case <-p.closed:
			cancel()
		case <-requestDone:
		}
	}()
	result, rpcErr := p.handler(requestCtx, p, req)
	close(requestDone)
	cancel()
	if protocol.IsNotification(req) {
		return
	}
	response := protocol.Response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr}
	if rpcErr == nil {
		response.Result = protocol.MarshalResult(result)
	}
	_ = p.writeResponse(response)
}

func (p *Peer) Call(ctx context.Context, method string, params any) (json.RawMessage, *protocol.RPCError, error) {
	id := p.nextID.Add(1)
	idRaw := protocol.MarshalResult(fmt.Sprintf("peer-%d", id))
	paramsRaw := protocol.MarshalResult(params)
	req := protocol.Request{JSONRPC: "2.0", ID: idRaw, Method: method, Params: paramsRaw}
	waiter := make(chan callResult, 1)
	key := protocol.IDKey(idRaw)
	p.pendingMu.Lock()
	p.pending[key] = waiter
	p.pendingMu.Unlock()
	if err := p.writeRequest(req); err != nil {
		p.pendingMu.Lock()
		delete(p.pending, key)
		p.pendingMu.Unlock()
		return nil, nil, err
	}
	select {
	case <-ctx.Done():
		p.pendingMu.Lock()
		delete(p.pending, key)
		p.pendingMu.Unlock()
		return nil, nil, ctx.Err()
	case <-p.closed:
		return nil, nil, p.Err()
	case result := <-waiter:
		if result.err != nil {
			return nil, nil, result.err
		}
		return result.response.Result, result.response.Error, nil
	}
}

func (p *Peer) Notify(method string, params any) error {
	return p.writeRequest(protocol.Request{JSONRPC: "2.0", Method: method, Params: protocol.MarshalResult(params)})
}

func (p *Peer) writeRequest(req protocol.Request) error {
	return p.writeJSON(req)
}

func (p *Peer) writeResponse(response protocol.Response) error {
	return p.writeJSON(response)
}

func (p *Peer) writeJSON(value any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	select {
	case <-p.closed:
		return net.ErrClosed
	default:
	}
	encoder := json.NewEncoder(p.conn)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		p.setServeErr(err)
		_ = p.Close()
		return err
	}
	return nil
}

func (p *Peer) Close() error {
	var err error
	p.closeOnce.Do(func() {
		err = p.conn.Close()
		close(p.closed)
		p.pendingMu.Lock()
		for key, waiter := range p.pending {
			delete(p.pending, key)
			waiter <- callResult{err: net.ErrClosed}
		}
		p.pendingMu.Unlock()
	})
	return err
}

func (p *Peer) Done() <-chan struct{} { return p.closed }

func (p *Peer) Err() error {
	p.serveMu.Lock()
	defer p.serveMu.Unlock()
	if p.serveErr != nil {
		return p.serveErr
	}
	return net.ErrClosed
}

func (p *Peer) setServeErr(err error) {
	if err == nil {
		return
	}
	p.serveMu.Lock()
	if p.serveErr == nil {
		p.serveErr = err
	}
	p.serveMu.Unlock()
}

func Dial(ctx context.Context, network, address string, handler Handler) (*Peer, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	peer := NewPeer(conn, handler)
	go func() { _ = peer.Serve(context.Background()) }()
	return peer, nil
}
