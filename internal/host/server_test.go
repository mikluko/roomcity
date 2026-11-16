package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
	"sarak.sh/mikluko/roomcity/internal/sessionauth"
)

const testToken = "tok"

// fakeServer is an in-process HostService: it keeps the instructions a test
// gives it and the frames the host reports, takes a frame once by its seq,
// and can fail a number of Reports or drop the host's attachment.
type fakeServer struct {
	roomcityv1connect.UnimplementedHostServiceHandler

	mu          sync.Mutex
	changed     chan struct{} // closed and replaced at every change
	instruction []*roomcityv1.AttachResponse
	frames      []*roomcityv1.Frame
	attachedAt  []uint64 // the last_seq of each Attach
	failReports int
	drops       int // attachments to end, from the first
	reports     int // Report calls, failed ones too
	manifest    *roomcityv1.StageManifest
	archive     []byte
	stages      int // Stage calls
	handled     []handledCall
	failHandled int // Handled calls to fail, from the first
}

// handledCall is a Handled call of the host.
type handledCall struct {
	Seq      uint64
	Handling roomcityv1.Handling
}

func newFakeServer(t *testing.T) (*fakeServer, string) {
	t.Helper()
	s := &fakeServer{changed: make(chan struct{})}
	mux := http.NewServeMux()
	mux.Handle(roomcityv1connect.NewHostServiceHandler(s))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func (s *fakeServer) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// instruct gives the host the next instruction.
func (s *fakeServer) instruct(in *roomcityv1.AttachResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in.Seq = uint64(len(s.instruction) + 1)
	s.instruction = append(s.instruction, in)
	s.notify()
}

// drop ends the host's current attachment, as a lost connection does.
func (s *fakeServer) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drops++
	s.notify()
}

// handlings returns what the host has told the server of its prompts.
func (s *fakeServer) handlings() []handledCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]handledCall(nil), s.handled...)
}

func (s *fakeServer) Handled(_ context.Context, req *connect.Request[roomcityv1.HandledRequest]) (*connect.Response[roomcityv1.HandledResponse], error) {
	if sessionauth.Token(req.Header()) != testToken {
		return nil, connect.NewError(connect.CodeUnauthenticated, nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failHandled > 0 {
		s.failHandled--
		return nil, connect.NewError(connect.CodeUnavailable, nil)
	}
	s.handled = append(s.handled, handledCall{req.Msg.InstructionSeq, req.Msg.Handling})
	s.notify()
	return connect.NewResponse(&roomcityv1.HandledResponse{}), nil
}

func (s *fakeServer) failNextReports(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failReports = n
}

// stored returns the frames the server holds.
func (s *fakeServer) stored() []*roomcityv1.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*roomcityv1.Frame(nil), s.frames...)
}

func (s *fakeServer) attachments() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.attachedAt...)
}

func (s *fakeServer) Attach(ctx context.Context, req *connect.Request[roomcityv1.AttachRequest], stream *connect.ServerStream[roomcityv1.AttachResponse]) error {
	if sessionauth.Token(req.Header()) != testToken {
		return connect.NewError(connect.CodeUnauthenticated, nil)
	}
	s.mu.Lock()
	s.attachedAt = append(s.attachedAt, req.Msg.LastSeq)
	mine := s.drops
	next := req.Msg.LastSeq
	s.notify()
	for {
		for next < uint64(len(s.instruction)) {
			in := s.instruction[next]
			next++
			s.mu.Unlock()
			if err := stream.Send(in); err != nil {
				return err
			}
			s.mu.Lock()
		}
		if s.drops > mine {
			s.mu.Unlock()
			return connect.NewError(connect.CodeUnavailable, nil)
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
		s.mu.Lock()
	}
}

func (s *fakeServer) Report(_ context.Context, req *connect.Request[roomcityv1.ReportRequest]) (*connect.Response[roomcityv1.ReportResponse], error) {
	if sessionauth.Token(req.Header()) != testToken {
		return nil, connect.NewError(connect.CodeUnauthenticated, nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports++
	if s.failReports > 0 {
		s.failReports--
		return nil, connect.NewError(connect.CodeUnavailable, nil)
	}
	for _, f := range req.Msg.Frames {
		if f.Seq == uint64(len(s.frames))+1 {
			s.frames = append(s.frames, f)
		}
	}
	s.notify()
	return connect.NewResponse(&roomcityv1.ReportResponse{LastSeq: uint64(len(s.frames))}), nil
}

// wire is a frame's message, as far as the tests read it.
type wire struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}

func decode(t *testing.T, f *roomcityv1.Frame) wire {
	t.Helper()
	var w wire
	if err := json.Unmarshal(f.Message, &w); err != nil {
		t.Fatalf("frame %d is not JSON: %v: %s", f.Seq, err, f.Message)
	}
	return w
}

// eventually waits for ok to hold of the server's frames, then returns.
func (s *fakeServer) eventually(t *testing.T, what string, ok func([]*roomcityv1.Frame) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if ok(s.stored()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never: %s; frames: %d", what, len(s.stored()))
		}
		time.Sleep(time.Millisecond)
	}
}

func (s *fakeServer) reportCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reports
}

// stageWith sets what Stage serves.
func (s *fakeServer) stageWith(m *roomcityv1.StageManifest, archive []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manifest, s.archive = m, archive
}

func (s *fakeServer) Stage(_ context.Context, req *connect.Request[roomcityv1.StageRequest], stream *connect.ServerStream[roomcityv1.StageResponse]) error {
	if sessionauth.Token(req.Header()) != testToken {
		return connect.NewError(connect.CodeUnauthenticated, nil)
	}
	s.mu.Lock()
	s.stages++
	m, archive := s.manifest, s.archive
	s.mu.Unlock()
	if m == nil {
		return connect.NewError(connect.CodeUnimplemented, nil)
	}
	if err := stream.Send(&roomcityv1.StageResponse{Part: &roomcityv1.StageResponse_Manifest{Manifest: m}}); err != nil {
		return err
	}
	// The archive in two pieces, so that the host has to join them.
	half := len(archive) / 2
	for _, chunk := range [][]byte{archive[:half], archive[half:]} {
		if err := stream.Send(&roomcityv1.StageResponse{Part: &roomcityv1.StageResponse_Archive{Archive: chunk}}); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeServer) stageCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stages
}
