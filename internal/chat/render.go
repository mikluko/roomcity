package chat

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sarak.sh/mikluko/roomcity/internal/roomtool"
	"sarak.sh/mikluko/roomcity/internal/session"
)

// outbox is what a renderer does to the room it writes in.
type outbox interface {
	send(Message) MessageID
	react(to MessageID, key string) ReactionID
	unreact(ReactionID)
	typing(on bool)
	// promptMessage returns the room's message that the owner's prompt of
	// that Seq came from, or "" if there is none.
	promptMessage(seq uint64) MessageID
	// forgetPrompt says the prompt of that Seq needs its message no more.
	forgetPrompt(seq uint64)
	answerPermission(requestID, optionID string)
}

const (
	// planEvery is the shortest gap between two edits of the plan notice.
	planEvery = 1200 * time.Millisecond
	// typingRenew is how long the homeserver is left to keep a typing notice
	// before it is sent again.
	typingRenew = 20 * time.Second
	// maxToolOutput is the part of a tool call's output the room shows.
	maxToolOutput = 4000
	// maxPermissionDetail is the part of a diff a permission request shows.
	maxPermissionDetail = 1500
	// maxRoomTask is the part of a task's first line a work notice shows.
	maxRoomTask = 160
	// closedFor is how long a reaction on a permission request that is over
	// is still told so.
	closedFor  = time.Hour
	waitingKey = "⏳"
)

// renderer turns the events of one session into what the room shows. It is
// not safe for concurrent use.
type renderer struct {
	out  outbox
	now  func() time.Time
	plan time.Duration

	answer   answer
	thinking strings.Builder
	tools    map[string]*tool
	planned  planState
	perms    map[string]*permission
	byMsg    map[MessageID]*permission
	// denied is, by the id of a tool call, why a permission request for it
	// ended without a yes: "cancelled" or "refused".
	denied  map[string]string
	prompts map[uint64]promptNote
	working bool
	typedAt time.Time
	// promptID is the id of the request that began the turn, as JSON.
	promptID string
	// at is the Seq of the event being handled; the three since-fields are the
	// Seq of the first event of what is held back unsent: the answer's last
	// paragraph, the thoughts, and the plan's edit. 0 is nothing held.
	at                                   uint64
	answerSince, thoughtSince, planSince uint64
	// toolLimit and detailLimit are how many bytes of a tool call's output
	// and of a permission request's detail are shown; 0 shows all.
	toolLimit, detailLimit int
}

type promptNote struct {
	source  session.Source
	message MessageID
	waiting ReactionID
}

func newRenderer(out outbox, now func() time.Time, planGap time.Duration) *renderer {
	if planGap == 0 {
		planGap = planEvery
	}
	return &renderer{
		out:     out,
		now:     now,
		plan:    planGap,
		tools:   map[string]*tool{},
		perms:   map[string]*permission{},
		byMsg:   map[MessageID]*permission{},
		denied:  map[string]string{},
		prompts: map[uint64]promptNote{},

		toolLimit:   maxToolOutput,
		detailLimit: maxPermissionDetail,
	}
}

// drain shows what the stream left unsaid, as a turn's end does, but leaves
// the open requests as they are: the session may still be running.
func (r *renderer) drain() {
	r.endThoughts()
	r.sendAll(r.flushAnswer())
	r.finishPlan()
}

// safe returns the last Seq, of the events handled up to last, after which a
// restart that takes the stream up shows nothing twice and loses nothing: the
// one before the oldest event whose text is still held back, or last itself
// when nothing is. The open tool calls and permission requests do not hold it
// back, since a result with no start, and an answer to a request the room
// never saw, are both handled.
func (r *renderer) safe(last uint64) uint64 {
	oldest := uint64(0)
	for _, since := range []uint64{r.answerSince, r.thoughtSince, r.planSince} {
		if since != 0 && (oldest == 0 || since < oldest) {
			oldest = since
		}
	}
	if oldest == 0 {
		return last
	}
	return oldest - 1
}

// feedAnswer gives the event to the answer and returns the paragraphs it
// completed. What is left of the answer began in this event when the event
// completed a paragraph, and earlier when it did not.
func (r *renderer) feedAnswer(ev session.Event) []string {
	out := r.answer.feed(ev)
	switch {
	case r.answer.pending == "":
		r.answerSince = 0
	case len(out) > 0 || r.answerSince == 0:
		r.answerSince = r.at
	}
	return out
}

