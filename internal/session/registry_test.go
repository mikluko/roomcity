package session_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/session"
)

func open(t *testing.T, path string) *session.Registry {
	t.Helper()
	r, err := session.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// untimed drops the time of a Prompt or a Lifecycle, which a test of what
// happened, and not when, compares without. A Frame keeps its own: the host
// gave it.
func untimed[T any](v T) T {
	switch e := any(v).(type) {
	case session.Prompt:
		e.Time = time.Time{}
		return any(e).(T)
	case session.Lifecycle:
		e.Time = time.Time{}
		return any(e).(T)
	}
	return v
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v, ok := <-ch:
		require.True(t, ok, "channel closed")
		return untimed(v)
	case <-time.After(5 * time.Second):
		t.Fatal("nothing received")
		panic("unreachable")
	}
}

func TestStartAuthorizesTheLatestTokenOnly(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))

	first, err := r.Start(ctx, "host-1", "host", session.AlwaysOn)
	require.NoError(t, err)
	require.True(t, r.Authorize("host-1", first))
	require.False(t, r.Authorize("host-1", ""))
	require.False(t, r.Authorize("other", first))

	second, err := r.Start(ctx, "host-1", "host", session.AlwaysOn)
	require.NoError(t, err)
	require.False(t, r.Authorize("host-1", first))
	require.True(t, r.Authorize("host-1", second))
}

func TestEventsShareOneNumberingAcrossAStartAfresh(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))

	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	at := time.Unix(1700000000, 5)
	_, err = r.Report(ctx, "s", []session.HostFrame{{Seq: 1, Time: at, Direction: session.FromAgent, Message: []byte(`{"a":1}`)}})
	require.NoError(t, err)
	_, err = r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)

	evs, err := r.Subscribe(ctx, "s", 0)
	require.NoError(t, err)
	require.Equal(t, session.Lifecycle{Seq: 1, Kind: session.Started}, recv(t, evs))
	require.Equal(t, session.Frame{Seq: 2, Time: at, Direction: session.FromAgent, Message: []byte(`{"a":1}`)}, recv(t, evs))
	require.Equal(t, session.Lifecycle{Seq: 3, Kind: session.StartedAfresh}, recv(t, evs))
}

func TestSubscribeFromAndFromNow(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	_, err = r.Report(ctx, "s", []session.HostFrame{
		{Seq: 1, Direction: session.ToAgent, Message: []byte("1")},
		{Seq: 2, Direction: session.FromAgent, Message: []byte("2")},
	})
	require.NoError(t, err)

	evs, err := r.Subscribe(ctx, "s", 2)
	require.NoError(t, err)
	require.Equal(t, uint64(3), recv(t, evs).Number())

	now, err := r.Subscribe(ctx, "s", session.FromNow)
	require.NoError(t, err)
	_, err = r.Report(ctx, "s", []session.HostFrame{{Seq: 3, Direction: session.FromAgent, Message: []byte("3")}})
	require.NoError(t, err)
	require.Equal(t, uint64(4), recv(t, now).Number())

	_, err = r.Subscribe(ctx, "nope", 0)
	require.ErrorIs(t, err, session.ErrNotFound)
}

func TestReportIsIdempotentOnTheHostsNumbers(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)

	batch := []session.HostFrame{
		{Seq: 1, Direction: session.ToAgent, Message: []byte("1")},
		{Seq: 2, Direction: session.FromAgent, Message: []byte("2")},
	}
	last, err := r.Report(ctx, "s", batch)
	require.NoError(t, err)
	require.Equal(t, uint64(2), last)
	last, err = r.Report(ctx, "s", append(batch, session.HostFrame{Seq: 3, Direction: session.FromAgent, Message: []byte("3")}))
	require.NoError(t, err)
	require.Equal(t, uint64(3), last)

	evs, err := r.Subscribe(ctx, "s", 1)
	require.NoError(t, err)
	for want := uint64(2); want <= 4; want++ {
		require.Equal(t, want, recv(t, evs).Number())
	}
}

