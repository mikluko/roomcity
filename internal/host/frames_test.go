package host

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

func TestTapsRecordWholeMessagesInOrderWithTheirDirection(t *testing.T) {
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	rec := newRecorder(func() time.Time { return at })
	var toAgent bytes.Buffer
	w := newTapWriter(&toAgent, rec, roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT)
	r := newTapReader(bytes.NewReader([]byte("{\"b\":1}\n{\"c\"")), rec, roomcityv1.FrameDirection_FRAME_DIRECTION_FROM_AGENT, nil)

	// A message split over two writes is one frame, and two in one write
	// are two.
	_, _ = w.Write([]byte("{\"a\":"))
	_, _ = w.Write([]byte("1}\n{\"d\":2}\n"))
	_, _ = io.ReadAll(r)

	got := rec.unacknowledged(10)
	var msgs []string
	for i, f := range got {
		require.Equal(t, uint64(i+1), f.Seq)
		require.True(t, at.Equal(f.Time.AsTime()))
		msgs = append(msgs, f.Direction.String()+" "+string(f.Message))
	}
	require.Equal(t, []string{
		"FRAME_DIRECTION_TO_AGENT {\"a\":1}",
		"FRAME_DIRECTION_TO_AGENT {\"d\":2}",
		"FRAME_DIRECTION_FROM_AGENT {\"b\":1}",
	}, msgs)
	require.Equal(t, "{\"a\":1}\n{\"d\":2}\n", toAgent.String(), "the tap passes everything on")
}

func TestAcknowledgeForgetsUpToSeq(t *testing.T) {
	rec := newRecorder(time.Now)
	for range 5 {
		rec.add(roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT, []byte("{}"))
	}

	rec.acknowledge(3)

	got := rec.unacknowledged(10)
	require.Len(t, got, 2)
	require.Equal(t, uint64(4), got[0].Seq)
	require.Len(t, rec.unacknowledged(1), 1)

	rec.acknowledge(1) // late and smaller: nothing returns
	require.Len(t, rec.unacknowledged(10), 2)
}

// answeringWriter is a pipe whose reader answers as soon as it has read, before
// the write returns, as an agent does.
type answeringWriter struct {
	rec *recorder
}

func (a answeringWriter) Write(p []byte) (int, error) {
	a.rec.add(roomcityv1.FrameDirection_FRAME_DIRECTION_FROM_AGENT, []byte(`{"answer":true}`))
	return len(p), nil
}

func TestARequestIsNumberedBeforeItsAnswer(t *testing.T) {
	rec := newRecorder(time.Now)
	w := newTapWriter(answeringWriter{rec}, rec, roomcityv1.FrameDirection_FRAME_DIRECTION_TO_AGENT)

	_, err := w.Write([]byte("{\"request\":true}\n"))
	require.NoError(t, err)

	got := rec.unacknowledged(10)
	require.Len(t, got, 2)
	require.Equal(t, `{"request":true}`, string(got[0].Message))
	require.Equal(t, `{"answer":true}`, string(got[1].Message))
}
