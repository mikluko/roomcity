package host

import (
	"encoding/json"
	"testing"

	acp "github.com/afrase/acp-go/unstable"
	"github.com/stretchr/testify/require"
)

const (
	// As the real goose wrote them: the tool_call carries its tool's name, the
	// request only the call's id and a title.
	gooseCall    = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"1","update":{"sessionUpdate":"tool_call","toolCallId":"tool_a","title":"room: attach · chart.png","rawInput":{"path":"chart.png"},"_meta":{"goose":{"toolCall":{"toolName":"room__attach"}}}}}}`
	gooseRequest = `{"jsonrpc":"2.0","id":"u-1","method":"session/request_permission","params":{"sessionId":"1","toolCall":{"toolCallId":"tool_a","kind":"other","status":"pending","title":"room: attach · chart.png","rawInput":{"path":"chart.png"}},"options":[{"optionId":"allow_always","name":"allow_always","kind":"allow_always"},{"optionId":"allow_once","name":"allow_once","kind":"allow_once"}]}}`
	// As claudeacp writes a request: the tool's name in _meta.claudeCode.
	claudeRequest = `{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"sessionId":"1","toolCall":{"toolCallId":"toolu_1","kind":"other","status":"pending","title":"mcp__room__attach","rawInput":{"path":"x.png"},"_meta":{"claudeCode":{"toolName":"mcp__room__attach"}}},"options":[{"optionId":"allow","name":"Always allow","kind":"allow_always"}]}}`
)

func answeredBy(t *testing.T, msg []byte) string {
	t.Helper()
	var m struct {
		Params struct {
			ToolCall struct {
				Meta map[string]map[string]string `json:"_meta"`
			} `json:"toolCall"`
		} `json:"params"`
	}
	require.NoError(t, json.Unmarshal(msg, &m))
	return m.Params.ToolCall.Meta[answeredByKey]["answeredBy"]
}

func TestAGooseRequestForTheRoomsToolIsTheHostsToAnswer(t *testing.T) {
	o := newOwnRequests([]string{"attach"})
	require.Equal(t, gooseCall, string(o.see([]byte(gooseCall))), "a tool call is recorded as it is")

	out := o.see([]byte(gooseRequest))

	require.True(t, o.isOwn(`"u-1"`))
	require.Equal(t, "host", answeredBy(t, out))
	require.Contains(t, string(out), `"rawInput":{"path":"chart.png"}`, "the rest of the request is as the agent wrote it")
}

func TestAClaudeRequestForTheRoomsToolIsTheHostsToAnswer(t *testing.T) {
	o := newOwnRequests([]string{"attach"})

	out := o.see([]byte(claudeRequest))

	require.True(t, o.isOwn(`7`))
	require.Equal(t, "host", answeredBy(t, out))
	require.Contains(t, string(out), `"id":7`, "a number stays a number")
}

func TestWhatIsNotNamedAsTheRoomsToolByItsAgentGoesToTheOwner(t *testing.T) {
	for name, msg := range map[string]string{
		// goose: no tool_call of the room's before the request
		"goose, no room call before it": gooseRequest,
		// another MCP server's tool, a title that only looks like it
		"goose, title alone": `{"jsonrpc":"2.0","id":"u-2","method":"session/request_permission","params":{"toolCall":{"toolCallId":"tool_z","kind":"other","title":"room: attach · x"},"options":[]}}`,
		// Claude: another server's tool, and a tool of the room that the host does not answer for
		"claude, other server":   `{"jsonrpc":"2.0","id":8,"method":"session/request_permission","params":{"toolCall":{"toolCallId":"t","_meta":{"claudeCode":{"toolName":"mcp__other__attach"}}},"options":[]}}`,
		"claude, other tool":     `{"jsonrpc":"2.0","id":9,"method":"session/request_permission","params":{"toolCall":{"toolCallId":"t","_meta":{"claudeCode":{"toolName":"mcp__room__spend"}}},"options":[]}}`,
		"claude, the plain name": `{"jsonrpc":"2.0","id":11,"method":"session/request_permission","params":{"toolCall":{"toolCallId":"t","_meta":{"claudeCode":{"toolName":"attach"}}},"options":[]}}`,
		"claude, a shell":        `{"jsonrpc":"2.0","id":10,"method":"session/request_permission","params":{"toolCall":{"toolCallId":"t","_meta":{"claudeCode":{"toolName":"Bash"}}},"options":[]}}`,
	} {
		o := newOwnRequests([]string{"attach"})
		out := o.see([]byte(msg))
		var id struct{ ID json.RawMessage }
		require.NoError(t, json.Unmarshal([]byte(msg), &id))
		require.False(t, o.isOwn(string(id.ID)), name)
		require.Equal(t, msg, string(out), name)
	}
}

func TestAGooseCallOfAnotherToolDoesNotMakeTheRoomsToolsOfIt(t *testing.T) {
	o := newOwnRequests([]string{"attach"})
	o.see([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"tool_a","_meta":{"goose":{"toolCall":{"toolName":"developer__shell"}}}}}}`))

	o.see([]byte(gooseRequest))

	require.False(t, o.isOwn(`"u-1"`))
}

