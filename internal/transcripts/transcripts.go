// Package transcripts serves the transcript of a session as a page: the
// session's card and its stream as a room shows it, but whole, and the raw
// stream as it passed through the server.
//
// The page opens no door of its own. It is mounted on the server's page
// listener, which only the pod's authenticating proxy reaches, and the proxy
// lets in whoever Pocket ID says is in the group admin.
package transcripts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"github.com/yuin/goldmark"

	"sarak.sh/mikluko/roomcity/internal/chat"
	"sarak.sh/mikluko/roomcity/internal/session"
)

// Prefix is where the pages are: /-/<session> for a session's transcript,
// /-/<session>/raw for its stream as it was, and /-/ for the sessions.
const Prefix = "/-/"

// Source is where the transcripts are kept.
type Source interface {
	List(ctx context.Context) ([]session.Info, error)
	Transcript(ctx context.Context, name string) (session.Card, []session.Entry, error)
}

// Page serves the transcripts.
type Page struct {
	Source Source
	// City is the city's name, shown in the page's title.
	City string
}

// Handler returns the handler of the pages, to be mounted at Prefix.
func (p *Page) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+Prefix+"{$}", p.index)
	mux.HandleFunc("GET "+Prefix+"{session}", p.transcript)
	mux.HandleFunc("GET "+Prefix+"{session}/raw", p.raw)
	return noStore(mux)
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (p *Page) index(w http.ResponseWriter, r *http.Request) {
	infos, err := p.Source.List(r.Context())
	if err != nil {
		http.Error(w, "the sessions could not be listed", http.StatusInternalServerError)
		return
	}
	render(w, indexPage, struct {
		City     string
		Sessions []session.Info
	}{p.City, infos})
}

func (p *Page) transcript(w http.ResponseWriter, r *http.Request) {
	card, entries, ok := p.load(w, r)
	if !ok {
		return
	}
	type shown struct {
		Time    string
		From    string
		Kind    string
		Body    template.HTML
		Summary string
		Fold    string
		Code    bool
		HasFold bool
	}
	var items []shown
	for _, it := range chat.Transcript(entries) {
		s := shown{Time: it.Time.UTC().Format("15:04:05"), From: it.From, Kind: "text", Body: markdown(it.Message.Body)}
		if it.Message.Kind == chat.Notice {
			s.Kind = "notice"
		}
		if f := it.Message.Fold; f != nil {
			s.HasFold, s.Summary, s.Fold, s.Code = true, f.Summary, f.Text, f.Code
		}
		items = append(items, s)
	}
	render(w, transcriptPage, struct {
		City  string
		Card  session.Card
		State string
		Began string
		Ended string
		Items []shown
	}{p.City, card, stateName(card.State), formatTime(card.Began), formatTime(card.Ended), items})
}

func (p *Page) load(w http.ResponseWriter, r *http.Request) (session.Card, []session.Entry, bool) {
	card, entries, err := p.Source.Transcript(r.Context(), r.PathValue("session"))
	switch {
	case errors.Is(err, session.ErrNotFound):
		http.Error(w, "no such session, or its transcript has been removed", http.StatusNotFound)
		return card, nil, false
	case err != nil:
		http.Error(w, "the transcript could not be read", http.StatusInternalServerError)
		return card, nil, false
	}
	return card, entries, true
}

