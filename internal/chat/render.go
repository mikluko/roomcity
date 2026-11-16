package chat

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	waitingKey          = "⏳"
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
	prompts  map[uint64]promptNote
	working  bool
	typedAt  time.Time
	// promptID is the id of the request that began the turn, as JSON.
	promptID string
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
		prompts: map[uint64]promptNote{},

		toolLimit:   maxToolOutput,
		detailLimit: maxPermissionDetail,
	}
}

// drain shows what the stream left unsaid, as a turn's end does, but leaves
// the open requests as they are: the session may still be running.
func (r *renderer) drain() {
	r.endThoughts()
	r.sendAll(r.answer.flush())
	r.finishPlan()
}

// event takes the next event of the session.
func (r *renderer) event(ev session.Event) {
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
		r.sendAll(r.answer.feed(f))
		r.permissionRequest(m)
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
		r.sendAll(r.answer.feed(f))
	case "agent_thought_chunk":
		r.thinking.WriteString(chunkText(u))
	case "tool_call", "tool_call_update":
		r.sendAll(r.answer.feed(f))
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
	case session.Delivered:
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
	r.sendAll(r.answer.flush())
	r.finishPlan()
	for _, p := range r.perms {
		if !p.answered {
			p.answered = true
			r.send(Message{Kind: Notice, Body: "The permission request was withdrawn.", ReplyTo: p.message})
		}
	}
	r.perms = map[string]*permission{}
	r.byMsg = map[MessageID]*permission{}
	r.setWorking(false)
}

// stopped takes the stop that the room itself asked for as the end of the
// waiting: nothing is typed any longer, and the owner's prompts that waited
// were dropped with the turn, so their marks go. gc's nudges are kept and
// delivered after the stop, so theirs stay.
func (r *renderer) stopped() {
	r.setWorking(false)
	for seq, note := range r.prompts {
		if note.source == session.FromGC {
			continue
		}
		if note.waiting != "" {
			r.out.unreact(note.waiting)
		}
		delete(r.prompts, seq)
		r.out.forgetPrompt(seq)
	}
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
	output, diff        string
	started             MessageID
	limit               int
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
	if t.started == "" && (toolKnown(u) || ended) {
		t.started = r.send(Message{Kind: Notice, Body: "🔧 **" + t.label() + "**"})
	}
	if ended && t.started != "" {
		r.send(t.result())
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

func (t *tool) merge(u map[string]any) {
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
}

// finishPlan makes the plan's last edit, if it has one waiting, and forgets
// the plan: the next turn's is a new notice.
func (r *renderer) finishPlan() {
	if r.planned.dirty {
		r.editPlan()
	}
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
	message   MessageID
	keys      []string                    // the reactions' keys, in the options' order
	options   map[string]permissionOption // by the reaction's key
	answered  bool
}

type permissionOption struct{ id, name string }

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

func (r *renderer) permissionRequest(m rpc) {
	title, _ := m.Params.ToolCall["title"].(string)
	p := &permission{requestID: string(m.ID), options: map[string]permissionOption{}}
	var labels []string
	for _, o := range m.Params.Options {
		key := p.freeKey(permissionKey(o.Kind))
		p.keys = append(p.keys, key)
		p.options[key] = permissionOption{id: o.ID, name: o.Name}
		labels = append(labels, key+" "+o.Name)
	}
	msg := Message{
		Kind: Text,
		Body: fmt.Sprintf("🔐 **Permission needed:** `%s`\n\nReact to answer: %s", title, strings.Join(labels, "   ")),
	}
	if d := permissionDetail(m.Params.ToolCall, r.detailLimit); d != "" {
		msg.Fold = &Fold{Summary: "details", Text: d, Code: true}
	}
	r.setWorking(false)
	p.message = r.send(msg)
	for _, key := range p.keys {
		r.out.react(p.message, key)
	}
	r.perms[p.requestID] = p
	r.byMsg[p.message] = p
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
	if p == nil || p.answered {
		return
	}
	opt, ok := p.options[key]
	if !ok {
		return
	}
	p.answered = true
	r.out.answerPermission(p.requestID, opt.id)
	r.send(Message{Kind: Notice, Body: fmt.Sprintf("Answered by %s: %s", by, opt.name), ReplyTo: p.message})
}