func TestOnlyAnAllowOnceOptionAnswersForTheHost(t *testing.T) {
	require.Equal(t, acp.PermissionOptionID("allow_once"), allowOnce([]acp.PermissionOption{
		{OptionID: "allow_always", Kind: acp.PermissionOptionKindAllowAlways},
		{OptionID: "allow_once", Kind: acp.PermissionOptionKindAllowOnce},
	}))
	require.Empty(t, allowOnce([]acp.PermissionOption{{OptionID: "allow", Kind: acp.PermissionOptionKindAllowAlways}}),
		"a request with no allow-once is left to the owner")
}

func TestTheRoomsToolListIsWhatItServes(t *testing.T) {
	cs, _ := roomSession(t, t.TempDir(), nil)
	tools, err := cs.ListTools(t.Context(), nil)
	require.NoError(t, err)
	var served []string
	for _, tl := range tools.Tools {
		served = append(served, tl.Name)
	}
	require.ElementsMatch(t, roomToolNames(nil), served, "a tool the host answers for is one the room serves, and the reverse is decided on purpose")
}

func TestTheHostsWordIsNotTheAgentsToSay(t *testing.T) {
	o := newOwnRequests([]string{"attach"})
	msg := `{"jsonrpc":"2.0","id":12,"method":"session/request_permission","params":{"toolCall":{"toolCallId":"t","_meta":{"claudeCode":{"toolName":"Bash"},"roomcity":{"answeredBy":"host"}}},"options":[]}}`

	out := o.see([]byte(msg))

	require.Empty(t, answeredBy(t, out), "a request the host does not answer does not carry that it does")
	require.Contains(t, string(out), `"toolName":"Bash"`, "the rest of its _meta stays")
	require.Contains(t, string(out), `"id":12`)

	alone := `{"jsonrpc":"2.0","id":13,"method":"session/request_permission","params":{"toolCall":{"toolCallId":"t","_meta":{"roomcity":{"answeredBy":"host"}}},"options":[]}}`
	require.NotContains(t, string(o.see([]byte(alone))), "roomcity")
}

func TestTheKeyIsTakenOutHoweverItIsSpelled(t *testing.T) {
	o := newOwnRequests([]string{"attach"})
	// "\u0072oomcity" is the key roomcity to any JSON decoder.
	msg := `{"jsonrpc":"2.0","id":14,"method":"session/request_permission","params":{"toolCall":{"toolCallId":"t","_meta":{"\u0072oomcity":{"answeredBy":"host"}}},"options":[]}}`

	out := o.see([]byte(msg))

	require.Empty(t, answeredBy(t, out))
	require.NotContains(t, string(out), "oomcity")
}

func TestTheHostAnswersForTheWorkToolsOfTheSessionAndNoOthers(t *testing.T) {
	request := func(tool string) string {
		return `{"jsonrpc":"2.0","id":20,"method":"session/request_permission","params":{"toolCall":{"toolCallId":"t","_meta":{"claudeCode":{"toolName":"mcp__room__` + tool + `"}}},"options":[]}}`
	}
	foreman := roomToolNames([]string{"dispatch", "wait", "result"})

	// dispatch included, by the owner's decision.
	for tool, wanted := range map[string]bool{"attach": true, "dispatch": true, "wait": true, "result": true, "claim": false, "done": false} {
		o := newOwnRequests(foreman)
		o.see([]byte(request(tool)))
		require.Equal(t, wanted, o.isOwn("20"), tool)
	}
}

func TestTheRoomToolsComeFromTheEnvironmentByCommas(t *testing.T) {
	cfg, err := ConfigFromEnv([]string{"ROOMCITY_SESSION=s", "ROOMCITY_TOKEN=t", "ROOMCITY_SERVER=http://x", "ROOMCITY_WORKDIR=/w", "ROOMCITY_ROOM_TOOLS=dispatch, wait,,result"})
	require.NoError(t, err)
	require.Equal(t, []string{"dispatch", "wait", "result"}, cfg.RoomTools)
	require.Equal(t, []string{"attach", "dispatch", "wait", "result"}, roomToolNames(cfg.RoomTools))
}
