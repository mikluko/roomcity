package host

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/claudeacp/claudeacp"
	"sarak.sh/mikluko/claudeacp/claudeagent/claudetest"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

const (
	initRequest = `{"type":"control_request","request":{"subtype":"initialize"}}`
	ok          = `{"type":"control_response","response":{"subtype":"success","request_id":"$req","response":{}}}`
	idle        = `{"type":"system","subtype":"session_state_changed","state":"idle"}`
	running     = `{"type":"system","subtype":"session_state_changed","state":"running"}`
	echo        = `{"type":"user","uuid":"$uuid","message":{"role":"user","content":"…"},"isReplay":true}`
	result      = `{"type":"result","subtype":"success","is_error":false,"result":"","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":1},"user_message_uuids":["$uuid"]}`
)

// claude is a scripted claude: it answers initialize, then plays steps. It
// accepts an interrupt, and answers the asks every session makes of its own.
func claude(steps ...claudetest.Step) *claudetest.Fake {
	f := claudetest.New(append([]claudetest.Step{
		claudetest.Expect(initRequest),
		claudetest.Send(`{"type":"control_response","response":{"subtype":"success","request_id":"$req","response":{}}}`),
	}, steps...)...)
	f.Respond(`{"type":"control_request","request":{"subtype":"interrupt"}}`, ok)
	f.Respond(`{"type":"control_request","request":{"subtype":"get_context_usage"}}`, ok)
	f.Respond(`{"type":"control_request","request":{"subtype":"generate_session_title"}}`,
		`{"type":"control_response","response":{"subtype":"success","request_id":"$req","response":{"title":null}}}`)
	return f
}

// answering is a turn: claude takes the prompt and answers text.
func answering(text string) []claudetest.Step {
	return []claudetest.Step{
		claudetest.Expect(`{"type":"user"}`),
		claudetest.Send(echo),
		claudetest.Send(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1","model":"claude-haiku-5-5","usage":{"input_tokens":1}}}}`),
		claudetest.Send(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + text + `"}}}`),
		claudetest.Send(`{"type":"assistant","message":{"id":"msg_1","model":"claude-haiku-5-5","content":[{"type":"text","text":"` + text + `"}]}}`),
		claudetest.Send(result),
		claudetest.Send(idle),
	}
}

// rig is a host running against a fake server and a scripted claude.
type rig struct {
	server *fakeServer
	done   chan struct{} // closed when Run has returned
	err    error         // what Run returned, once done is closed
	cancel context.CancelFunc
}

func start(t *testing.T, f *claudetest.Fake) *rig {
	t.Helper()
	server, url := newFakeServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	r := &rig{server: server, done: make(chan struct{}), cancel: cancel}
	cfg := Config{
		Session: "s", Token: testToken, Server: url,
		WorkDir: t.TempDir(),
		Claude:  claudeacp.Config{CLIPath: "/claude", ConfigDir: t.TempDir(), Spawn: f.Spawn},
		Retry:   time.Millisecond,
	}
	go func() { r.err = Run(ctx, cfg); close(r.done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			t.Error("the host did not end")
		}
	})
	return r
}

// stop tells the host to stop and waits for it to end.
func (r *rig) stop(t *testing.T) {
	t.Helper()
	r.server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_Stop{Stop: &roomcityv1.Stop{}}})
	select {
	case <-r.done:
		require.NoError(t, r.err)
	case <-time.After(10 * time.Second):
		t.Fatal("the host did not stop")
	}
}

func prompt(text string) *roomcityv1.AttachResponse {
	return &roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_Prompt{Prompt: &roomcityv1.Prompt{Text: text, Source: roomcityv1.PromptSource_PROMPT_SOURCE_OWNER}}}
}