// raw is the stream as it was, a JSON object on each line.
func (p *Page) raw(w http.ResponseWriter, r *http.Request) {
	_, entries, ok := p.load(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="`+url.PathEscape(r.PathValue("session"))+`.jsonl"`)
	enc := json.NewEncoder(w)
	for _, e := range entries {
		_ = enc.Encode(line(e))
	}
}

// rawLine is one entry of the raw stream.
type rawLine struct {
	Time      time.Time       `json:"time"`
	Seq       uint64          `json:"seq"`
	Type      string          `json:"type"`
	Direction string          `json:"direction,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
	Source    string          `json:"source,omitempty"`
	State     string          `json:"state,omitempty"`
	Text      string          `json:"text,omitempty"`
	Of        uint64          `json:"of,omitempty"`
	Kind      string          `json:"kind,omitempty"`
}

func line(e session.Entry) rawLine {
	l := rawLine{Time: e.Time}
	switch ev := e.Event.(type) {
	case session.Frame:
		l.Seq, l.Type, l.Direction = ev.Seq, "frame", "to_agent"
		if ev.Direction == session.FromAgent {
			l.Direction = "from_agent"
		}
		if json.Valid(ev.Message) {
			l.Message = ev.Message
		} else {
			l.Message, _ = json.Marshal(string(ev.Message))
		}
	case session.Prompt:
		l.Seq, l.Type, l.Text, l.Of = ev.Seq, "prompt", ev.Text, ev.Of
		l.Source = map[session.Source]string{session.FromOwner: "owner", session.FromGC: "gc"}[ev.Source]
		l.State = map[session.PromptState]string{session.Stored: "stored", session.Waiting: "waiting", session.Delivered: "delivered", session.Dropped: "dropped"}[ev.State]
	case session.Lifecycle:
		l.Seq, l.Type = ev.Seq, "lifecycle"
		l.Kind = map[session.LifecycleKind]string{session.Started: "started", session.StartedAfresh: "started_afresh", session.Stopped: "stopped"}[ev.Kind]
	}
	return l
}

var md = goldmark.New()

// markdown renders text as HTML. Raw HTML in the text is escaped, and a link
// to a script is dropped: the text is an agent's.
func markdown(text string) template.HTML {
	var b bytes.Buffer
	if err := md.Convert([]byte(text), &b); err != nil {
		return template.HTML(template.HTMLEscapeString(text))
	}
	return template.HTML(b.String()) //nolint:gosec // goldmark's output, with raw HTML escaped
}

func stateName(s session.State) string {
	switch s {
	case session.Starting:
		return "starting"
	case session.Running:
		return "running"
	case session.Ended:
		return "ended"
	}
	return ""
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

func render(w http.ResponseWriter, t *template.Template, data any) {
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		http.Error(w, "the page could not be made", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b.Bytes())
}

const style = `<style>
body{font:15px/1.5 system-ui,sans-serif;max-width:56rem;margin:2rem auto;padding:0 1rem;color:#222}
table.card td{padding:0 1rem 0 0}table.card td:first-child{color:#666}
.item{margin:.6rem 0;padding:.4rem .8rem;border-left:3px solid #bbb}
.item.owner{border-color:#36c;background:#f1f5ff}.item.notice{color:#555;font-size:.92em}
.time{color:#888;font-size:.8em}pre{background:#f6f6f6;padding:.5rem;overflow:auto}
summary{cursor:pointer;color:#555}
</style>`

var pathFunc = template.FuncMap{
	"path": func(s string) string { return Prefix + url.PathEscape(s) },
}

var indexPage = template.Must(template.New("index").Funcs(pathFunc).Parse(`<!doctype html><html><head><meta charset="utf-8"><title>{{.City}}: sessions</title>` + style + `</head><body>
<h1>{{.City}}: sessions</h1>
<ul>{{range .Sessions}}<li><a href="{{path .Name}}">{{.Name}}</a> ({{.Agent}})</li>{{end}}</ul>
</body></html>`))

var transcriptPage = template.Must(template.New("transcript").Funcs(pathFunc).Parse(`<!doctype html><html><head><meta charset="utf-8"><title>{{.Card.Name}}: transcript</title>` + style + `</head><body>
<h1>{{.Card.Name}}</h1>
<table class="card">
<tr><td>city</td><td>{{.City}}</td></tr>
<tr><td>agent</td><td>{{.Card.Agent}}</td></tr>
<tr><td>session</td><td>{{.Card.Name}}</td></tr>
<tr><td>state</td><td>{{.State}}</td></tr>
<tr><td>began</td><td>{{.Began}}</td></tr>
<tr><td>ended</td><td>{{if .Ended}}{{.Ended}}{{else}}not yet{{end}}</td></tr>
<tr><td>stream</td><td><a href="{{path .Card.Name}}/raw">as it was, one JSON message a line</a></td></tr>
</table>
<hr>
{{range .Items}}<div class="item {{.From}} {{.Kind}}"><span class="time">{{.Time}} {{.From}}</span>
{{.Body}}{{if .HasFold}}<details><summary>{{.Summary}}</summary>{{if .Code}}<pre><code>{{.Fold}}</code></pre>{{else}}<blockquote>{{.Fold}}</blockquote>{{end}}</details>{{end}}
</div>
{{end}}</body></html>`))
