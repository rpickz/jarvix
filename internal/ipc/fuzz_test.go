package ipc

import (
	"encoding/json"
	"testing"
)

// FuzzWireDecode feeds arbitrary bytes through the same decode-and-dispatch
// path serveConn applies to every socket line: any process of the same user
// can write to the socket, so malformed frames must produce a JSON-RPC error
// (or be dropped), never a panic — and every response must marshal back.
func FuzzWireDecode(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"status.get"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":"abc","method":"echo","params":{"text":"hi"}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","method":"notify.only","params":[1,2,3]}`))
	f.Add([]byte(`{"jsonrpc":"1.0","id":2,"method":"status.get"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":3,"method":"no.such.method"}`))
	f.Add([]byte(`{"id":4}`))
	f.Add([]byte(`{broken`))
	f.Add([]byte(``))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":{"nested":"id"},"method":"echo"}`))
	f.Fuzz(func(t *testing.T, line []byte) {
		srv := NewServer("/unused", nil, nil)
		srv.Handle("echo", func(params json.RawMessage) (any, error) {
			return map[string]any{"echo": string(params)}, nil
		})
		srv.Handle("status.get", func(json.RawMessage) (any, error) {
			return map[string]any{"state": "idle"}, nil
		})
		srv.Handle("boom", func(json.RawMessage) (any, error) {
			return nil, Errorf(CodeSessionError, "no active session")
		})

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			return // serveConn answers with a parse error; nothing to dispatch
		}
		resp := srv.dispatch(&req)
		if resp.JSONRPC != "2.0" {
			t.Fatalf("response version = %q", resp.JSONRPC)
		}
		if resp.Error == nil && resp.Result == nil {
			t.Fatalf("response carries neither result nor error: %+v", resp)
		}
		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("response does not marshal: %v", err)
		}
		var roundTrip Response
		if err := json.Unmarshal(data, &roundTrip); err != nil {
			t.Fatalf("response does not round-trip: %v", err)
		}
	})
}