// stopReasons are the stop reasons of the prompts the agent has answered.
func stopReasons(t *testing.T, frames []*roomcityv1.Frame) []string {
	t.Helper()
	var out []string
	for _, f := range frames {
		if f.Direction != roomcityv1.FrameDirection_FRAME_DIRECTION_FROM_AGENT {
			continue
		}
		var res struct {
			StopReason string `json:"stopReason"`
		}
		if w := decode(t, f); len(w.Result) > 0 && json.Unmarshal(w.Result, &res) == nil && res.StopReason != "" {
			out = append(out, res.StopReason)
		}
	}
	return out
}

func methods(t *testing.T, frames []*roomcityv1.Frame, dir roomcityv1.FrameDirection) []string {
	t.Helper()
	var out []string
	for _, f := range frames {
		if m := decode(t, f).Method; m != "" && f.Direction == dir {
			out = append(out, m)
		}
	}
	return out
}

func requireContiguous(t *testing.T, frames []*roomcityv1.Frame) {
	t.Helper()
	for i, f := range frames {
		require.Equal(t, uint64(i+1), f.Seq, "frame %d", i)
		require.NotNil(t, f.Time)
	}
}

func TestPromptBecomesATurnWhoseFramesReachTheServerInOrder(t *testing.T) {
	r := start(t, claude(answering("Hello from claude")...))

	r.server.instruct(prompt("Say hello"))
	r.server.eventually(t, "the turn's answer", func(fs []*roomcityv1.Frame) bool {
		return len(stopReasons(t, fs)) == 1
	})
	r.stop(t)

	frames := r.server.stored()
	requireContiguous(t, frames)
	require.Equal(t, []string{"end_turn"}, stopReasons(t, frames))
	to := methods(t, frames, roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT)
	require.Equal(t, []string{"initialize", "session/new", "session/prompt"}, to)
	require.Contains(t, methods(t, frames, roomcityv1.FrameDirection_FRAME_DIRECTION_FROM_AGENT), "session/update")
	var text strings.Builder
	for _, f := range frames {
		text.Write(f.Message)
	}
	require.Contains(t, text.String(), "Hello from claude")
	require.Contains(t, text.String(), "Say hello")
}

func TestCancelEndsARunningTurn(t *testing.T) {
	interrupted := make(chan struct{})
	started := make(chan struct{})
	r := start(t, claude(
		claudetest.Expect(`{"type":"user"}`),
		claudetest.Send(echo),
		claudetest.Send(running),
		claudetest.Do(func() { close(started) }),
		claudetest.Until(interrupted), // the turn does not end by itself
		claudetest.Send(result),
		claudetest.Send(idle),
	))

	r.server.instruct(prompt("go on for ever"))
	<-started
	r.server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_Cancel{Cancel: &roomcityv1.Cancel{}}})
	r.server.eventually(t, "the cancel", func(fs []*roomcityv1.Frame) bool {
		return slices.Contains(methods(t, fs, roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT), "session/cancel")
	})
	close(interrupted) // claude ends the turn it was told to interrupt
	r.server.eventually(t, "the cancelled turn's answer", func(fs []*roomcityv1.Frame) bool {
		return len(stopReasons(t, fs)) == 1
	})
	r.stop(t)

	require.Equal(t, []string{"cancelled"}, stopReasons(t, r.server.stored()))
}

