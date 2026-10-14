package facade

import (
	"context"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/sse"
)

// Comments carry no model content and work in both Chat and Anthropic SSE.
// Join the writer before sending a terminal event or closing the stream.
func streamHeartbeat(ctx context.Context, sw *sse.Writer) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				if sw.WriteRaw(sse.CommentPing) != nil {
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(stop); <-done }) }
}
