package facade

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPlainStreamsKeepAliveAndStopBeforeTerminal(t *testing.T) {
	for _, api := range []string{"chat", "messages"} {
		t.Run(api, func(t *testing.T) {
			f := newProgressFixture(t, []string{progressPending, progressFinal}, 1800*time.Millisecond)
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/", nil)
			if api == "chat" {
				f.handler.streamChat(w, r, f.request, "id", 1, "test", nil, nil)
			} else {
				f.handler.streamAnthropic(w, r, f.request, "id", "test")
			}
			out := w.Body.String()
			terminal := "data: [DONE]"
			if api == "messages" {
				terminal = "event: message_stop"
			}
			ping, end := strings.LastIndex(out, ": ping\n\n"), strings.Index(out, terminal)
			if ping < 0 || end < 0 || ping > end {
				t.Fatal("heartbeat missing or emitted after stream terminal")
			}
		})
	}
}