func TestAgentsAnswerToSessionNewMakesTheSessionRunning(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- r.WaitRunning(ctx, "s") }()

	notRunning := func() {
		t.Helper()
		select {
		case err := <-done:
			t.Fatalf("running too early: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	report := func(seq uint64, d session.Direction, msg string) {
		t.Helper()
		_, err := r.Report(ctx, "s", []session.HostFrame{{Seq: seq, Direction: d, Message: []byte(msg)}})
		require.NoError(t, err)
	}

	report(1, session.ToAgent, `{"jsonrpc":"2.0","id":0,"method":"initialize"}`)
	report(2, session.FromAgent, `{"jsonrpc":"2.0","id":0,"result":{}}`)
	report(3, session.ToAgent, `{"jsonrpc":"2.0","id":1,"method":"session/new"}`)
	notRunning()
	report(4, session.FromAgent, `{"jsonrpc":"2.0","id":1,"error":{"code":-1}}`)
	notRunning()
	report(5, session.FromAgent, `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"x"}}`)
	require.NoError(t, <-done)
}

func TestAuthorizeRefusesAnEndedSession(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	token, err := r.Start(ctx, "s", "worker", session.Task)
	require.NoError(t, err)
	require.True(t, r.Authorize("s", token))
	require.NoError(t, r.End(ctx, "s"))
	require.False(t, r.Authorize("s", token))
}

func TestReportRefusesAFrameThatSkipsANumber(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)

	_, err = r.Report(ctx, "s", []session.HostFrame{
		{Seq: 1, Direction: session.ToAgent, Message: []byte("1")},
		{Seq: 3, Direction: session.ToAgent, Message: []byte("3")},
	})
	require.ErrorIs(t, err, session.ErrFrameGap)
	last, err := r.Report(ctx, "s", nil)
	require.NoError(t, err)
	require.Zero(t, last, "nothing of a refused call is stored")
}

