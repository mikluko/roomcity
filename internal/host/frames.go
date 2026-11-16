package host

import (
	"bytes"
	"io"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

// recorder numbers the ACP messages of a session and keeps those the server
// has not acknowledged.
type recorder struct {
	now func() time.Time

	mu      sync.Mutex
	next    uint64
	pending []*roomcityv1.Frame
	wake    chan struct{}
}

func newRecorder(now func() time.Time) *recorder {
	return &recorder{now: now, next: 1, wake: make(chan struct{}, 1)}
}

// continueAfter numbers the next frame after seq, the last the server holds:
// a host that starts again in a pod that had one before must not reuse seqs.
func (r *recorder) continueAfter(seq uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next = seq + 1
}

// add records one message, as it passed in direction dir.
func (r *recorder) add(dir roomcityv1.FrameDirection, message []byte) {
	r.mu.Lock()
	r.pending = append(r.pending, &roomcityv1.Frame{
		Seq:       r.next,
		Time:      timestamppb.New(r.now()),
		Direction: dir,
		Message:   bytes.Clone(message),
	})
	r.next++
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// unacknowledged returns up to limit frames, in order, that the server has
// not acknowledged.
func (r *recorder) unacknowledged(limit int) []*roomcityv1.Frame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*roomcityv1.Frame(nil), r.pending[:min(limit, len(r.pending))]...)
}

// acknowledge forgets the frames up to and including seq.
func (r *recorder) acknowledge(seq uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := 0
	for i < len(r.pending) && r.pending[i].Seq <= seq {
		i++
	}
	r.pending = r.pending[i:]
}

// lines splits a stream of newline-ended messages and hands each message,
// without its newline, to emit.
type lines struct {
	buf  []byte
	emit func([]byte)
}

func (l *lines) write(p []byte) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			return
		}
		if line := bytes.TrimSuffix(l.buf[:i], []byte("\r")); len(line) > 0 {
			l.emit(line)
		}
		l.buf = l.buf[i+1:]
	}
}

// tapWriter records each complete message in what it is given, then passes
// the write on to w. The order matters: a write to a pipe returns only once
// the agent has read, and the agent's answer may be recorded by the other tap
// before that, so a request recorded after its write could be numbered after
// its own answer.
type tapWriter struct {
	w io.Writer
	l lines
}

func newTapWriter(w io.Writer, rec *recorder, dir roomcityv1.FrameDirection) *tapWriter {
	return &tapWriter{w: w, l: lines{emit: func(m []byte) { rec.add(dir, m) }}}
}

func (t *tapWriter) Write(p []byte) (int, error) {
	t.l.write(p)
	return t.w.Write(p)
}

// tapReader records each complete message in what r yields, as it yields it.
type tapReader struct {
	r io.Reader
	l lines
}

func newTapReader(r io.Reader, rec *recorder, dir roomcityv1.FrameDirection) *tapReader {
	return &tapReader{r: r, l: lines{emit: func(m []byte) { rec.add(dir, m) }}}
}

func (t *tapReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.l.write(p[:n])
	return n, err
}
