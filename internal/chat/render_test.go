package chat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/session"
)

// fakeOut records what a renderer does to its room.
type fakeOut struct {
	sent      []Message
	reactions []string // "<to> <key>"
	undone    []ReactionID
	typings   []bool
	answers   []string // "<request> <option>"
	prompts   map[uint64]MessageID
	forgotten []uint64
}

func (o *fakeOut) send(m Message) MessageID {
	o.sent = append(o.sent, m)
	return MessageID(fmt.Sprintf("m%d", len(o.sent)))
}

func (o *fakeOut) react(to MessageID, key string) ReactionID {
	o.reactions = append(o.reactions, fmt.Sprintf("%s %s", to, key))
	return ReactionID(fmt.Sprintf("r%d", len(o.reactions)))
}

func (o *fakeOut) unreact(r ReactionID)               { o.undone = append(o.undone, r) }
func (o *fakeOut) typing(on bool)                     { o.typings = append(o.typings, on) }
func (o *fakeOut) promptMessage(seq uint64) MessageID { return o.prompts[seq] }
func (o *fakeOut) forgetPrompt(seq uint64)            { o.forgotten = append(o.forgotten, seq) }
func (o *fakeOut) answerPermission(req, opt string) {
	o.answers = append(o.answers, req+" "+opt)
}

// clock is a time that moves when a test says.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestRenderer() (*renderer, *fakeOut, *clock) {
	out := &fakeOut{prompts: map[uint64]MessageID{}}
	c := &clock{t: time.Unix(1000, 0)}
	return newRenderer(out, c.now, planEvery), out, c
}

func fromAgent(json string) session.Frame {
	return session.Frame{Direction: session.FromAgent, Message: []byte(json)}
}

func toAgent(json string) session.Frame {
	return session.Frame{Direction: session.ToAgent, Message: []byte(json)}
}

func update(kind, fields string) session.Frame {
	if fields != "" {
		fields = "," + fields
	}
	return fromAgent(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"` + kind + `"` + fields + `}}}`)
}

func thought(text string) session.Frame {
	return update("agent_thought_chunk", `"content":{"type":"text","text":"`+text+`"}`)
}

func endOfTurn() session.Frame {
	return fromAgent(`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)
}

func bodies(ms []Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Body)
	}
	return out
}

func TestRendererShowsAThoughtRunAsOneFoldedNoticeBeforeWhatFollows(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(thought("first "))
	r.event(update("usage_update", `"used":10`))
	r.event(thought("second"))
	require.Empty(t, out.sent, "a run is sent when it ends")

	r.event(update("agent_message_chunk", `"content":{"type":"text","text":"Answer.\n\n"}`))
	require.Len(t, out.sent, 2)
	require.Equal(t, Notice, out.sent[0].Kind)
	require.Equal(t, &Fold{Summary: "💭 Thinking", Text: "first second"}, out.sent[0].Fold)
	require.Equal(t, "Answer.", out.sent[1].Body)
}

func TestRendererShowsAToolCallAsAStartNoticeAndAResultInReply(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(update("tool_call", `"toolCallId":"t1","title":"Read x.go","status":"pending","rawInput":{}`))
	require.Empty(t, out.sent, "its input is not known yet")

	r.event(update("tool_call_update", `"toolCallId":"t1","rawInput":{"path":"x.go"}`))
	require.Equal(t, []string{"🔧 **Read x.go**"}, bodies(out.sent))
	require.Equal(t, Notice, out.sent[0].Kind)

	long := strings.Repeat("x", maxToolOutput+100)
	r.event(update("tool_call_update", `"toolCallId":"t1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"`+long+`"}}]`))
	require.Len(t, out.sent, 2)
	result := out.sent[1]
	require.Equal(t, "✅ done", result.Body)
	require.Equal(t, MessageID("m1"), result.ReplyTo)
	require.Equal(t, "output", result.Fold.Summary)
	require.True(t, result.Fold.Code)
	require.True(t, strings.HasSuffix(result.Fold.Text, "\n…"))
	require.Equal(t, maxToolOutput+len("\n…"), len(result.Fold.Text), "the output is cut to its first 4000 bytes")
}

