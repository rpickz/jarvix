package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/quiesce"
	"github.com/rpickz/jarvix/internal/session"
)

// Handler dispatches one RPC method. Returning an *Error preserves its code;
// any other error becomes an internal error.
type Handler func(params json.RawMessage) (any, error)

// Server accepts clients on a Unix socket, dispatches requests, and pushes
// bus events to every connected client.
type Server struct {
	socketPath string
	handlers   map[string]Handler
	bus        *session.Bus
	log        *slog.Logger

	// serving tracks the per-connection goroutines. Serve returning only means
	// the listener stopped accepting; the connections it already handed off
	// are still dispatching requests and pushing events. Drain lets the daemon
	// wait for them, so "the daemon has stopped" is not something a client can
	// still be mid-request through.
	serving quiesce.Group

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
}

// NewServer creates a server. bus may be nil for tests that only exercise
// request handling.
func NewServer(socketPath string, bus *session.Bus, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		socketPath: socketPath,
		handlers:   make(map[string]Handler),
		bus:        bus,
		log:        logger,
		conns:      make(map[net.Conn]struct{}),
	}
}

// Handle registers a method.
func (s *Server) Handle(method string, h Handler) { s.handlers[method] = h }

// Listen binds the socket. A live socket from another daemon instance is an
// error; a stale one is removed.
func (s *Server) Listen() error {
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	if _, err := os.Stat(s.socketPath); err == nil {
		conn, err := net.DialTimeout("unix", s.socketPath, time.Second)
		if err == nil {
			_ = conn.Close()
			return fmt.Errorf("another jarvixd is already listening on %s", s.socketPath)
		}
		if err := os.Remove(s.socketPath); err != nil {
			return fmt.Errorf("remove stale socket: %w", err)
		}
		s.log.Info("removed stale socket", "component", "ipc", "path", s.socketPath)
	}
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.socketPath, err)
	}
	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("restrict socket permissions: %w", err)
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()
	return nil
}

// Serve accepts connections until ctx is cancelled or Close is called.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	ln := s.listener
	s.mu.Unlock()
	if ln == nil {
		return errors.New("Serve called before Listen")
	}
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.serving.Go(func() { s.serveConn(ctx, conn) })
	}
}

// Drain waits for every connection goroutine to exit, or until ctx is done.
// Close must have been called first — it is closing the sockets that ends the
// per-connection read loops — which Serve does for a cancelled context.
func (s *Server) Drain(ctx context.Context) error { return s.serving.Wait(ctx) }

// InFlight reports how many connections are still being served, for the
// shutdown log when a drain gives up.
func (s *Server) InFlight() int { return s.serving.InFlight() }

// Close stops listening and disconnects all clients.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		_ = s.listener.Close()
		s.listener = nil
	}
	for conn := range s.conns {
		_ = conn.Close()
	}
	_ = os.Remove(s.socketPath)
}

func (s *Server) dropConn(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
	_ = conn.Close()
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	defer s.dropConn(conn)

	// Writes come from two directions — responses and event pushes — so they
	// share one mutex to keep frames intact.
	var writeMu sync.Mutex
	writeJSON := func(v any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err = conn.Write(append(data, '\n'))
		return err
	}

	// Forward bus events as notifications for this connection's lifetime.
	if s.bus != nil {
		events, unsubscribe := s.bus.Subscribe()
		defer unsubscribe()
		forwardCtx, stopForward := context.WithCancel(ctx)
		defer stopForward()
		go func() {
			for {
				select {
				case ev, ok := <-events:
					if !ok {
						return
					}
					if err := writeJSON(Notification{JSONRPC: "2.0", Method: ev.Type, Params: ev.Data}); err != nil {
						return
					}
				case <-forwardCtx.Done():
					return
				}
			}
		}()
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = writeJSON(Response{JSONRPC: "2.0", Error: Errorf(CodeParseError, "malformed request: %v", err)})
			continue
		}
		resp := s.dispatch(&req)
		if req.ID == nil {
			continue // notification: no response
		}
		if err := writeJSON(resp); err != nil {
			return
		}
	}
}

func (s *Server) dispatch(req *Request) Response {
	resp := Response{JSONRPC: "2.0", ID: req.ID}
	if req.JSONRPC != "2.0" {
		resp.Error = Errorf(CodeInvalidRequest, "jsonrpc must be \"2.0\"")
		return resp
	}
	handler, ok := s.handlers[req.Method]
	if !ok {
		resp.Error = Errorf(CodeMethodNotFound, "unknown method %q", req.Method)
		return resp
	}
	result, err := handler(req.Params)
	if err != nil {
		var rpcErr *Error
		if errors.As(err, &rpcErr) {
			resp.Error = rpcErr
		} else {
			resp.Error = Errorf(CodeInternalError, "%v", err)
		}
		return resp
	}
	if result == nil {
		result = struct{}{}
	}
	resp.Result = result
	return resp
}