// flushAnswer returns what is left of the answer, and holds none.
func (r *renderer) flushAnswer() []string {
	out := r.answer.flush()
	r.answerSince = 0
	return out
}

// event takes the next event of the session.
func (r *renderer) event(ev session.Event) {
	r.at = ev.Number()
	switch ev := ev.(type) {
	case session.Frame:
		r.frame(ev)
	case session.Prompt:
		r.prompt(ev)
	case session.Lifecycle:
		r.lifecycle(ev)
	}
}

func (r *renderer) frame(f session.Frame) {
	var m rpc
	if json.Unmarshal(f.Message, &m) != nil {
		return
	}
	switch {
	case f.Direction == session.ToAgent && m.Method == "session/prompt":
		r.promptID = string(m.ID)
		r.setWorking(true)
	case f.Direction == session.ToAgent && m.Method == "" && r.perms[string(m.ID)] != nil:
		r.permissionAnswered(m)
	case f.Direction != session.FromAgent:
	case m.Method == "session/update":
		r.update(f, m)
	case m.Method == "session/request_permission":
		r.endThoughts()
		r.sendAll(r.feedAnswer(f))
		if !answeredByHost(m) {
			r.flushPlan()
			r.permissionRequest(m)
		}
	case m.Method == "" && m.Result != nil && m.Result.StopReason != "":
		r.endTurn()
	case m.Method == "" && m.Error != nil && r.promptID != "" && string(m.ID) == r.promptID:
		r.endTurn()
		r.send(Message{Kind: Notice, Body: "The turn ended with an error: " + m.Error.Message})
	}
}

// rpc is the part of a JSON-RPC message the renderer reads.
type rpc struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Update   map[string]any `json:"update"`
		ToolCall map[string]any `json:"toolCall"`
		Options  []struct {
			ID   string `json:"optionId"`
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"options"`
	} `json:"params"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Result *struct {
		StopReason string `json:"stopReason"`
		Outcome    struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	} `json:"result"`
}

func (r *renderer) update(f session.Frame, m rpc) {
	u := m.Params.Update
	kind, _ := u["sessionUpdate"].(string)
	if kind != "agent_thought_chunk" && isRunBreak(kind) {
		r.endThoughts()
	}
	switch kind {
	case "agent_message_chunk":
		r.sendAll(r.feedAnswer(f))
	case "agent_thought_chunk":
		if r.thinking.Len() == 0 {
			r.thoughtSince = r.at
		}
		r.thinking.WriteString(chunkText(u))
	case "tool_call", "tool_call_update":
		r.sendAll(r.feedAnswer(f))
		r.flushPlan()
		r.toolUpdate(u)
	case "plan":
		r.planUpdate(u)
	}
}

// isRunBreak says whether an update ends a run of thoughts. Updates that are
// bookkeeping, usage among them, do not.
func isRunBreak(kind string) bool {
	switch kind {
	case "agent_message_chunk", "tool_call", "tool_call_update", "plan":
		return true
	}
	return false
}

func chunkText(u map[string]any) string {
	c, _ := u["content"].(map[string]any)
	s, _ := c["text"].(string)
	return s
}

func (r *renderer) sendAll(texts []string) {
	for _, t := range texts {
		r.send(Message{Kind: Text, Body: t})
	}
}

// send sends a message and, while a turn is on, sends the typing notice again,
// in case the homeserver ends a user's notice when the user speaks.
func (r *renderer) send(m Message) MessageID {
	id := r.out.send(m)
	if r.working {
		r.typed(true)
	}
	return id
}

func (r *renderer) typed(on bool) {
	r.out.typing(on)
	r.typedAt = r.now()
}

func (r *renderer) setWorking(on bool) {
	if r.working == on {
		return
	}
	r.working = on
	r.typed(on)
}

func (r *renderer) endThoughts() {
	text := strings.TrimSpace(r.thinking.String())
	r.thinking.Reset()
	r.thoughtSince = 0
	if text == "" {
		return
	}
	r.send(Message{Kind: Notice, Fold: &Fold{Summary: "💭 Thinking", Text: text}})
}