func TestRendererGivesAFailedToolCallWithNoOutputItsResultToo(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(update("tool_call", `"toolCallId":"t1","title":"Run it","status":"pending","rawInput":{"command":"x"}`))
	r.event(update("tool_call_update", `"toolCallId":"t1","status":"failed"`))
	require.Len(t, out.sent, 2)
	require.Equal(t, "❌ failed", out.sent[1].Body)
	require.Nil(t, out.sent[1].Fold)
}

func TestRendererSendsTheTextBeforeAToolCallStarts(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(update("agent_message_chunk", `"content":{"type":"text","text":"Unfinished"}`))
	r.event(update("tool_call", `"toolCallId":"t1","title":"Run","status":"pending","rawInput":{"a":1}`))
	require.Equal(t, []string{"Unfinished", "🔧 **Run**"}, bodies(out.sent))
}

func plan(statuses ...string) session.Frame {
	entries := make([]string, len(statuses))
	for i, s := range statuses {
		entries[i] = fmt.Sprintf(`{"content":"step %d","status":"%s","priority":"medium"}`, i+1, s)
	}
	return update("plan", `"entries":[`+strings.Join(entries, ",")+`]`)
}

func TestRendererEditsThePlanInPlaceNoOftenerThanItsGap(t *testing.T) {
	r, out, c := newTestRenderer()
	r.event(plan("pending", "pending"))
	require.Len(t, out.sent, 1)
	require.Equal(t, "**Plan** 0/2\n\n- ☐ step 1\n- ☐ step 2", out.sent[0].Body)
	require.Empty(t, out.sent[0].Replaces)

	r.event(plan("completed", "pending"))
	r.event(plan("completed", "in_progress"))
	r.tick()
	require.Len(t, out.sent, 1, "inside the gap nothing is edited")

	c.advance(planEvery)
	r.tick()
	require.Len(t, out.sent, 2)
	require.Equal(t, MessageID("m1"), out.sent[1].Replaces)
	require.Equal(t, "**Plan** 1/2\n\n- ☑ ~~step 1~~\n- ▶ **step 2**", out.sent[1].Body)

	r.event(plan("completed", "completed"))
	r.event(endOfTurn())
	require.Len(t, out.sent, 3, "the turn's end makes the last edit at once")
	require.Equal(t, MessageID("m1"), out.sent[2].Replaces)

	r.event(plan("pending"))
	require.Len(t, out.sent, 4)
	require.Empty(t, out.sent[3].Replaces, "the next turn's plan is a new notice")
}

func TestRendererKeepsTheTypingNoticeOnForTheTurnAndOffWhileAPermissionWaits(t *testing.T) {
	r, out, c := newTestRenderer()
	r.event(session.Prompt{Seq: 1, Source: session.FromOwner, State: session.Stored})
	require.Equal(t, []bool{true}, out.typings)

	r.event(update("agent_message_chunk", `"content":{"type":"text","text":"Hi.\n\n"}`))
	require.Equal(t, []bool{true, true}, out.typings, "sent again after the agent speaks")

	c.advance(typingRenew)
	r.tick()
	require.Len(t, out.typings, 3, "renewed before the homeserver lets it lapse")

	r.event(permissionRequest("7", "Run it"))
	require.False(t, out.typings[len(out.typings)-1], "off while the request waits")

	r.event(toAgent(`{"jsonrpc":"2.0","id":7,"result":{"outcome":{"outcome":"selected","optionId":"allow"}}}`))
	require.True(t, out.typings[len(out.typings)-1], "on again once the host answered")

	r.event(endOfTurn())
	require.False(t, out.typings[len(out.typings)-1])
	r.tick()
	n := len(out.typings)
	c.advance(time.Hour)
	r.tick()
	require.Len(t, out.typings, n, "nothing is renewed when no turn runs")
}