func TestPermissionRequestWaitsAndIsAnswered(t *testing.T) {
	steps := []claudetest.Step{
		claudetest.Expect(`{"type":"user"}`),
		claudetest.Send(echo),
		claudetest.Send(`{"type":"assistant","message":{"id":"msg_1","model":"claude-haiku-5-5","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"rm x"}}]}}`),
		claudetest.Send(`{"type":"control_request","request_id":"p_toolu_1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm x"},"tool_use_id":"toolu_1"}}`),
		claudetest.Expect(`{"type":"control_response","response":{"request_id":"p_toolu_1","response":{"behavior":"allow"}}}`),
		claudetest.Send(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"removed"}]}}`),
		claudetest.Send(result),
		claudetest.Send(idle),
	}
	r := start(t, claude(steps...))

	r.server.instruct(prompt("remove x"))
	var asked wire
	r.server.eventually(t, "the permission request", func(fs []*roomcityv1.Frame) bool {
		for _, f := range fs {
			if w := decode(t, f); w.Method == "session/request_permission" {
				asked = w
				return true
			}
		}
		return false
	})

	// Nothing ends the turn while the request waits.
	time.Sleep(50 * time.Millisecond)
	require.Empty(t, stopReasons(t, r.server.stored()))

	var params struct {
		Options []struct {
			OptionID string `json:"optionId"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(asked.Params, &params))
	require.Contains(t, []string{params.Options[0].OptionID}, "allow-once")
	r.server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_PermissionAnswer{
		PermissionAnswer: &roomcityv1.PermissionAnswer{RequestId: string(asked.ID), OptionId: "allow-once"},
	}})
	r.server.eventually(t, "the turn's end", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) == 1 })
	r.stop(t)

	require.Equal(t, []string{"end_turn"}, stopReasons(t, r.server.stored()))
}

func TestAttachTakenUpAgainNamesTheLastInstructionAndDoublesNothing(t *testing.T) {
	steps := append(answering("one"), answering("two")...)
	r := start(t, claude(steps...))

	r.server.instruct(prompt("first"))
	r.server.eventually(t, "the first answer", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) == 1 })
	r.server.drop()
	for len(r.server.attachments()) < 2 { // the host attaches again
		time.Sleep(time.Millisecond)
	}
	r.server.instruct(prompt("second"))
	r.server.eventually(t, "the second answer", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) == 2 })
	r.stop(t)

	require.Equal(t, []uint64{0, 1}, r.server.attachments()[:2], "the second attachment names the first instruction")
	frames := r.server.stored()
	requireContiguous(t, frames)
	require.Equal(t, []string{"initialize", "session/new", "session/prompt", "session/prompt"},
		methods(t, frames, roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT), "no prompt ran twice")
}

func TestFramesNotAcknowledgedAreSentAgain(t *testing.T) {
	r := start(t, claude(answering("answer")...))
	r.server.failNextReports(5)

	r.server.instruct(prompt("hi"))
	r.server.eventually(t, "the answer after the failed reports", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) == 1 })
	r.stop(t)

	frames := r.server.stored()
	requireContiguous(t, frames)
	require.Equal(t, []string{"end_turn"}, stopReasons(t, frames))
	require.GreaterOrEqual(t, r.server.reportCalls(), 6)
}

func TestWrongTokenIsRefused(t *testing.T) {
	server, url := newFakeServer(t)
	_ = server
	f := claude()
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		done <- Run(ctx, Config{Session: "s", Token: "wrong", Server: url, WorkDir: t.TempDir(),
			Claude: claudeacp.Config{CLIPath: "/claude", ConfigDir: t.TempDir(), Spawn: f.Spawn}, Retry: time.Millisecond})
	}()

	require.ErrorContains(t, <-done, "unauthenticated")
	require.Empty(t, server.stored())
}

func TestConfigFromEnv(t *testing.T) {
	cfg, err := ConfigFromEnv([]string{"ROOMCITY_SESSION=s", "ROOMCITY_TOKEN=t", "ROOMCITY_SERVER=http://x", "ROOMCITY_WORKDIR=/w", "PATH=/bin"})
	require.NoError(t, err)
	require.Equal(t, Config{Session: "s", Token: "t", Server: "http://x", WorkDir: "/w"}, cfg)

	for _, missing := range []string{"ROOMCITY_SESSION", "ROOMCITY_TOKEN", "ROOMCITY_SERVER", "ROOMCITY_WORKDIR"} {
		env := []string{"ROOMCITY_SESSION=s", "ROOMCITY_TOKEN=t", "ROOMCITY_SERVER=http://x", "ROOMCITY_WORKDIR=/w"}
		var kept []string
		for _, kv := range env {
			if !strings.HasPrefix(kv, missing+"=") {
				kept = append(kept, kv)
			}
		}
		_, err := ConfigFromEnv(kept)
		require.ErrorContains(t, err, missing)
	}
}