// prompt shows a prompt's journey: gc's as a notice, and the ⏳ on one that
// waits for the end of the running turn.
func (r *renderer) prompt(p session.Prompt) {
	switch p.State {
	case session.Stored:
		note := promptNote{source: p.Source}
		if p.Source == session.FromGC {
			note.message = r.send(Message{Kind: Notice, Body: "From gc: " + p.Text})
		} else {
			r.setWorking(true)
		}
		r.prompts[p.Seq] = note
	case session.Waiting:
		note, ok := r.prompts[p.Of]
		if !ok {
			return
		}
		if note.source == session.FromOwner {
			note.message = r.out.promptMessage(p.Of)
		}
		if note.message != "" {
			note.waiting = r.out.react(note.message, waitingKey)
		}
		r.prompts[p.Of] = note
	case session.Delivered, session.Dropped:
		note, ok := r.prompts[p.Of]
		if !ok {
			return
		}
		if note.waiting != "" {
			r.out.unreact(note.waiting)
		}
		delete(r.prompts, p.Of)
		r.out.forgetPrompt(p.Of)
	}
}

func (r *renderer) lifecycle(l session.Lifecycle) {
	switch l.Kind {
	case session.Stopped:
		r.endTurn()
	case session.StartedAfresh:
		r.endTurn()
		r.send(Message{Kind: Notice, Body: startedAfresh})
	}
}

// endTurn shows what a turn left unsaid and closes what it left open.
func (r *renderer) endTurn() {
	r.endThoughts()
	r.sendAll(r.flushAnswer())
	r.finishPlan()
	for _, p := range r.perms {
		if !p.answered {
			p.answered = true
			r.denied[p.tool] = "cancelled"
			r.send(Message{Kind: Notice, Body: "The permission request was withdrawn.", ReplyTo: p.message})
		}
	}
	r.perms = map[string]*permission{}
	r.setWorking(false)
}

// stopped takes the stop that the room itself asked for as the end of the
// waiting: nothing is typed any longer. The marks of the prompts the stop
// dropped go when the server closes each with a Dropped record, and those of
// gc's nudges, which are kept, when they are Delivered.
func (r *renderer) stopped() {
	r.setWorking(false)
}

// tick does what is due with time: the plan's edit and the typing notice's
// renewal.
func (r *renderer) tick() {
	if r.planned.dirty && r.now().Sub(r.planned.at) >= r.plan {
		r.editPlan()
	}
	if r.working && r.now().Sub(r.typedAt) >= typingRenew {
		r.typed(true)
	}
}

// ---- tool calls ----

type tool struct {
	title, kind, status string
	name                string
	work                string
	input               map[string]any
	output, diff        string
	// denial is "cancelled" or "refused" when the call ended without its
	// permission, and its output is the agent's own account of that.
	denial  string
	started MessageID
	limit   int
}

func (r *renderer) toolUpdate(u map[string]any) {
	id, _ := u["toolCallId"].(string)
	t := r.tools[id]
	if t == nil {
		t = &tool{status: "pending", limit: r.toolLimit}
		r.tools[id] = t
	}
	t.merge(u)
	ended := t.status == "completed" || t.status == "failed"
	if ended {
		t.denial = r.denied[id]
		delete(r.denied, id)
	}
	if t.work == "" && t.started == "" && (toolKnown(u) || ended) {
		t.started = r.send(Message{Kind: Notice, Body: "🔧 **" + t.label() + "**"})
	}
	if ended {
		if t.work != "" {
			r.send(t.workResult())
		} else if t.started != "" {
			r.send(t.result())
		}
		delete(r.tools, id)
	}
}

// hasOutput says whether the call has ended and so has its output.
func (t *tool) hasOutput() bool { return t.status == "completed" || t.status == "failed" }

func (t *tool) label() string {
	if t.title != "" {
		return t.title
	}
	return t.name
}

// roomWorkTools maps the two agents' metadata names to the room's work tools.
// A display title is not evidence of a tool's name or server.
var roomWorkTools = map[string]string{
	"mcp__room__dispatch": "dispatch",
	"mcp__room__wait":     "wait",
	"mcp__room__result":   "result",
	"mcp__room__claim":    "claim",
	"mcp__room__done":     "done",
	"room__dispatch":      "dispatch",
	"room__wait":          "wait",
	"room__result":        "result",
	"room__claim":         "claim",
	"room__done":          "done",
}

func roomWorkTool(u map[string]any) string {
	meta, _ := u["_meta"].(map[string]any)
	cc, _ := meta["claudeCode"].(map[string]any)
	name, _ := cc["toolName"].(string)
	if work := roomWorkTools[name]; work != "" {
		return work
	}
	goose, _ := meta["goose"].(map[string]any)
	call, _ := goose["toolCall"].(map[string]any)
	name, _ = call["toolName"].(string)
	return roomWorkTools[name]
}

