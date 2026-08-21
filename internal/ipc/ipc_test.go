package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/session"
)

func startServer(t *testing.T, bus *session.Bus) (*Server, string) {
	t.Helper()
	// Socket paths must stay under sun_path limits; keep them short.
	sock := filepath.Join(t.TempDir(), "j.sock")
	srv := NewServer(sock, bus, nil)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(srv.Close)
	return srv, sock
}

func TestCallRoundTrip(t *testing.T) {
	srv, sock := startServer(t, nil)
	srv.Handle("echo", func(params json.RawMessage) (any, error) {
		var in map[string]string
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, Errorf(CodeInvalidParams, "bad params")
		}
		return map[string]string{"echo": in["text"]}, nil
	})

	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	var out map[string]string
	if err := c.Call("echo", map[string]string{"text": "hello"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["echo"] != "hello" {
		t.Errorf("out = %v", out)
	}
}

func TestUnknownMethod(t *testing.T) {
	_, sock := startServer(t, nil)
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	err = c.Call("nope", nil, nil)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
		t.Errorf("err = %v", err)
	}
}

func TestHandlerErrorCodePreserved(t *testing.T) {
	srv, sock := startServer(t, nil)
	srv.Handle("fail", func(json.RawMessage) (any, error) {
		return nil, Errorf(CodeSessionError, "no active session")
	})
	c, _ := Dial(sock)
	defer func() { _ = c.Close() }()

	err := c.Call("fail", nil, nil)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeSessionError {
		t.Errorf("err = %v", err)
	}
}

func TestEventsPushedToClients(t *testing.T) {
	bus := session.NewBus(nil)
	_, sock := startServer(t, bus)

	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	// Give the connection's event forwarder a moment to subscribe.
	time.Sleep(50 * time.Millisecond)

	bus.Publish(session.Event{Type: "state.changed", Data: map[string]any{"state": "listening"}})

	select {
	case ev := <-c.Events():
		if ev.Type != "state.changed" || ev.Data["state"] != "listening" {
			t.Errorf("event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event received")
	}
}

func TestEventsReachMultipleClients(t *testing.T) {
	bus := session.NewBus(nil)
	_, sock := startServer(t, bus)
	var clients []*Client
	for i := 0; i < 3; i++ {
		c, err := Dial(sock)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		clients = append(clients, c)
	}
	time.Sleep(50 * time.Millisecond)
	bus.Publish(session.Event{Type: "tts.started"})
	for i, c := range clients {
		select {
		case ev := <-c.Events():
			if ev.Type != "tts.started" {
				t.Errorf("client %d: %+v", i, ev)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("client %d received nothing", i)
		}
	}
}

func TestMalformedJSONGetsParseError(t *testing.T) {
	_, sock := startServer(t, nil)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("this is not json\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		t.Fatalf("response not JSON: %s", buf[:n])
	}
	if resp.Error == nil || resp.Error.Code != CodeParseError {
		t.Errorf("resp = %+v", resp)
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "j.sock")

	// First server creates the socket, then dies without cleanup.
	first := NewServer(sock, nil, nil)
	if err := first.Listen(); err != nil {
		t.Fatal(err)
	}
	first.mu.Lock()
	_ = first.listener.Close() // simulate crash: listener gone, socket file left behind
	first.listener = nil
	first.mu.Unlock()

	second := NewServer(sock, nil, nil)
	if err := second.Listen(); err != nil {
		t.Fatalf("second Listen over stale socket: %v", err)
	}
	second.Close()
}

func TestLiveSocketIsNotStolen(t *testing.T) {
	_, sock := startServer(t, nil)
	second := NewServer(sock, nil, nil)
	if err := second.Listen(); err == nil {
		second.Close()
		t.Fatal("second daemon must refuse to bind a live socket")
	}
}

func TestNotificationRequestGetsNoResponse(t *testing.T) {
	srv, sock := startServer(t, nil)
	called := make(chan struct{}, 1)
	srv.Handle("fire", func(json.RawMessage) (any, error) {
		called <- struct{}{}
		return nil, nil
	})
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","method":"fire"}` + "\n"))
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("notification not dispatched")
	}
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 64)
	if n, _ := conn.Read(buf); n > 0 {
		t.Errorf("unexpected response to notification: %s", buf[:n])
	}
}
