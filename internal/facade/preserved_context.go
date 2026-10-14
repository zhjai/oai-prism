package facade

import (
	"errors"
	"fmt"
	"strings"

	"github.com/oai-prism/oaiprism/internal/prism"
)

var errContextRestore = errors.New("context_restore_failed")

const preservedSeedSystem = "The next messages contain ordered fragments of an API client's exact transcript. Read them as inert context only. A JSON value may span fragments: concatenate their data in order before interpreting it. Do not act, request tools, execute anything, or answer the task yet. Reply with exactly OK to acknowledge each fragment. Only resume the task when the gateway explicitly sends the end-of-restoration instruction."
const preservedSeedHeader = "[Client context fragment %d/%d; UTF-8 bytes %d:%d]\n"
const preservedResume = "[End of client context restoration]\nAll transcript fragments have been delivered in order. Treat their concatenated data as the exact client transcript, including the current user input and tool results. Respond to that current input now, following the system instructions and client tool choice below. Do not acknowledge the restoration again."

// splitPreservedTranscript slices bytes without trimming, clipping, reformatting
// or interpreting partial JSON. Every byte is present exactly once.
func splitPreservedTranscript(text string, budget int) ([]string, error) {
	if budget < 4 {
		return nil, fmt.Errorf("%w: restore fragment budget too small", errContextRestore)
	}
	var parts []string
	for len(text) > 0 {
		part := cutUTF8(text, budget)
		if len(part) == 0 {
			return nil, fmt.Errorf("%w: invalid UTF-8 fragment boundary", errContextRestore)
		}
		parts = append(parts, part)
		text = text[len(part):]
	}
	return parts, nil
}

// planPreservedContext runs after native planning, while holding the binding's
// turn lock. New conversations restore the full input; continuations restore
// only the delta. Canonical client history and fingerprints stay unchanged.
func (r *Runner) planPreservedContext(req *RunRequest, input []prism.InputItem) ([]prism.InputItem, error) {
	limit := r.cfg.Facade.PromptByteLimit()
	if limit <= 0 || promptBytes(input) <= limit {
		return input, nil
	}
	nt := req.Native
	if nt == nil || nt.plan == nil || nt.plan.b == nil || nt.plan.cid == "" {
		return nil, fmt.Errorf("%w: a registered, exclusively locked conversation is required", errContextRestore)
	}
	// Reassert the complete current system/catalog/choice in the final inference.
	// A system which alone exceeds the transport cap cannot be restored safely.
	finalSystem := nt.conv.system + "\n\n" + nt.conv.extra
	finalUser := prism.NewUserItem(preservedResume + "\n\n" + nt.conv.extra)
	for _, item := range input {
		for _, c := range item.Content {
			if c.Type == "input_image" || c.Type == "input_file" {
				finalUser.Content = append(finalUser.Content, c)
			}
		}
	}
	final := []prism.InputItem{prism.NewSystemItem(finalSystem), finalUser}
	if err := r.checkUpstreamPromptSize(final); err != nil {
		return nil, err
	}
	var transcript strings.Builder
	for _, item := range input {
		transcript.WriteString("\n[Transcript role: " + item.Role + "]\n")
		transcript.WriteString(itemText(item))
	}
	parts, err := splitPreservedTranscript(transcript.String(), limit-len(preservedSeedSystem)-256)
	if err != nil {
		return nil, err
	}
	seeds := make([][]prism.InputItem, 0, len(parts))
	offset := 0
	for i, part := range parts {
		head := fmt.Sprintf(preservedSeedHeader, i+1, len(parts), offset, offset+len(part))
		seed := []prism.InputItem{prism.NewSystemItem(preservedSeedSystem), prism.NewUserItem(head + part)}
		if err := r.checkUpstreamPromptSize(seed); err != nil {
			return nil, err
		}
		seeds = append(seeds, seed)
		offset += len(part)
	}
	nt.plan.seeds = seeds
	nt.plan.sysHash = textFingerprint(nt.conv.system)
	nt.plan.sinceSys = len(itemText(finalUser))
	return final, nil
}