func (t *tool) merge(u map[string]any) {
	if work := roomWorkTool(u); work != "" {
		t.work = work
	}
	if input, ok := u["rawInput"].(map[string]any); ok && len(input) > 0 {
		t.input = input
	}
	if v, ok := u["title"].(string); ok && v != "" {
		t.title = v
	}
	if v, ok := u["kind"].(string); ok {
		t.kind = v
	}
	if v, ok := u["status"].(string); ok {
		t.status = v
	}
	if meta, ok := u["_meta"].(map[string]any); ok {
		if cc, ok := meta["claudeCode"].(map[string]any); ok {
			if n, ok := cc["toolName"].(string); ok {
				t.name = n
			}
		}
	}
	items, _ := u["content"].([]any)
	for _, c := range items {
		t.mergeContent(c)
	}
	if t.output == "" && t.hasOutput() {
		if ro, ok := u["rawOutput"].(string); ok {
			t.output = ro
		}
	}
}

func (t *tool) mergeContent(c any) {
	cm, _ := c.(map[string]any)
	switch cm["type"] {
	case "diff":
		t.diff = diffText(cm)
	case "content":
		inner, _ := cm["content"].(map[string]any)
		if txt, ok := inner["text"].(string); ok && t.hasOutput() {
			t.output = txt
		}
	}
}

// toolKnown reports whether an update carries the call's input or content,
// which is when its start is worth a notice.
func toolKnown(u map[string]any) bool {
	if ri, ok := u["rawInput"].(map[string]any); ok && len(ri) > 0 {
		return true
	}
	cs, _ := u["content"].([]any)
	return len(cs) > 0
}

// result is the notice of a tool call's end, in reply to its start.
func (t *tool) result() Message {
	mark, word := "✅", "done"
	if t.status == "failed" {
		mark, word = "❌", "failed"
	}
	if t.denial != "" {
		return Message{Kind: Notice, Body: "❌ " + t.denial, ReplyTo: t.started}
	}
	m := Message{Kind: Notice, Body: mark + " " + word, ReplyTo: t.started}
	detail := t.diff
	if detail == "" {
		detail = unfence(t.output)
	}
	if detail != "" {
		m.Fold = &Fold{Summary: "output", Text: cut(detail, t.limit), Code: true}
	}
	return m
}

// workResult says what a completed room work tool did, or which act failed.
func (t *tool) workResult() Message {
	m := t.result()
	if t.status == "failed" || t.denial != "" {
		m.Body = "❌ " + t.workFailure()
		return m
	}
	output := unfence(t.output)
	switch t.work {
	case "dispatch":
		m.Body = "Handed to a worker"
		if task := taskLine(t.argument("task"), maxRoomTask); task != "" {
			m.Body += ": " + task
		}
		if id := roomtool.DispatchedTask(output); id != "" {
			m.Body += " (task " + id + ")"
		}
		m.Fold = nil
	case "wait":
		m.Body = "Waiting to be told when " + taskRef(t.argument("task")) + " is closed"
		m.Fold = nil
	case "result":
		m.Body = "Read the result of " + taskRef(t.argument("task"))
	case "claim":
		if output == roomtool.NoTaskAnswer {
			m.Body = "No task to take; its session is ending"
			m.Fold = nil
			break
		}
		id, task, ok := roomtool.ClaimedTask(output)
		if !ok {
			m.Body = "❌ Could not take a task"
			return m
		}
		m.Body = "Took task " + id + ": " + taskLine(task, maxRoomTask)
		m.Fold = nil
	case "done":
		m.Body = "Closed its task with the result"
		m.Fold = nil
		if result := t.argument("result"); result != "" {
			m.Fold = &Fold{Summary: "output", Text: cut(result, t.limit), Code: true}
		}
	}
	return m
}

func (t *tool) workFailure() string {
	switch t.work {
	case "dispatch":
		body := "Could not hand to a worker"
		if task := taskLine(t.argument("task"), maxRoomTask); task != "" {
			body += ": " + task
		}
		return body
	case "wait":
		return "Could not wait for " + taskRef(t.argument("task"))
	case "result":
		return "Could not read the result of " + taskRef(t.argument("task"))
	case "claim":
		return "Could not take a task"
	case "done":
		return "Could not close its task"
	default:
		return "The tool call failed"
	}
}

func (t *tool) argument(name string) string {
	value, _ := t.input[name].(string)
	return value
}

func taskRef(id string) string {
	if id == "" {
		return "the task"
	}
	return "task " + id
}

