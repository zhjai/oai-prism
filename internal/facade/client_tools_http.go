package facade

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/oai-prism/oaiprism/internal/middleware"
)

// prepareClientTools runs before model/account lookup so invalid declarations
// and broken tool histories fail before any upstream operation.
func (h *Handler) prepareClientTools(r *http.Request, raw map[string]json.RawMessage, api string) (*clientToolRequest, error) {
	if api == "responses" && codexBridgeRequest(r, raw) {
		return nil, nil
	}
	legacy := func(v json.RawMessage) bool {
		text := strings.TrimSpace(string(v))
		return text != "" && text != "null" && text != "[]"
	}
	if legacy(raw["functions"]) || legacy(raw["function_call"]) {
		return nil, errors.New("legacy functions/function_call are unsupported; use tools/tool_choice")
	}
	active := false
	if t := strings.TrimSpace(string(raw["tools"])); t != "" && t != "null" && t != "[]" {
		active = true
	}
	var hasHistory bool
	switch api {
	case "chat":
		var msgs []ChatMessage
		_ = json.Unmarshal(raw["messages"], &msgs)
		for _, m := range msgs {
			hasHistory = hasHistory || len(m.ToolCalls) > 0 || m.Role == "tool" || m.Role == "function"
		}
	case "responses":
		var items []struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw["input"], &items)
		for _, i := range items {
			hasHistory = hasHistory || strings.HasSuffix(i.Type, "_call") || strings.HasSuffix(i.Type, "_call_output")
		}
	case "messages":
		var msgs []AnthropicMessage
		_ = json.Unmarshal(raw["messages"], &msgs)
		for _, m := range msgs {
			var blocks []struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(m.Content.Raw(), &blocks)
			for _, b := range blocks {
				hasHistory = hasHistory || b.Type == "tool_use" || b.Type == "tool_result"
			}
		}
	}
	b := &clientToolRequest{}
	if api == "responses" {
		_ = json.Unmarshal(raw["previous_response_id"], &b.previous)
		if b.previous == "" {
			_ = json.Unmarshal(raw["previousResponseId"], &b.previous)
		}
		if b.previous != "" {
			if prior, ok := h.toolHistory.get(middleware.Tenant(r.Context()), b.previous); ok {
				b.prior = &prior
				for _, turn := range prior.Turns {
					if len(turn.Calls) > 0 || len(turn.Results) > 0 {
						hasHistory = true
					}
				}
			}
		}
	}
	if !active && !hasHistory {
		// Choice is validated without changing the plain inference path.
		if err := b.parseChoice(raw["tool_choice"], raw["parallel_tool_calls"], api); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var err error
	b.tools, err = normalizeClientTools(raw["tools"], api)
	if err != nil {
		return nil, err
	}
	// Catalogs are kept whole; reject oversized ones rather than pruning schemas.
	catalog, _ := json.Marshal(b.tools)
	cap := 48 << 10
	if limit := h.cfg.Facade.PromptByteLimit(); limit > 0 {
		cap = min(cap, limit/2)
	}
	if len(catalog) > cap {
		return nil, errors.New("tools_too_large: client tool catalog exceeds its prompt budget")
	}
	if err = b.parseChoice(raw["tool_choice"], raw["parallel_tool_calls"], api); err != nil {
		return nil, err
	}
	switch api {
	case "chat":
		b.turns, err = chatClientTurns(raw["messages"])
	case "responses":
		b.turns, err = responsesClientTurns(raw["input"])
	case "messages":
		b.turns, err = anthropicClientTurns(raw["messages"])
	}
	if err != nil {
		return nil, err
	}
	if api == "responses" && b.previous != "" {
		if b.prior == nil {
			return nil, errors.New("previous_response_not_found: expired or unknown response; resend complete history")
		}
		b.turns = groupClientTurns(append(cloneClientTurns(b.prior.Turns), b.turns...))
	}
	if err = validateClientTranscript(b.turns); err != nil {
		return nil, err
	}
	return b, nil
}

func clientToolInputError(w http.ResponseWriter, err error, api ...string) {
	code := "invalid_tool_request"
	if strings.HasPrefix(err.Error(), "previous_response_not_found") {
		code = "previous_response_not_found"
	}
	if strings.HasPrefix(err.Error(), "tools_too_large") {
		code = "tools_too_large"
	}
	if len(api) > 0 && api[0] == "messages" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "code": code, "message": err.Error()}})
		return
	}
	writeErrorCode(w, http.StatusBadRequest, "invalid_request_error", code, err.Error())
}