func permissionRequest(id, title string) session.Frame {
	return fromAgent(`{"jsonrpc":"2.0","id":` + id + `,"method":"session/request_permission","params":{"toolCall":{"toolCallId":"t","title":"` + title + `","rawInput":{"command":"rm -rf x"}},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"},{"optionId":"no","name":"Reject","kind":"reject_once"}]}}`)
}

func TestRendererAsksForAPermissionWithAReactionForEachOption(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(update("agent_message_chunk", `"content":{"type":"text","text":"Unfinished"}`))
	r.event(permissionRequest("7", "Run it"))

	require.Len(t, out.sent, 2)
	require.Equal(t, "Unfinished", out.sent[0].Body, "what is unsent goes first")
	ask := out.sent[1]
	require.Equal(t, Text, ask.Kind, "a request notifies")
	require.Contains(t, ask.Body, "Run it")
	require.Equal(t, &Fold{Summary: "details", Text: "rm -rf x", Code: true}, ask.Fold)
	require.ElementsMatch(t, []string{"m2 ✅", "m2 ❌"}, out.reactions)
}

func TestRendererTakesTheFirstOwnersReactionAsTheAnswer(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(permissionRequest("7", "Run it"))

	r.answerPermission("m1", "🚫", "@owner:x")
	r.answerPermission("other", "✅", "@owner:x")
	require.Empty(t, out.answers, "an option the request does not offer, and another message, answer nothing")

	r.answerPermission("m1", "❌", "@owner:x")
	r.answerPermission("m1", "✅", "@owner2:x")
	require.Equal(t, []string{"7 no"}, out.answers)
	require.Len(t, out.sent, 3)
	require.Equal(t, "Answered by @owner:x: Reject", out.sent[1].Body)
	require.Equal(t, MessageID("m1"), out.sent[1].ReplyTo)
	require.Equal(t, "The permission request is no longer open.", out.sent[2].Body, "the second owner's reaction is told it came late")
}

func TestRendererWithdrawsAPermissionNobodyAnswered(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(permissionRequest("7", "Run it"))
	r.event(endOfTurn())
	require.Equal(t, "The permission request was withdrawn.", out.sent[len(out.sent)-1].Body)
	require.Equal(t, MessageID("m1"), out.sent[len(out.sent)-1].ReplyTo)

	r.answerPermission("m1", "✅", "@owner:x")
	r.answerPermission("m1", "❌", "@owner:x")
	require.Empty(t, out.answers, "a request that is over takes no answer")
	require.Equal(t, "The permission request is no longer open.", out.sent[len(out.sent)-1].Body)
	require.Equal(t, MessageID("m1"), out.sent[len(out.sent)-1].ReplyTo)
	require.Len(t, out.sent, 3, "and is told so once")

	r.answerPermission("m1", "🚫", "@owner:x")
	require.Len(t, out.sent, 3, "a key the request never offered gets no word")
}

func TestRendererForgetsAClosedRequestAfterAWhile(t *testing.T) {
	r, out, c := newTestRenderer()
	r.event(permissionRequest("7", "Run it"))
	r.event(endOfTurn())
	c.advance(2 * closedFor)
	r.event(permissionRequest("8", "Run another"))

	n := len(out.sent)
	r.answerPermission("m1", "✅", "@owner:x")
	require.Len(t, out.sent, n)
}

func TestRendererDoesNotShowACommandTwiceInAPermissionRequest(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(permissionRequest("7", "rm -rf x"))
	require.Nil(t, out.sent[0].Fold)
	require.Equal(t, 1, strings.Count(out.sent[0].Body, "rm -rf x"))
}