func diffText(cm map[string]any) string {
	oldT, _ := cm["oldText"].(string)
	newT, _ := cm["newText"].(string)
	path, _ := cm["path"].(string)
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", path, path)
	for _, l := range strings.Split(strings.TrimRight(oldT, "\n"), "\n") {
		b.WriteString("-" + l + "\n")
	}
	for _, l := range strings.Split(strings.TrimRight(newT, "\n"), "\n") {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

// unfence drops a Markdown code fence around a tool's output.
func unfence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") || !strings.HasSuffix(s, "```") || len(s) < 6 {
		return s
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "```"), "```")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// taskLine keeps the first line of a task, cut at a rune boundary.
func taskLine(s string, limit int) string {
	line, _, _ := strings.Cut(s, "\n")
	line = strings.TrimSpace(line)
	if limit <= 0 || len(line) <= limit {
		return line
	}
	for limit > 0 && !isRuneStart(line[limit]) {
		limit--
	}
	return strings.TrimSpace(line[:limit]) + "…"
}

// cut keeps the first n bytes of s, at a rune boundary, marking the cut; with
// n of 0 it keeps all.
func cut(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if n <= 0 || len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n] + "\n…"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// ---- the plan ----

type planState struct {
	entries []planEntry
	message MessageID
	dirty   bool
	at      time.Time
}

type planEntry struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

func (r *renderer) planUpdate(u map[string]any) {
	raw, err := json.Marshal(u["entries"])
	if err != nil {
		return
	}
	var entries []planEntry
	if json.Unmarshal(raw, &entries) != nil {
		return
	}
	r.planned.entries = entries
	if !r.planned.dirty {
		r.planSince = r.at
	}
	r.planned.dirty = true
	if r.planned.message == "" {
		r.editPlan()
	}
}

// editPlan sends the plan as it is, as a new notice the first time and as an
// edit of it after.
func (r *renderer) editPlan() {
	p := &r.planned
	m := Message{Kind: Notice, Body: planBody(p.entries), Replaces: p.message}
	if id := r.send(m); p.message == "" {
		p.message = id
	}
	p.dirty = false
	p.at = r.now()
	r.planSince = 0
}

// flushPlan makes the plan's edit that waits, if there is one, now: a tool
// notice or a permission request that follows is not shown without it, and a
// restart between the two would show them again.
func (r *renderer) flushPlan() {
	if r.planned.dirty {
		r.editPlan()
	}
}

// finishPlan makes the plan's last edit, if it has one waiting, and forgets
// the plan: the next turn's is a new notice.
func (r *renderer) finishPlan() {
	r.flushPlan()
	r.planned = planState{}
}

func planBody(entries []planEntry) string {
	done := 0
	for _, e := range entries {
		if e.Status == "completed" {
			done++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Plan** %d/%d\n\n", done, len(entries))
	for _, e := range entries {
		switch e.Status {
		case "completed":
			fmt.Fprintf(&b, "- ☑ ~~%s~~\n", e.Content)
		case "in_progress":
			fmt.Fprintf(&b, "- ▶ **%s**\n", e.Content)
		default:
			fmt.Fprintf(&b, "- ☐ %s\n", e.Content)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---- permission requests ----

type permission struct {
	requestID string
	tool      string // the id of the tool call it is for
	message   MessageID
	at        time.Time
	told      bool                        // a reaction after its end has been answered
	keys      []string                    // the reactions' keys, in the options' order
	options   map[string]permissionOption // by the reaction's key
	answered  bool
}

type permissionOption struct{ id, name, kind string }

// spareKeys are the reactions for options whose kind's key another option
// has taken.
var spareKeys = []string{"1️⃣", "2️⃣", "3️⃣", "4️⃣", "5️⃣", "6️⃣", "7️⃣", "8️⃣", "9️⃣"}

func permissionKey(kind string) string {
	switch kind {
	case "allow_once":
		return "✅"
	case "allow_always":
		return "♾️"
	case "reject_once":
		return "❌"
	case "reject_always":
		return "🚫"
	}
	return "❔"
}

// answeredByHost says whether the host answered the request itself, as it
// does for the agent's leave to use the room's own tools: the copy that reaches
// the server says so in the call's _meta, and the room has nothing to ask.
func answeredByHost(m rpc) bool {
	meta, _ := m.Params.ToolCall["_meta"].(map[string]any)
	roomcity, _ := meta["roomcity"].(map[string]any)
	by, _ := roomcity["answeredBy"].(string)
	return by == "host"
}

func (r *renderer) permissionRequest(m rpc) {
	title, _ := m.Params.ToolCall["title"].(string)
	p := &permission{requestID: string(m.ID), options: map[string]permissionOption{}, at: r.now()}
	p.tool, _ = m.Params.ToolCall["toolCallId"].(string)
	var labels []string
	for _, o := range m.Params.Options {
		key := p.freeKey(permissionKey(o.Kind))
		p.keys = append(p.keys, key)
		p.options[key] = permissionOption{id: o.ID, name: o.Name, kind: o.Kind}
		labels = append(labels, key+" "+o.Name)
	}
	msg := Message{
		Kind: Text,
		Body: fmt.Sprintf("🔐 **Permission needed:** `%s`\n\nReact to answer: %s", title, strings.Join(labels, "   ")),
	}
	if d := permissionDetail(m.Params.ToolCall, r.detailLimit); d != "" && !strings.Contains(title, d) {
		msg.Fold = &Fold{Summary: "details", Text: d, Code: true}
	}
	r.setWorking(false)
	p.message = r.send(msg)
	for _, key := range p.keys {
		r.out.react(p.message, key)
	}
	r.perms[p.requestID] = p
	r.byMsg[p.message] = p
	r.forgetClosed()
}

// forgetClosed drops the requests that have been over for closedFor.
func (r *renderer) forgetClosed() {
	for msg, p := range r.byMsg {
		if p.answered && r.now().Sub(p.at) > closedFor {
			delete(r.byMsg, msg)
		}
	}
}

// optionName returns the name of the option with the id, or the id.
func (p *permission) optionName(id string) string {
	for _, o := range p.options {
		if o.id == id {
			return o.name
		}
	}
	return id
}

func (p *permission) optionKind(id string) string {
	for _, o := range p.options {
		if o.id == id {
			return o.kind
		}
	}
	return ""
}

// freeKey returns key if no option has it yet, and else the first spare key
// that none has.
func (p *permission) freeKey(key string) string {
	for _, spare := range spareKeys {
		if _, taken := p.options[key]; !taken {
			return key
		}
		key = spare
	}
	return key
}

func permissionDetail(call map[string]any, limit int) string {
	items, _ := call["content"].([]any)
	for _, c := range items {
		if cm, _ := c.(map[string]any); cm["type"] == "diff" {
			return cut(diffText(cm), limit)
		}
	}
	if ri, ok := call["rawInput"].(map[string]any); ok {
		if cmd, ok := ri["command"].(string); ok {
			return cmd
		}
	}
	return ""
}

// permissionAnswered sees the host's answer to a permission request go to the
// agent, whoever it came from, and takes the turn up again. A request that
// ends cancelled, without an answer from the room, was withdrawn.
func (r *renderer) permissionAnswered(m rpc) {
	p := r.perms[string(m.ID)]
	switch {
	case m.Result == nil:
	case m.Result.Outcome.Outcome == "cancelled":
		r.denied[p.tool] = "cancelled"
	case strings.HasPrefix(p.optionKind(m.Result.Outcome.OptionID), "reject"):
		r.denied[p.tool] = "refused"
	}
	if !p.answered && m.Result != nil {
		switch {
		case m.Result.Outcome.Outcome == "cancelled":
			r.send(Message{Kind: Notice, Body: "The permission request was withdrawn.", ReplyTo: p.message})
		case m.Result.Outcome.OptionID != "":
			r.send(Message{Kind: Notice, Body: "Answered: " + p.optionName(m.Result.Outcome.OptionID), ReplyTo: p.message})
		}
	}
	p.answered = true
	r.setWorking(true)
}

// answerPermission takes a reaction on a permission request as its answer, if
// it is the first. The caller has checked that the sender holds the owner's
// role.
func (r *renderer) answerPermission(to MessageID, key string, by UserID) {
	p := r.byMsg[to]
	if p == nil {
		return
	}
	opt, ok := p.options[key]
	if !ok {
		return
	}
	if p.answered {
		if !p.told {
			p.told = true
			r.send(Message{Kind: Notice, Body: "The permission request is no longer open.", ReplyTo: p.message})
		}
		return
	}
	p.answered = true
	r.out.answerPermission(p.requestID, opt.id)
	r.send(Message{Kind: Notice, Body: fmt.Sprintf("Answered by %s: %s", by, opt.name), ReplyTo: p.message})
}
