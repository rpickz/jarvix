package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/session"
)

// Client is the CLI/test side of the protocol. It multiplexes synchronous
// calls and asynchronous event notifications over one connection.
type Client struct {
	conn net.Conn

	mu      sync.Mutex
	nextID  int
	pending map[int]chan Response
	events  chan session.Event
	readErr error
	closed  chan struct{}
}

// Dial connects to the daemon socket.
func Dial(socketPath string) (*Client, error) {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("jarvixd is not reachable at %s — is it running? (systemctl --user start jarvixd): %w", socketPath, err)
	}
	c := &Client{
		conn:    conn,
		pending: make(map[int]chan Response),
		events:  make(chan session.Event, 256),
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Events returns the stream of daemon events received on this connection.
// The channel closes when the connection drops.
func (c *Client) Events() <-chan session.Event { return c.events }

// Close disconnects.
func (c *Client) Close() error { return c.conn.Close() }

// Err returns the terminal read error after the connection closed, if any.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

// Call performs one RPC and decodes the result into out (which may be nil).
func (c *Client) Call(method string, params any, out any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan Response, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	rawID := json.RawMessage(fmt.Sprintf("%d", id))
	req := Request{JSONRPC: "2.0", ID: &rawID, Method: method}
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = data
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := c.conn.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("send %s: %w", method, err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil {
			raw, err := json.Marshal(resp.Result)
			if err != nil {
				return err
			}
			return json.Unmarshal(raw, out)
		}
		return nil
	case <-c.closed:
		return fmt.Errorf("connection to jarvixd lost during %s", method)
	case <-time.After(30 * time.Second):
		return fmt.Errorf("%s timed out", method)
	}
}

func (c *Client) readLoop() {
	defer func() {
		c.mu.Lock()
		close(c.closed)
		close(c.events)
		c.mu.Unlock()
	}()
	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		// Distinguish responses (have an id) from notifications (have a method).
		var frame struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
			Params map[string]any   `json:"params"`
			Result json.RawMessage  `json:"result"`
			Error  *Error           `json:"error"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			continue
		}
		if frame.Method != "" {
			select {
			case c.events <- session.Event{Type: frame.Method, Data: frame.Params}:
			default: // drop rather than stall the read loop
			}
			continue
		}
		if frame.ID == nil {
			continue
		}
		var id int
		if err := json.Unmarshal(*frame.ID, &id); err != nil {
			continue
		}
		var result any
		if len(frame.Result) > 0 {
			_ = json.Unmarshal(frame.Result, &result)
		}
		c.mu.Lock()
		ch, ok := c.pending[id]
		c.mu.Unlock()
		if ok {
			ch <- Response{JSONRPC: "2.0", ID: frame.ID, Result: result, Error: frame.Error}
		}
	}
	c.mu.Lock()
	c.readErr = scanner.Err()
	c.mu.Unlock()
}
