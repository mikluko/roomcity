package host

import (
	"context"

	acp "github.com/afrase/acp-go/unstable"
)

// agentClient is the ACP client the agent talks to. Everything the agent
// sends passes the tap and reaches the server as a frame, so the client
// itself does only what needs an answer: it holds a permission request until
// the server's PermissionAnswer comes.
type agentClient struct {
	acp.UnimplementedClient
	h *host
}

// SessionUpdate takes a notification and does nothing with it: the frame is
// the report.
func (*agentClient) SessionUpdate(context.Context, *acp.SessionNotification) error { return nil }

// RequestPermission waits for the PermissionAnswer that names this request by
// its JSON-RPC id. An answer with no option, or the end of the connection,
// cancels it.
func (c *agentClient) RequestPermission(ctx context.Context, _ *acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
	raw, ok := acp.RequestIDFromContext(ctx)
	if !ok {
		return &acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: true}}, nil
	}
	id := string(raw)
	ch := make(chan string, 1)
	c.h.mu.Lock()
	c.h.pending[id] = ch
	c.h.mu.Unlock()
	defer func() {
		c.h.mu.Lock()
		delete(c.h.pending, id)
		c.h.mu.Unlock()
	}()

	select {
	case option := <-ch:
		if option == "" {
			return &acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: true}}, nil
		}
		return &acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.SelectedPermissionOutcome{OptionID: acp.PermissionOptionID(option)}}}, nil
	case <-ctx.Done():
		return &acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: true}}, nil
	}
}