func TestRendererSaysACallWasCancelledOrRefusedNotWhatTheAgentSaid(t *testing.T) {
	const boilerplate = "The user doesn't want to proceed with this tool use. STOP what you are doing."
	fail := update("tool_call_update", `"toolCallId":"t","status":"failed","content":[{"type":"content","content":{"type":"text","text":"`+boilerplate+`"}}]`)

	r, out, _ := newTestRenderer()
	r.event(update("tool_call_update", `"toolCallId":"t","title":"Bash","rawInput":{"command":"rm -rf x"}`))
	r.event(permissionRequest("7", "Run it"))
	r.event(toAgent(`{"jsonrpc":"2.0","id":7,"result":{"outcome":{"outcome":"cancelled"}}}`))
	r.event(fail)
	last := out.sent[len(out.sent)-1]
	require.Equal(t, "❌ cancelled", last.Body)
	require.Nil(t, last.Fold)

	r, out, _ = newTestRenderer()
	r.event(update("tool_call_update", `"toolCallId":"t","title":"Bash","rawInput":{"command":"rm -rf x"}`))
	r.event(permissionRequest("7", "Run it"))
	r.answerPermission("m2", "❌", "@owner:x")
	r.event(toAgent(`{"jsonrpc":"2.0","id":7,"result":{"outcome":{"outcome":"selected","optionId":"no"}}}`))
	r.event(fail)
	last = out.sent[len(out.sent)-1]
	require.Equal(t, "❌ refused", last.Body)
	require.Nil(t, last.Fold)

	r, out, _ = newTestRenderer()
	r.event(update("tool_call_update", `"toolCallId":"t","title":"Bash","rawInput":{"command":"false"}`))
	r.event(fail)
	require.Equal(t, "❌ failed", out.sent[len(out.sent)-1].Body, "a call that failed on its own is failed")
	require.NotNil(t, out.sent[len(out.sent)-1].Fold)
}

func TestRendererShowsGcsNudgeAsANoticeAndMarksAWaitingPrompt(t *testing.T) {
	r, out, _ := newTestRenderer()
	out.prompts[5] = "owner-message"

	r.event(session.Prompt{Seq: 4, Source: session.FromGC, Text: "mail arrived", State: session.Stored})
	require.Equal(t, Message{Kind: Notice, Body: "From gc: mail arrived"}, out.sent[0])

	r.event(session.Prompt{Seq: 5, Source: session.FromOwner, Text: "hello", State: session.Stored})
	require.Len(t, out.sent, 1, "the owner's own words are not posted again")

	r.event(session.Prompt{Seq: 6, State: session.Waiting, Of: 5})
	r.event(session.Prompt{Seq: 7, State: session.Waiting, Of: 4})
	require.Equal(t, []string{"owner-message ⏳", "m1 ⏳"}, out.reactions)

	r.event(session.Prompt{Seq: 8, State: session.Delivered, Of: 5})
	require.Equal(t, []ReactionID{"r1"}, out.undone)
	r.event(session.Prompt{Seq: 9, State: session.Delivered, Of: 4})
	require.Equal(t, []ReactionID{"r1", "r2"}, out.undone)
}

func TestRendererLeavesAPromptThatWentAtOnceUnmarked(t *testing.T) {
	r, out, _ := newTestRenderer()
	out.prompts[5] = "owner-message"
	r.event(session.Prompt{Seq: 5, Source: session.FromOwner, State: session.Stored})
	r.event(session.Prompt{Seq: 6, State: session.Delivered, Of: 5})
	require.Empty(t, out.reactions)
	require.Empty(t, out.undone)
}

func TestRendererTellsTheRoomWhenTheSessionBeginsAnew(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(update("agent_message_chunk", `"content":{"type":"text","text":"Half"}`))
	r.event(session.Lifecycle{Seq: 9, Kind: session.StartedAfresh})
	require.Equal(t, []string{"Half", startedAfresh}, bodies(out.sent))
}

