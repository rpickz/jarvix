// Package ai defines the provider-independent assistant interface.
//
// A Provider streams its response as a channel of Events. The channel is
// always closed when the stream ends, after a final Done or Error event.
// Cancellation happens through the context passed to Chat.
package ai

import "context"

// Role identifies the author of a message.
type Role string

// Message roles.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn of conversation history.
type Message struct {
	Role    Role
	Content string
}

// ChatRequest describes one assistant invocation.
type ChatRequest struct {
	Model       string
	Messages    []Message
	MaxTokens   int
	Temperature float64
}

// EventType classifies stream events.
type EventType string

// Stream event types.
const (
	EventDelta EventType = "delta" // Content carries a text fragment
	EventDone  EventType = "done"  // stream completed normally
	EventError EventType = "error" // Err carries the failure; stream ends
)

// Event is one element of a streamed response.
type Event struct {
	Type    EventType
	Content string
	Err     error
}

// Provider is a streaming assistant backend.
//
// Chat returns an error for failures detected before streaming begins
// (bad configuration, connection refused). Failures mid-stream arrive as an
// EventError on the channel. Implementations must close the channel when the
// stream ends for any reason, and must stop promptly when ctx is cancelled.
type Provider interface {
	Name() string
	Chat(ctx context.Context, req ChatRequest) (<-chan Event, error)
}
