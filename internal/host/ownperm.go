package host

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"

	acp "github.com/afrase/acp-go/unstable"
)

// roomTools are the tools of the room that the host answers the agent's
// request for leave to use, on the owner's behalf: what each does is shown to
// the owner as it happens, and to ask him whether the agent may show him a
// file is noise. A tool that reaches outside the room or spends something is
// on this list deliberately, or it is left to the owner.
var roomTools = []string{"attach"}

// answeredByKey is set, in _meta.roomcity of the request's tool call, in the
// copy of a permission request that goes to the server, so that the room and
// the transcript can tell who answered it.
const answeredByKey = "roomcity"

// ownRequests tells which of the agent's permission requests are for the
// room's own tools, by the way each agent names a tool of an MCP server it
// was given: Claude Code, through claudeacp, as mcp__room__<tool> in the
// tool call's _meta.claudeCode.toolName; goose as room__<tool> in the
// _meta.goose.toolCall.toolName of the tool_call update that precedes the
// request, to which the request's toolCallId leads. A request that is not
// named so by its agent goes to the owner like any other.
type ownRequests struct {
	mu    sync.Mutex
	goose map[string]string // toolCallId of a goose tool call of the room's -> tool
	own   map[string]bool   // the JSON-RPC ids of the permission requests that are the room's
}

func newOwnRequests() *ownRequests {
	return &ownRequests{goose: map[string]string{}, own: map[string]bool{}}
}

// isOwn says whether the request with that JSON-RPC id, as it was written, is
// one the host answers.
func (o *ownRequests) isOwn(id string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.own[id]
}

// see is given each message the agent writes, in order. It remembers what
// leads to a request of the room's, and returns the message as the server is
// to be given it: a request the host answers carries who answers it.
func (o *ownRequests) see(msg []byte) []byte {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Update   map[string]any `json:"update"`
			ToolCall map[string]any `json:"toolCall"`
		} `json:"params"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return msg
	}
	switch m.Method {
	case "session/update":
		if kind, _ := m.Params.Update["sessionUpdate"].(string); kind == "tool_call" {
			if id, _ := m.Params.Update["toolCallId"].(string); id != "" {
				if tool := gooseRoomTool(m.Params.Update); tool != "" {
					o.mu.Lock()
					o.goose[id] = tool
					o.mu.Unlock()
				}
			}
		}
	case "session/request_permission":
		id, _ := m.Params.ToolCall["toolCallId"].(string)
		o.mu.Lock()
		tool := o.goose[id]
		o.mu.Unlock()
		if tool == "" {
			tool = claudeRoomTool(m.Params.ToolCall)
		}
		if !isRoomTool(tool) || len(m.ID) == 0 {
			return msg
		}
		o.mu.Lock()
		o.own[string(m.ID)] = true
		o.mu.Unlock()
		return withAnswerer(msg)
	}
	return msg
}

func isRoomTool(name string) bool {
	for _, t := range roomTools {
		if t == name {
			return true
		}
	}
	return false
}

// claudeRoomTool is the tool of the room that a Claude Code tool call names,
// "" for any other.
func claudeRoomTool(call map[string]any) string {
	meta, _ := call["_meta"].(map[string]any)
	cc, _ := meta["claudeCode"].(map[string]any)
	name, _ := cc["toolName"].(string)
	tool, ok := strings.CutPrefix(name, "mcp__"+roomServerName+"__")
	if !ok {
		return ""
	}
	return tool
}

// gooseRoomTool is the tool of the room that a goose tool_call update names,
// "" for any other.
func gooseRoomTool(update map[string]any) string {
	meta, _ := update["_meta"].(map[string]any)
	g, _ := meta["goose"].(map[string]any)
	call, _ := g["toolCall"].(map[string]any)
	name, _ := call["toolName"].(string)
	tool, ok := strings.CutPrefix(name, roomServerName+"__")
	if !ok {
		return ""
	}
	return tool
}

// withAnswerer returns the permission request with _meta.roomcity.answeredBy
// "host" set in its tool call, and the message as it was when that fails.
func withAnswerer(msg []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(msg))
	dec.UseNumber()
	var top map[string]any
	if dec.Decode(&top) != nil {
		return msg
	}
	params, _ := top["params"].(map[string]any)
	call, _ := params["toolCall"].(map[string]any)
	if call == nil {
		return msg
	}
	meta, _ := call["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		call["_meta"] = meta
	}
	meta[answeredByKey] = map[string]any{"answeredBy": "host"}
	out, err := json.Marshal(top)
	if err != nil {
		return msg
	}
	return out
}

// allowOnce is the option of the request that allows the call once, "" when
// there is none.
func allowOnce(options []acp.PermissionOption) acp.PermissionOptionID {
	for _, o := range options {
		if o.Kind == acp.PermissionOptionKindAllowOnce {
			return o.OptionID
		}
	}
	return ""
}
