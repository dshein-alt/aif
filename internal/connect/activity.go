package connect

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"time"
)

type activityReporter interface {
	trackTurn(context.Context) func()
}

// trackTurn reports a running harness turn without putting network requests on the turn's
// critical path. The server expires abandoned activity if the connector exits abruptly.
func (c *Client) trackTurn(parent context.Context) func() {
	path := "/api/agent-activity/" + rand.Text()
	ctx, cancel := context.WithCancel(parent)
	report := func(ctx context.Context, method string) error {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		return c.request(ctx, method, path, nil, nil)
	}
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			err := report(ctx, http.MethodPut)
			var apiErr *APIError
			if errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusMethodNotAllowed) {
				return // Older AIF servers can still run the connector.
			}
			select {
			case <-ctx.Done():
				_ = report(context.WithoutCancel(ctx), http.MethodDelete)
				return
			case <-ticker.C:
			}
		}
	}()
	return cancel
}