func TestCancelAnswersAnOpenPermissionRequestAsCancelled(t *testing.T) {
	steps := []claudetest.Step{
		claudetest.Expect(`{"type":"user"}`),
		claudetest.Send(echo),
		claudetest.Send(`{"type":"assistant","message":{"id":"msg_1","model":"claude-haiku-5-5","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"rm x"}}]}}`),
		claudetest.Send(`{"type":"control_request","request_id":"p_toolu_1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm x"},"tool_use_id":"toolu_1"}}`),
		claudetest.Expect(`{"type":"control_response","response":{"request_id":"p_toolu_1","response":{"behavior":"deny"}}}`),
		claudetest.Send(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"refused","is_error":true}]}}`),
		claudetest.Send(result),
		claudetest.Send(idle),
	}
	r := start(t, claude(steps...))
	r.server.instruct(prompt("remove x"))
	r.server.eventually(t, "the permission request", func(fs []*roomcityv1.Frame) bool {
		return slices.Contains(methods(t, fs, roomcityv1.FrameDirection_FRAME_DIRECTION_FROM_AGENT), "session/request_permission")
	})

	r.server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_Cancel{Cancel: &roomcityv1.Cancel{}}})
	r.server.eventually(t, "the request answered", func(fs []*roomcityv1.Frame) bool {
		for _, f := range fs {
			if f.Direction == roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT && strings.Contains(string(f.Message), `"outcome":"cancelled"`) {
				return true
			}
		}
		return false
	})
	r.stop(t)
}

func TestTheOwnersPromptsGoAheadOfGcsThatAreWaiting(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	steps := []claudetest.Step{
		claudetest.Expect(`{"type":"user"}`), claudetest.Send(echo), claudetest.Send(running),
		claudetest.Do(func() { close(started) }), claudetest.Until(release),
		claudetest.Send(result), claudetest.Send(idle),
	}
	steps = append(steps, answering("a")...)
	steps = append(steps, answering("b")...)
	steps = append(steps, answering("c")...)
	steps = append(steps, answering("d")...)
	r := start(t, claude(steps...))
	gc := func(text string) *roomcityv1.AttachResponse {
		in := prompt(text)
		in.GetPrompt().Source = roomcityv1.PromptSource_PROMPT_SOURCE_GC
		return in
	}

	r.server.instruct(prompt("running"))
	<-started
	r.server.instruct(gc("nudge"))
	r.server.instruct(prompt("owner one"))
	r.server.instruct(gc("another nudge"))
	r.server.instruct(prompt("owner two"))
	time.Sleep(50 * time.Millisecond) // the host has them all queued
	close(release)
	r.server.eventually(t, "five turns", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) == 5 })
	r.stop(t)

	var order []string
	for _, f := range r.server.stored() {
		if f.Direction != roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT {
			continue
		}
		var w struct {
			Method string `json:"method"`
			Params struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal(f.Message, &w))
		if w.Method == "session/prompt" {
			order = append(order, w.Params.Prompt[0].Text)
		}
	}
	require.Equal(t, []string{"running", "owner one", "owner two", "nudge", "another nudge"}, order)
}

func TestFramesContinueAfterTheOnesTheServerHolds(t *testing.T) {
	server, url := newFakeServer(t)
	for i := range 5 { // what an earlier host in this pod reported
		server.frames = append(server.frames, &roomcityv1.Frame{Seq: uint64(i + 1)})
	}
	f := claude(answering("hi")...)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- Run(ctx, Config{Session: "s", Token: testToken, Server: url, WorkDir: t.TempDir(),
			Claude: claudeacp.Config{CLIPath: "/claude", ConfigDir: t.TempDir(), Spawn: f.Spawn}, Retry: time.Millisecond})
	}()

	server.instruct(prompt("again"))
	server.eventually(t, "the new frames after the old", func(fs []*roomcityv1.Frame) bool { return len(fs) > 10 })
	cancel()
	<-done

	for i, fr := range server.stored() {
		require.Equal(t, uint64(i+1), fr.Seq)
	}
	require.Contains(t, string(server.stored()[5].Message), "initialize")
}

func TestCancelDropsTheOwnersWaitingPromptsAndKeepsGcs(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	steps := []claudetest.Step{
		claudetest.Expect(`{"type":"user"}`), claudetest.Send(echo), claudetest.Send(running),
		claudetest.Do(func() { close(started) }), claudetest.Until(release),
		claudetest.Send(result), claudetest.Send(idle),
	}
	steps = append(steps, answering("a")...)
	steps = append(steps, answering("b")...)
	r := start(t, claude(steps...))
	gc := func(text string) *roomcityv1.AttachResponse {
		in := prompt(text)
		in.GetPrompt().Source = roomcityv1.PromptSource_PROMPT_SOURCE_GC
		return in
	}

	r.server.instruct(prompt("running"))
	<-started
	r.server.instruct(gc("nudge one"))
	r.server.instruct(prompt("owner waits"))
	r.server.instruct(gc("nudge two"))
	r.server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_Cancel{Cancel: &roomcityv1.Cancel{}}})
	r.server.eventually(t, "the cancel", func(fs []*roomcityv1.Frame) bool {
		return slices.Contains(methods(t, fs, roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT), "session/cancel")
	})
	close(release) // claude ends the interrupted turn
	r.server.eventually(t, "three turns", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) == 3 })
	r.stop(t)

	var order []string
	for _, f := range r.server.stored() {
		if f.Direction != roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT {
			continue
		}
		var w struct {
			Method string `json:"method"`
			Params struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal(f.Message, &w))
		if w.Method == "session/prompt" {
			order = append(order, w.Params.Prompt[0].Text)
		}
	}
	require.Equal(t, []string{"running", "nudge one", "nudge two"}, order, "the owner's waiting prompt was dropped, gc's were delivered in order after the stop")
}

func TestAnInterruptEndsTheTurnAndDropsNothing(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	steps := []claudetest.Step{
		claudetest.Expect(`{"type":"user"}`), claudetest.Send(echo), claudetest.Send(running),
		claudetest.Do(func() { close(started) }), claudetest.Until(release),
		claudetest.Send(result), claudetest.Send(idle),
	}
	steps = append(steps, answering("a")...)
	steps = append(steps, answering("b")...)
	steps = append(steps, answering("c")...)
	r := start(t, claude(steps...))
	fromGc := func(text string) *roomcityv1.AttachResponse {
		in := prompt(text)
		in.GetPrompt().Source = roomcityv1.PromptSource_PROMPT_SOURCE_GC
		return in
	}

	r.server.instruct(prompt("running"))
	<-started
	r.server.instruct(fromGc("nudge one"))
	r.server.instruct(prompt("owner waits"))
	r.server.instruct(fromGc("nudge two"))
	r.server.instruct(&roomcityv1.AttachResponse{Instruction: &roomcityv1.AttachResponse_Cancel{Cancel: &roomcityv1.Cancel{KeepWaiting: true}}})
	r.server.eventually(t, "the cancel", func(fs []*roomcityv1.Frame) bool {
		return slices.Contains(methods(t, fs, roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT), "session/cancel")
	})
	close(release)
	r.server.eventually(t, "four turns", func(fs []*roomcityv1.Frame) bool { return len(stopReasons(t, fs)) == 4 })
	r.stop(t)

	var order []string
	for _, f := range r.server.stored() {
		if f.Direction != roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT {
			continue
		}
		var w struct {
			Method string `json:"method"`
			Params struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal(f.Message, &w))
		if w.Method == "session/prompt" {
			order = append(order, w.Params.Prompt[0].Text)
		}
	}
	require.Equal(t, []string{"running", "owner waits", "nudge one", "nudge two"}, order, "nothing was dropped; the owner's came first")
}
