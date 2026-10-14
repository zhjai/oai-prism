package facade

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/prism"
)

func TestPreservedTranscriptEveryByteIncludingUTF8AndJSON(t *testing.T) {
	text := " \nBEGIN\n" + strings.Repeat("中文🙂\\\"{\"tool_result\":\"完整内容\"}\n", 40000) + "\nEND \n"
	parts, err := splitPreservedTranscript(text, 8191)
	if err != nil || len(parts) < 100 {
		t.Fatalf("parts=%d err=%v", len(parts), err)
	}
	if strings.Join(parts, "") != text {
		t.Fatal("fragment delivery changed or omitted context bytes")
	}
	for _, part := range parts {
		if !utf8.ValidString(part) || len(part) > 8191 {
			t.Fatal("invalid transport fragment")
		}
	}
}

func TestPreservedContextAdmissionAndTransportAreSeparate(t *testing.T) {
	r := &Runner{cfg: &config.Config{Facade: config.FacadeConfig{MaxPromptBytes: 98304}}}
	items := []prism.InputItem{prism.NewUserItem(strings.Repeat("data ", 25000))}
	req := &RunRequest{Input: items, Bridge: true, PreserveContext: true, Native: &nativeTurn{conv: nativeConv("", itemText(items[0]))}}
	if err := r.checkPromptSize(req); err != nil {
		t.Fatal("125KB complete context prematurely rejected", err)
	}
	if err := r.checkUpstreamPromptSize(items); !errors.Is(err, ErrContextTooLarge) {
		t.Fatal("physical transport limit bypassed", err)
	}
	req.Input = []prism.InputItem{prism.NewUserItem(strings.Repeat("x ", (1<<19)+1))}
	if err := r.checkPromptSize(req); !errors.Is(err, ErrContextTooLarge) || !strings.Contains(err.Error(), "max_context_bytes") {
		t.Fatal("logical context limit not enforced", err)
	}
}