func TestOpenRefusesAnotherSchemaVersion(t *testing.T) {
	ctx := ctxT(t)
	path := filepath.Join(t.TempDir(), "r.db")
	require.NoError(t, open(t, path).Close())

	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA user_version = 99`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = session.Open(ctx, path)
	require.ErrorContains(t, err, "schema version 99")
}

func TestInstructionsAtOrBelowAfterAreNoLongerDroppedByCancel(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	for _, text := range []string{"carried out", "waits"} {
		_, err = r.Prompt(ctx, "s", session.PromptInput{Text: text})
		require.NoError(t, err)
	}

	// The host names 1 as carried out; nobody takes 2.
	attached, cancel := context.WithCancel(ctx)
	_, err = r.Instructions(attached, "s", 1)
	require.NoError(t, err)
	cancel()
	require.NoError(t, r.Cancel(ctx, "s"))

	in, err := r.Instructions(ctx, "s", 0)
	require.NoError(t, err)
	require.Equal(t, "carried out", recv(t, in).Text)
	require.Equal(t, session.InstructionCancel, recv(t, in).Kind)
}

func TestNudgeSurvivesARestartBeforeItIsDelivered(t *testing.T) {
	ctx := ctxT(t)
	path := filepath.Join(t.TempDir(), "r.db")
	r := open(t, path)
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	stored, err := r.Nudge(ctx, "s", "wake up")
	require.NoError(t, err)
	require.NoError(t, r.Close())

	r = open(t, path)
	in, err := r.Instructions(ctx, "s", 0)
	require.NoError(t, err)
	got := recv(t, in)
	require.Equal(t, session.Instruction{Seq: 1, Kind: session.InstructionPrompt, Source: session.FromGC, Text: "wake up"}, got)

	evs, err := r.Subscribe(ctx, "s", 0)
	require.NoError(t, err)
	require.Equal(t, session.Lifecycle{Seq: 1, Kind: session.Started}, recv(t, evs))
	require.Equal(t, session.Prompt{Seq: stored, Source: session.FromGC, Text: "wake up", State: session.Stored}, recv(t, evs))

	// Being handed to the host is not being delivered: the host says what it did.
	require.NoError(t, r.Handled(ctx, "s", 1, session.BeganTurn))
	require.Equal(t, session.Prompt{Seq: 3, Source: session.FromGC, Text: "wake up", State: session.Delivered, Of: stored}, recv(t, evs))
}

func TestHandledMakesAPromptWaitingOrDeliveredOnce(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	stored, err := r.Prompt(ctx, "s", session.PromptInput{Text: "steer"})
	require.NoError(t, err)
	evs, err := r.Subscribe(ctx, "s", session.FromNow)
	require.NoError(t, err)

	require.NoError(t, r.Handled(ctx, "s", 1, session.Queued))
	require.NoError(t, r.Handled(ctx, "s", 1, session.Queued), "a report that comes again changes nothing")
	require.Equal(t, session.Prompt{Seq: stored + 1, Source: session.FromOwner, Text: "steer", State: session.Waiting, Of: stored}, recv(t, evs))

	require.NoError(t, r.Handled(ctx, "s", 1, session.BeganTurn))
	require.NoError(t, r.Handled(ctx, "s", 1, session.Steered), "nor does a second delivery")
	require.NoError(t, r.Handled(ctx, "s", 1, session.Queued), "nor a late report that it waits")
	require.Equal(t, session.Prompt{Seq: stored + 2, Source: session.FromOwner, Text: "steer", State: session.Delivered, Of: stored}, recv(t, evs))

	select {
	case ev := <-evs:
		t.Fatalf("an event nothing asked for: %#v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHandledAPromptThatWentAtOnceHasNoWaitingRecord(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	stored, err := r.Prompt(ctx, "s", session.PromptInput{Text: "now"})
	require.NoError(t, err)
	evs, err := r.Subscribe(ctx, "s", session.FromNow)
	require.NoError(t, err)

	require.NoError(t, r.Handled(ctx, "s", 1, session.Steered))
	require.Equal(t, session.Prompt{Seq: stored + 1, Source: session.FromOwner, Text: "now", State: session.Delivered, Of: stored}, recv(t, evs))
}

func TestHandledNamesAPromptOrIsNotFound(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	err = r.Cancel(ctx, "s")
	require.NoError(t, err)

	require.ErrorIs(t, r.Handled(ctx, "s", 9, session.Steered), session.ErrNotFound, "no such instruction")
	require.ErrorIs(t, r.Handled(ctx, "s", 1, session.Steered), session.ErrNotFound, "a cancel is no prompt")
}

func TestAttachAgainReceivesOnlyTheInstructionsAfterTheLastCarried(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	for _, text := range []string{"a", "b", "c"} {
		_, err = r.Prompt(ctx, "s", session.PromptInput{Text: text})
		require.NoError(t, err)
	}

	in, err := r.Instructions(ctx, "s", 1)
	require.NoError(t, err)
	require.Equal(t, "b", recv(t, in).Text)
	require.Equal(t, "c", recv(t, in).Text)

	_, err = r.Prompt(ctx, "s", session.PromptInput{Text: "d"})
	require.NoError(t, err)
	require.Equal(t, "d", recv(t, in).Text)
}

func TestCancelDropsPromptsNotYetSent(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	_, err = r.Prompt(ctx, "s", session.PromptInput{Text: "waits"})
	require.NoError(t, err)
	require.NoError(t, r.Cancel(ctx, "s"))
	require.NoError(t, r.AnswerPermission(ctx, "s", "7", "allow"))

	in, err := r.Instructions(ctx, "s", 0)
	require.NoError(t, err)
	require.Equal(t, session.InstructionCancel, recv(t, in).Kind)
	answer := recv(t, in)
	require.Equal(t, session.Instruction{Seq: 3, Kind: session.InstructionPermissionAnswer, RequestID: "7", OptionID: "allow"}, answer)
}

func TestCancelKeepsGcsNudgesThatWaitInTheirOrder(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	for _, p := range []struct {
		nudge bool
		text  string
	}{{true, "gc 1"}, {false, "owner 1"}, {true, "gc 2"}, {false, "owner 2"}} {
		if p.nudge {
			_, err = r.Nudge(ctx, "s", p.text)
		} else {
			_, err = r.Prompt(ctx, "s", session.PromptInput{Text: p.text})
		}
		require.NoError(t, err)
	}
	require.NoError(t, r.Cancel(ctx, "s"))

	in, err := r.Instructions(ctx, "s", 0)
	require.NoError(t, err)
	var got []string
	for range 3 {
		i := recv(t, in)
		if i.Kind == session.InstructionCancel {
			got = append(got, "cancel")
		} else {
			got = append(got, i.Text)
		}
	}
	require.Equal(t, []string{"cancel", "gc 1", "gc 2"}, got)
}

func TestACancelBetweenTheReadAndTheSendTakesTheNudgeOutOfTheStream(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	_, err = r.Nudge(ctx, "s", "gc waits")
	require.NoError(t, err)

	// The stream has read the nudge as instruction 1; a Cancel moves it
	// behind itself (cancel 2, nudge 3) before the stream sends.
	read, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	restore := session.SetAfterPending(func() {
		once.Do(func() {
			close(read)
			<-release
		})
	})
	defer restore()
	in, err := r.Instructions(ctx, "s", 0)
	require.NoError(t, err)
	<-read
	require.NoError(t, r.Cancel(ctx, "s"))
	close(release)

	var got []session.Instruction
	for range 2 {
		got = append(got, recv(t, in))
	}
	require.Equal(t, session.InstructionCancel, got[0].Kind)
	require.Equal(t, uint64(2), got[0].Seq)
	require.Equal(t, session.Instruction{Seq: 3, Kind: session.InstructionPrompt, Source: session.FromGC, Text: "gc waits"}, got[1])
	select {
	case again := <-in:
		t.Fatalf("the nudge went to the host twice: %+v", again)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestAHostThatAttachesAgainFromAnEarlierNumberGetsWhatItWasSent(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	for _, text := range []string{"a", "b"} {
		_, err = r.Nudge(ctx, "s", text)
		require.NoError(t, err)
	}
	first, err := r.Instructions(ctx, "s", 0)
	require.NoError(t, err)
	require.Equal(t, "a", recv(t, first).Text)
	require.Equal(t, "b", recv(t, first).Text)

	again, err := r.Instructions(ctx, "s", 0)
	require.NoError(t, err)
	require.Equal(t, "a", recv(t, again).Text)
	require.Equal(t, "b", recv(t, again).Text)
}

func TestStartAfreshEmptiesTheQueue(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	_, err = r.Nudge(ctx, "s", "old")
	require.NoError(t, err)
	_, err = r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	_, err = r.Nudge(ctx, "s", "new")
	require.NoError(t, err)

	in, err := r.Instructions(ctx, "s", 0)
	require.NoError(t, err)
	got := recv(t, in)
	require.Equal(t, uint64(1), got.Seq)
	require.Equal(t, "new", got.Text)
}

func TestInstructionsToAnEndedSessionAreRefused(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Prompt(ctx, "s", session.PromptInput{Text: "x"})
	require.ErrorIs(t, err, session.ErrNotFound)

	_, err = r.Start(ctx, "s", "worker", session.Task)
	require.NoError(t, err)
	require.NoError(t, r.End(ctx, "s"))
	_, err = r.Prompt(ctx, "s", session.PromptInput{Text: "x"})
	require.ErrorIs(t, err, session.ErrNotRunning)
	require.ErrorIs(t, r.WaitRunning(ctx, "s"), session.ErrNotRunning)
}

func TestWatchReportsCurrentSessionsThenChanges(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "a", "host", session.AlwaysOn)
	require.NoError(t, err)

	w, err := r.Watch(ctx)
	require.NoError(t, err)
	require.Equal(t, session.Info{Name: "a", Agent: "host", Kind: session.AlwaysOn, State: session.Starting}, recv(t, w))

	_, err = r.Start(ctx, "b", "worker", session.Task)
	require.NoError(t, err)
	require.Equal(t, "b", recv(t, w).Name)

	require.NoError(t, r.End(ctx, "a"))
	require.Equal(t, session.Info{Name: "a", Agent: "host", Kind: session.AlwaysOn, State: session.Ended}, recv(t, w))
}

func TestReportWithNoFramesReturnsTheStoredNumber(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)

	last, err := r.Report(ctx, "s", nil)
	require.NoError(t, err)
	require.Zero(t, last)

	_, err = r.Report(ctx, "s", []session.HostFrame{{Seq: 1, Direction: session.ToAgent, Message: []byte("1")}})
	require.NoError(t, err)
	last, err = r.Report(ctx, "s", nil)
	require.NoError(t, err)
	require.Equal(t, uint64(1), last)
}

func TestAttachStreamEndsWhenTheSessionIsStartedAfreshOrEnded(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)

	old, err := r.Instructions(ctx, "s", 0)
	require.NoError(t, err)
	_, err = r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	_, err = r.Nudge(ctx, "s", "for the new host")
	require.NoError(t, err)
	select {
	case in, ok := <-old:
		require.False(t, ok, "the earlier host was handed %+v", in)
	case <-time.After(5 * time.Second):
		t.Fatal("the earlier host's stream stayed open")
	}

	current, err := r.Instructions(ctx, "s", 0)
	require.NoError(t, err)
	require.Equal(t, "for the new host", recv(t, current).Text)
	require.NoError(t, r.End(ctx, "s"))
	select {
	case _, ok := <-current:
		require.False(t, ok)
	case <-time.After(5 * time.Second):
		t.Fatal("the stream of an ended session stayed open")
	}
}

func TestStopIsSentBeforeTheStreamEnds(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	in, err := r.Instructions(ctx, "s", 0)
	require.NoError(t, err)

	seq, err := r.Stop(ctx, "s")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- r.WaitSent(ctx, "s", seq) }()
	require.Equal(t, session.InstructionStop, recv(t, in).Kind)
	require.NoError(t, <-done)
}

func TestMarksAreKeptPerSessionAndEmptyWhenUnset(t *testing.T) {
	ctx := ctxT(t)
	path := filepath.Join(t.TempDir(), "r.db")
	r := open(t, path)

	v, err := r.Mark(ctx, "nobody", "k")
	require.NoError(t, err)
	require.Empty(t, v)

	require.NoError(t, r.SetMark(ctx, "s", "GC_DRAIN", "1"))
	require.NoError(t, r.SetMark(ctx, "s", "GC_DRAIN", "2"))
	require.NoError(t, r.SetMark(ctx, "s", "other", "x"))
	require.NoError(t, r.Close())

	r = open(t, path)
	v, err = r.Mark(ctx, "s", "GC_DRAIN")
	require.NoError(t, err)
	require.Equal(t, "2", v)

	require.NoError(t, r.RemoveMark(ctx, "s", "GC_DRAIN"))
	require.NoError(t, r.RemoveMark(ctx, "s", "GC_DRAIN"))
	v, _ = r.Mark(ctx, "s", "GC_DRAIN")
	require.Empty(t, v)
	require.NoError(t, r.ClearMarks(ctx, "s"))
	v, _ = r.Mark(ctx, "s", "other")
	require.Empty(t, v)
}

func TestActivityFollowsTheTurnsOfTheFrames(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Activity(ctx, "nobody")
	require.ErrorIs(t, err, session.ErrNotFound)
	_, err = r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)

	act, err := r.Activity(ctx, "s")
	require.NoError(t, err)
	require.Equal(t, session.Activity{}, act)

	at := time.Unix(1700000000, 0)
	seq := uint64(0)
	report := func(d session.Direction, msg string) {
		seq++
		at = at.Add(time.Second)
		_, err := r.Report(ctx, "s", []session.HostFrame{{Seq: seq, Time: at, Direction: d, Message: []byte(msg)}})
		require.NoError(t, err)
	}
	report(session.ToAgent, `{"jsonrpc":"2.0","id":3,"method":"session/prompt"}`)
	act, err = r.Activity(ctx, "s")
	require.NoError(t, err)
	require.True(t, act.Turn)
	require.True(t, act.Last.Equal(at))

	// A response to another request does not end the turn; its own does.
	report(session.FromAgent, `{"jsonrpc":"2.0","id":4,"result":{}}`)
	act, _ = r.Activity(ctx, "s")
	require.True(t, act.Turn)
	report(session.FromAgent, `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"cancelled"}}`)
	act, _ = r.Activity(ctx, "s")
	require.False(t, act.Turn)
	require.True(t, act.Last.Equal(at))

	// A start afresh ends a turn that was running.
	report(session.ToAgent, `{"jsonrpc":"2.0","id":5,"method":"session/prompt"}`)
	_, err = r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	act, _ = r.Activity(ctx, "s")
	require.False(t, act.Turn)
}

func TestTailReturnsTheLastEventsInOrder(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	for i := uint64(1); i <= 5; i++ {
		_, err = r.Report(ctx, "s", []session.HostFrame{{Seq: i, Direction: session.ToAgent, Message: []byte("x")}})
		require.NoError(t, err)
	}
	evs, err := r.Tail(ctx, "s", 3)
	require.NoError(t, err)
	require.Len(t, evs, 3)
	require.Equal(t, uint64(4), evs[0].Number())
	require.Equal(t, uint64(6), evs[2].Number())
	evs, err = r.Tail(ctx, "s", 100)
	require.NoError(t, err)
	require.Len(t, evs, 6)
}

func TestAnInterruptDropsNothingAndLeavesTheOwnersPromptsOpen(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	_, err = r.Prompt(ctx, "s", session.PromptInput{Text: "owner waits"})
	require.NoError(t, err)
	_, err = r.Nudge(ctx, "s", "gc waits")
	require.NoError(t, err)
	require.NoError(t, r.Interrupt(ctx, "s"))

	in, err := r.Instructions(ctx, "s", 0)
	require.NoError(t, err)
	require.Equal(t, "owner waits", recv(t, in).Text)
	require.Equal(t, "gc waits", recv(t, in).Text)
	cancel := recv(t, in)
	require.Equal(t, session.Instruction{Seq: 3, Kind: session.InstructionCancel, KeepWaiting: true}, cancel)

	evs, err := r.Subscribe(ctx, "s", 0)
	require.NoError(t, err)
	for range 3 { // started and the two stored prompts, and nothing more
		e := recv(t, evs)
		if p, ok := e.(session.Prompt); ok {
			require.Equal(t, session.Stored, p.State)
		}
	}
	select {
	case e := <-evs:
		t.Fatalf("an interrupt closed a record: %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestStopClosesTheRecordOfEveryOwnersPromptThatWaitsAndOnlyThose(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	first, err := r.Prompt(ctx, "s", session.PromptInput{Text: "taken"})
	require.NoError(t, err)
	waits, err := r.Prompt(ctx, "s", session.PromptInput{Text: "waits"})
	require.NoError(t, err)
	_, err = r.Nudge(ctx, "s", "gc waits")
	require.NoError(t, err)
	// The host took the first prompt into a turn: its record is closed.
	require.NoError(t, r.Handled(ctx, "s", 1, session.BeganTurn))

	require.NoError(t, r.Cancel(ctx, "s"))
	require.NoError(t, r.Cancel(ctx, "s"))

	evs, err := r.Tail(ctx, "s", 100)
	require.NoError(t, err)
	var closed []session.Prompt
	for _, e := range evs {
		if p, ok := e.(session.Prompt); ok && p.State == session.Dropped {
			closed = append(closed, p)
		}
	}
	require.Len(t, closed, 1, "the prompt that waited, once, though Cancel ran twice: %+v", closed)
	require.Equal(t, session.Prompt{Seq: closed[0].Seq, Source: session.FromOwner, Text: "waits", State: session.Dropped, Of: waits}, untimed(closed[0]))
	require.NotEqual(t, first, closed[0].Of)
}

func TestSessionEndDropsHeldAndQueuedPromptsOnce(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	delivered, err := r.Nudge(ctx, "s", "delivered")
	require.NoError(t, err)
	held, err := r.Nudge(ctx, "s", "held")
	require.NoError(t, err)
	queued, err := r.Nudge(ctx, "s", "queued")
	require.NoError(t, err)
	require.NoError(t, r.Handled(ctx, "s", 1, session.BeganTurn))
	require.NoError(t, r.Handled(ctx, "s", 2, session.Queued))

	require.NoError(t, r.End(ctx, "s"))
	require.NoError(t, r.End(ctx, "s"), "a repeated end closes nothing again")

	evs, err := r.Tail(ctx, "s", 100)
	require.NoError(t, err)
	var dropped, deliveredRecords []session.Prompt
	for _, e := range evs {
		p, ok := e.(session.Prompt)
		if !ok {
			continue
		}
		if p.State == session.Dropped {
			dropped = append(dropped, untimed(p))
		}
		if p.Of == delivered {
			deliveredRecords = append(deliveredRecords, p)
		}
	}
	require.Len(t, dropped, 2)
	require.Equal(t, []session.Prompt{
		{Seq: dropped[0].Seq, Source: session.FromGC, Text: "held", State: session.Dropped, Of: held},
		{Seq: dropped[1].Seq, Source: session.FromGC, Text: "queued", State: session.Dropped, Of: queued},
	}, dropped)
	require.Len(t, deliveredRecords, 1)
	require.Equal(t, session.Delivered, deliveredRecords[0].State)
}

func TestSessionEndWithoutAWaitingPromptAddsNoPromptRecord(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
	require.NoError(t, err)
	require.NoError(t, r.End(ctx, "s"))

	evs, err := r.Tail(ctx, "s", 100)
	require.NoError(t, err)
	for _, e := range evs {
		_, ok := e.(session.Prompt)
		require.False(t, ok, "an end with nothing waiting recorded a prompt: %+v", e)
	}
}

func TestEveryWayOfEndingDropsGcsWaitingPrompt(t *testing.T) {
	tests := []struct {
		name string
		end  func(context.Context, *session.Registry) error
	}{
		{"end", func(ctx context.Context, r *session.Registry) error {
			return r.End(ctx, "s")
		}},
		{"failed end", func(ctx context.Context, r *session.Registry) error {
			return r.EndAs(ctx, "s", session.HowFailed)
		}},
		{"lost end", func(ctx context.Context, r *session.Registry) error {
			return r.EndAs(ctx, "s", session.HowLost)
		}},
		{"start afresh", func(ctx context.Context, r *session.Registry) error {
			_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
			return err
		}},
		{"reset then end", func(ctx context.Context, r *session.Registry) error {
			if err := r.Reset(ctx, "s"); err != nil {
				return err
			}
			return r.End(ctx, "s")
		}},
		{"reset then start afresh", func(ctx context.Context, r *session.Registry) error {
			if err := r.Reset(ctx, "s"); err != nil {
				return err
			}
			_, err := r.Start(ctx, "s", "host", session.AlwaysOn)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := ctxT(t)
			r := open(t, filepath.Join(t.TempDir(), "r.db"))
			r.SetResetter(func(context.Context, string) error { return nil })
			_, err := r.StartWith(ctx, "s", "host", session.AlwaysOn, session.StartInfo{GCID: "gc-1"})
			require.NoError(t, err)
			waiting, err := r.Nudge(ctx, "s", "gc waits")
			require.NoError(t, err)
			require.NoError(t, r.Handled(ctx, "s", 1, session.Queued))

			require.NoError(t, tt.end(ctx, r))

			evs, err := r.Tail(ctx, "s", 100)
			require.NoError(t, err)
			var dropped []session.Prompt
			for _, e := range evs {
				if p, ok := e.(session.Prompt); ok && p.State == session.Dropped {
					dropped = append(dropped, p)
				}
			}
			require.Len(t, dropped, 1)
			require.Equal(t, waiting, dropped[0].Of)
			require.Equal(t, session.FromGC, dropped[0].Source)
		})
	}
}

func TestAStartsIdentityIsKeptAsMarksAndTheOnesGcStampsMove(t *testing.T) {
	ctx := ctxT(t)
	r := open(t, filepath.Join(t.TempDir(), "r.db"))
	_, err := r.Start(ctx, "s", "guest", session.Task)
	require.NoError(t, err)

	_, ok, err := r.Identity(ctx, "s")
	require.NoError(t, err)
	require.False(t, ok, "a start that kept none")

	require.NoError(t, r.RecordIdentity(ctx, "s", map[string]string{
		"GC_AGENT": "guest", "GC_RUNTIME_EPOCH": "5", "GC_INSTANCE_TOKEN": "tok", "GC_CONTROLLER_TOKEN": "x", "GC_HOME": "/h", "GC_ALIAS": "",
	}))
	got, ok, err := r.Identity(ctx, "s")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, map[string]string{"GC_AGENT": "guest", "GC_RUNTIME_EPOCH": "5", "GC_INSTANCE_TOKEN": "tok"}, got)

	// gc's adoption barrier writes the epoch of a runtime it adopts.
	require.NoError(t, r.SetMark(ctx, "s", "GC_RUNTIME_EPOCH", "6"))
	got, _, _ = r.Identity(ctx, "s")
	require.Equal(t, "6", got["GC_RUNTIME_EPOCH"])

	require.NoError(t, r.ClearMarks(ctx, "s"))
	_, ok, _ = r.Identity(ctx, "s")
	require.False(t, ok, "a start afresh clears the record before it makes its own")
}