func (h *Handler) handleClientTools(w http.ResponseWriter, r *http.Request, b *clientToolRequest, raw map[string]json.RawMessage, api, model, effort, publicModel, account, project string) {
	fallback := h.cfg.Facade.DefaultSystemPrompt
	if api == "messages" {
		var sys StringOrArray
		_ = json.Unmarshal(raw["system"], &sys)
		if text := sys.Text(); text != "" {
			fallback = text
		}
	}
	if api == "responses" {
		var instructions string
		_ = json.Unmarshal(raw["instructions"], &instructions)
		if instructions != "" {
			fallback = ""
		}
	}
	input, conv, err := b.conversation(fallback, h.cfg.Facade.PromptByteLimit())
	if err != nil {
		clientToolInputError(w, err, api)
		return
	}
	if h.cfg.Facade.PlatformNotice {
		input = prependSystemText(input, platformNotice)
		conv.system = platformNotice + "\n\n" + conv.system
	}
	if api == "responses" {
		var instructions string
		_ = json.Unmarshal(raw["instructions"], &instructions)
		if instructions != "" {
			input = prependSystemText(input, instructions)
			conv.system = instructions + "\n\n" + conv.system
		}
	}
	var user string
	_ = json.Unmarshal(raw["user"], &user)
	if api == "messages" {
		user = anthropicUserID(raw)
	}
	sticky := ""
	switch api {
	case "chat":
		var msgs []ChatMessage
		_ = json.Unmarshal(raw["messages"], &msgs)
		sticky = conversationKey(r, raw, msgs)
	case "responses":
		sticky = responsesConversationKey(r, raw, input)
	case "messages":
		var msgs []AnthropicMessage
		_ = json.Unmarshal(raw["messages"], &msgs)
		sticky = anthropicConversationKey(r, raw, msgs)
	}
	idPrefix := "chatcmpl-"
	if api == "responses" {
		idPrefix = "resp_"
	} else if api == "messages" {
		idPrefix = "msg_"
	}
	id := newID(idPrefix)
	runReq := &RunRequest{Input: input, Model: model, Effort: effort, UserID: user, Metadata: clientMetadata(raw), StickyKey: sticky, AccountID: account, ProjectID: project, API: api, Bridge: true, ExtraHeaders: extractSentinelToken(r)}
	known := chatKnownFields
	if api == "responses" {
		known = responsesKnownFields
	}
	if api == "messages" {
		known = anthropicKnownFields
	}
	runReq.Extra = passthroughFields(raw, known)
	chainKey := ""
	nativeKey, strong := sticky, isStrongSessionKey(sticky)
	if api == "responses" {
		chainKey = scopeKey(r, "r:"+id)
		lookup := ""
		if strong {
			lookup = sticky
		}
		hit, found := sessionChainFind(middleware.Tenant(r.Context()), lookup, b.previous, conversationIDFrom(r, raw))
		if strong {
			chainKey = sticky
		} else if found {
			chainKey = hit.Key
		}
		if found {
			nativeKey, strong = chainKey, true
			if runReq.ProjectID == "" && hit.ProjectID != "" {
				runReq.ProjectID = hit.ProjectID
				runReq.MarkProjectFromChain()
			}
			runReq.BoundAccountID = hit.AccountID
		}
		if b.prior != nil && runReq.BoundAccountID == "" {
			runReq.BoundAccountID = b.prior.Account
		}
		sessionChainBind(chainKey, runReq.ProjectID)
	}
	h.attachNative(runReq, &nativeTurn{key: nativeKey, strong: strong, conv: conv})
	if !verifyCatalogContinuity(w, r, runReq) {
		return
	}
	if err = h.runner.checkPromptSize(runReq); err != nil {
		if api == "messages" {
			writeAnthropicTooLarge(w, err)
		} else {
			writeContextTooLarge(w, err)
		}
		return
	}
	var reply *clientToolReply
	runReq.PostProcess = func(res *RunResult) error {
		if res == nil {
			return errClientToolOutput
		}
		var parseErr error
		reply, parseErr = b.parseReply(res.Text)
		if parseErr != nil {
			return parseErr
		}
		if api == "messages" {
			for i := range reply.Calls {
				reply.Calls[i].ID = newID("toolu_")
			}
		}
		canonical := toolText(clientAssistant(reply))
		res.CanonicalAssistant = &canonical
		return nil
	}
	finish := func(res *RunResult) {
		if api != "responses" {
			return
		}
		sessionChainRecord(chainKey, res, model)
		sessionChainRecordLocalID(chainKey, id)
		if nt := runReq.Native; nt != nil {
			h.runner.AliasNative(chainKey, nt.key)
		}
		turns := append(cloneClientTurns(b.turns), clientAssistant(reply))
		if string(raw["store"]) != "false" {
			h.toolHistory.put(middleware.Tenant(r.Context()), id, turns, res.AccountID, model)
		}
	}
	var stream bool
	_ = json.Unmarshal(raw["stream"], &stream)
	if stream {
		h.streamClientTools(w, r, runReq, b, api, id, publicModel, time.Now().Unix(), &reply, finish, raw)
		return
	}
	res, err := h.runner.Run(r.Context(), runReq, nil)
	bindLogResult(r, res)
	if err != nil {
		h.writeClientToolError(w, r, api, err)
		return
	}
	finish(res)
	setConversationHeader(w, res.ConversationID)
	h.syncClientToolReply(w, api, id, publicModel, time.Now().Unix(), reply, res)
}

func (h *Handler) writeClientToolError(w http.ResponseWriter, r *http.Request, api string, err error) {
	middleware.RecordLogError(r, "client tools failed: %v", err)
	if api == "messages" && writeAnthropicTooLarge(w, err) {
		return
	}
	if writeContextTooLarge(w, err) {
		return
	}
	if errors.Is(err, errClientToolOutput) {
		if api == "messages" {
			writeJSON(w, http.StatusBadGateway, map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "code": "tool_bridge_invalid_output", "message": err.Error()}})
			return
		}
		writeErrorCode(w, http.StatusBadGateway, "upstream_error", "tool_bridge_invalid_output", err.Error())
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	status, typ, msg := mapError(err)
	writeError(w, status, typ, msg)
}