func TestCutKeepsRunesWhole(t *testing.T) {
	require.Equal(t, "é\n…", cut("éé", 3))
	require.Equal(t, "abc", cut("abc\n", 10))
}

func TestUnfence(t *testing.T) {
	require.Equal(t, "ls -l\nx", unfence("```sh\nls -l\nx\n```"))
	require.Equal(t, "plain", unfence("  plain\n"))
	require.Equal(t, "```", unfence("```"))
}

func TestRendererEndsTheTurnWhenThePromptIsAnsweredWithAnError(t *testing.T) {
	r, out, c := newTestRenderer()
	r.event(toAgent(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{}}`))
	r.event(plan("pending"))
	r.event(plan("in_progress"))
	r.event(permissionRequest("7", "Run it"))
	require.False(t, out.typings[len(out.typings)-1])

	r.event(fromAgent(`{"jsonrpc":"2.0","id":3,"error":{"code":-32603,"message":"the model is overloaded"}}`))

	var got []string
	for _, m := range out.sent {
		got = append(got, m.Body)
	}
	require.Contains(t, got, "The permission request was withdrawn.")
	require.Equal(t, "The turn ended with an error: the model is overloaded", got[len(got)-1])
	require.Equal(t, "**Plan** 0/1\n\n- ▶ **step 1**", planEdit(out), "the plan's last edit is made")
	n := len(out.typings)
	c.advance(time.Hour)
	r.tick()
	require.Len(t, out.typings, n, "nothing is typed after the turn")
}

func planEdit(out *fakeOut) string {
	for i := len(out.sent) - 1; i >= 0; i-- {
		if out.sent[i].Replaces != "" {
			return out.sent[i].Body
		}
	}
	return ""
}

func TestRendererIgnoresAnErrorThatIsNotThePromptsAnswer(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(toAgent(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{}}`))
	r.event(fromAgent(`{"jsonrpc":"2.0","id":9,"error":{"code":-1,"message":"some other request failed"}}`))
	require.Empty(t, out.sent)
	require.True(t, out.typings[len(out.typings)-1])
}

func TestRendererLetsTheStopTheRoomAskedForEndTheWaiting(t *testing.T) {
	r, out, _ := newTestRenderer()
	out.prompts[5] = "owner-message"
	r.event(session.Prompt{Seq: 5, Source: session.FromOwner, State: session.Stored})
	r.event(session.Prompt{Seq: 6, State: session.Waiting, Of: 5})
	require.True(t, out.typings[len(out.typings)-1])

	r.stopped()
	require.False(t, out.typings[len(out.typings)-1], "no turn runs, so nothing is typed")
	require.Empty(t, out.undone, "the room does not reckon what was dropped: the server says")

	r.event(session.Prompt{Seq: 7, Source: session.FromOwner, State: session.Dropped, Of: 5})
	require.Equal(t, []ReactionID{"r1"}, out.undone, "the mark of a prompt that was dropped goes with its closing record")
	require.Equal(t, []uint64{5}, out.forgotten)
}

func TestRendererTakesNoMarkOffAPromptTheStopDidNotDrop(t *testing.T) {
	r, out, _ := newTestRenderer()
	out.prompts[5] = "owner-message"
	r.event(session.Prompt{Seq: 5, Source: session.FromOwner, State: session.Stored})
	r.event(session.Prompt{Seq: 6, State: session.Waiting, Of: 5})

	r.stopped() // gc's interrupt, or a stop that drops nothing, has no Dropped record
	require.Empty(t, out.undone, "a mark stays until the prompt is delivered or dropped")
	r.event(session.Prompt{Seq: 7, State: session.Delivered, Of: 5})
	require.Equal(t, []ReactionID{"r1"}, out.undone)
}

func TestRendererKeepsTheMarkOfGcsNudgeWhenTheOwnerStops(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(session.Prompt{Seq: 4, Source: session.FromGC, Text: "nudge", State: session.Stored})
	r.event(session.Prompt{Seq: 5, State: session.Waiting, Of: 4})
	require.Equal(t, []string{"m1 ⏳"}, out.reactions)

	r.stopped()
	require.Empty(t, out.undone, "the nudge is delivered after the stop, so it still waits")
	require.Empty(t, out.forgotten)
	r.event(session.Prompt{Seq: 6, State: session.Delivered, Of: 4})
	require.Equal(t, []ReactionID{"r1"}, out.undone)
}

func TestRendererClearsGcsNudgeMarkWhenTheSessionEndDropsIt(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(session.Prompt{Seq: 4, Source: session.FromGC, Text: "nudge", State: session.Stored})
	r.event(session.Prompt{Seq: 5, State: session.Waiting, Of: 4})
	require.Equal(t, []string{"m1 ⏳"}, out.reactions)

	r.event(session.Prompt{Seq: 6, Source: session.FromGC, State: session.Dropped, Of: 4})
	require.Equal(t, []ReactionID{"r1"}, out.undone, "a gc nudge's ⏳ goes with its Dropped record, as an owner's does")
	require.Equal(t, []uint64{4}, out.forgotten)
	r.event(session.Lifecycle{Seq: 7, Kind: session.Stopped})
	require.Equal(t, []ReactionID{"r1"}, out.undone)
}

func TestRendererShowsTheOutputOfAFailedToolCall(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(update("tool_call", `"toolCallId":"t1","title":"Run it","status":"pending","rawInput":{"command":"x"}`))
	r.event(update("tool_call_update", `"toolCallId":"t1","status":"failed","content":[{"type":"content","content":{"type":"text","text":"exit status 2"}}]`))
	require.Equal(t, "❌ failed", out.sent[1].Body)
	require.Equal(t, "exit status 2", out.sent[1].Fold.Text)
}

func TestRendererReactsInTheOptionsOrderWithAKeyForEachOption(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(fromAgent(`{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"toolCall":{"title":"Run it"},"options":[` +
		`{"optionId":"a","name":"Allow once","kind":"allow_once"},{"optionId":"b","name":"Allow this once too","kind":"allow_once"},` +
		`{"optionId":"c","name":"Reject","kind":"reject_once"}]}}`))
	require.Equal(t, []string{"m1 ✅", "m1 1️⃣", "m1 ❌"}, out.reactions)

	r.answerPermission("m1", "1️⃣", "@owner:x")
	require.Equal(t, []string{"7 b"}, out.answers)
}

func TestRendererForgetsAPromptOnceItIsDelivered(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(session.Prompt{Seq: 5, Source: session.FromOwner, State: session.Stored})
	r.event(session.Prompt{Seq: 6, State: session.Delivered, Of: 5})
	require.Equal(t, []uint64{5}, out.forgotten)
}

func TestRendererAsksNothingOfARequestTheHostAnsweredItself(t *testing.T) {
	r, out, _ := newTestRenderer()
	r.event(update("agent_message_chunk", `"content":{"type":"text","text":"Unfinished"}`))
	r.event(fromAgent(`{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"toolCall":{"toolCallId":"t","title":"attach","_meta":{"roomcity":{"answeredBy":"host"}}},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"}]}}`))

	require.Equal(t, []string{"Unfinished"}, bodies(out.sent), "the text before it is shown, and no question follows")
	require.Empty(t, out.reactions)
	r.event(update("tool_call", `"toolCallId":"t","title":"attach","status":"pending","rawInput":{"path":"a.png"}`))
	require.Equal(t, []string{"Unfinished", "🔧 **attach**"}, bodies(out.sent), "the tool call is shown as any other")

	r.event(permissionRequest("8", "Run it"))
	require.Contains(t, out.sent[len(out.sent)-1].Body, "Permission needed", "a request the host did not answer is still asked")
}
