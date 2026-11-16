package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1/roomcityv1connect"
	"sarak.sh/mikluko/roomcity/internal/session"
	"sarak.sh/mikluko/roomcity/internal/sessionauth"
)

// stageChunk is the size of a piece of the archive Stage sends.
const stageChunk = 64 << 10

// Host is the server's side of HostService: what a session host calls to
// receive its instructions, hand over its frames and fetch its start's
// bundle. A call proves its session by the session's token.
type Host struct {
	Registry *session.Registry
	// Files is where a file goes to the room; nil is a server with no chat side.
	Files Files
	// Work carries out the tools that hand work between sessions, and Tools
	// says which of dispatch, wait, claim and done an agent's sessions have;
	// a session of an agent that lacks the tool is refused. Without either,
	// every one is refused.
	Work  Work
	Tools func(agent string) []string
	// Observe, when set, is given the frames of a session as they are
	// reported, once they are stored.
	Observe func(ctx context.Context, session string, frames []session.HostFrame)
	// MaxFile is the most bytes of a file that Send takes; zero is DefaultMaxFile.
	MaxFile int64
	Log     *slog.Logger
}

// DefaultMaxFile is the most bytes of a file Send takes when the server is
// told no other, as the chat side's own limit is by default.
const DefaultMaxFile = 10 << 20

// File is a file for the room.
type File struct {
	Name, MediaType, Caption string
	Data                     []byte
}

// Files is the chat side as Send uses it: it sends the file to the session's
// room and returns the chat system's id of the event that carries it. Its
// refusals are ErrFileTooLarge, ErrFileKind, ErrNoRoom and ErrChatDown, wrapped
// with the reason, which Send maps to the codes that the agent reads.
type Files interface {
	SendFile(ctx context.Context, session string, f File) (event string, err error)
}

// What the chat side refuses a file for.
var (
	ErrFileTooLarge = errors.New("the file is larger than the room takes")
	ErrFileKind     = errors.New("the room does not take a file of that kind")
	ErrNoRoom       = errors.New("the session has no room")
	ErrChatDown     = errors.New("the chat system cannot be reached")
)

func (h *Host) maxFile() int64 {
	if h.MaxFile > 0 {
		return h.MaxFile
	}
	return DefaultMaxFile
}

func (h *Host) logger() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

var _ roomcityv1connect.HostServiceHandler = (*Host)(nil)

func (h *Host) authorize(name string, header http.Header) error {
	token := sessionauth.Token(header)
	if name == "" || token == "" || !h.Registry.Authorize(name, token) {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("the session's token is missing or not its own"))
	}
	return nil
}

func fail(err error) error {
	switch {
	case errors.Is(err, session.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, session.ErrNotRunning):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, session.ErrFrameGap):
		return connect.NewError(connect.CodeOutOfRange, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return connect.NewError(connect.CodeInternal, err)
}

// Attach streams the session's instructions after the request's last_seq,
// and those queued later, until the host goes away.
func (h *Host) Attach(ctx context.Context, req *connect.Request[roomcityv1.AttachRequest], stream *connect.ServerStream[roomcityv1.AttachResponse]) error {
	if err := h.authorize(req.Msg.Session, req.Header()); err != nil {
		return err
	}
	in, err := h.Registry.Instructions(ctx, req.Msg.Session, req.Msg.LastSeq)
	if err != nil {
		return fail(err)
	}
	for i := range in {
		if err := stream.Send(instruction(i)); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func instruction(i session.Instruction) *roomcityv1.AttachResponse {
	out := &roomcityv1.AttachResponse{Seq: i.Seq}
	switch i.Kind {
	case session.InstructionPrompt:
		src := roomcityv1.PromptSource_PROMPT_SOURCE_OWNER
		if i.Source == session.FromGC {
			src = roomcityv1.PromptSource_PROMPT_SOURCE_GC
		}
		out.Instruction = &roomcityv1.AttachResponse_Prompt{Prompt: &roomcityv1.Prompt{
			Text:     i.Text,
			Source:   src,
			IntoTurn: i.Source != session.FromGC,
		}}
	case session.InstructionCancel:
		out.Instruction = &roomcityv1.AttachResponse_Cancel{Cancel: &roomcityv1.Cancel{KeepWaiting: i.KeepWaiting}}
	case session.InstructionPermissionAnswer:
		out.Instruction = &roomcityv1.AttachResponse_PermissionAnswer{PermissionAnswer: &roomcityv1.PermissionAnswer{RequestId: i.RequestID, OptionId: i.OptionID}}
	case session.InstructionStop:
		out.Instruction = &roomcityv1.AttachResponse_Stop{Stop: &roomcityv1.Stop{}}
	}
	return out
}

// Report stores the frames, and answers the number of the last host frame
// stored.
func (h *Host) Report(ctx context.Context, req *connect.Request[roomcityv1.ReportRequest]) (*connect.Response[roomcityv1.ReportResponse], error) {
	if err := h.authorize(req.Msg.Session, req.Header()); err != nil {
		return nil, err
	}
	frames := make([]session.HostFrame, 0, len(req.Msg.Frames))
	for _, f := range req.Msg.Frames {
		d := session.ToAgent
		if f.Direction == roomcityv1.FrameDirection_FRAME_DIRECTION_FROM_AGENT {
			d = session.FromAgent
		}
		hf := session.HostFrame{Seq: f.Seq, Direction: d, Message: f.Message}
		if f.Time != nil {
			hf.Time = f.Time.AsTime()
		}
		frames = append(frames, hf)
	}
	last, err := h.Registry.Report(ctx, req.Msg.Session, frames)
	if err != nil {
		return nil, fail(err)
	}
	if h.Observe != nil {
		h.Observe(ctx, req.Msg.Session, frames)
	}
	return connect.NewResponse(&roomcityv1.ReportResponse{LastSeq: last}), nil
}

// Handled records what the host did with a prompt it was handed.
func (h *Host) Handled(ctx context.Context, req *connect.Request[roomcityv1.HandledRequest]) (*connect.Response[roomcityv1.HandledResponse], error) {
	if err := h.authorize(req.Msg.Session, req.Header()); err != nil {
		return nil, err
	}
	var handling session.Handling
	switch req.Msg.Handling {
	case roomcityv1.Handling_HANDLING_STEERED:
		handling = session.Steered
	case roomcityv1.Handling_HANDLING_STARTED:
		handling = session.BeganTurn
	case roomcityv1.Handling_HANDLING_QUEUED:
		handling = session.Queued
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the handling is not one the server knows"))
	}
	if err := h.Registry.Handled(ctx, req.Msg.Session, req.Msg.InstructionSeq, handling); err != nil {
		return nil, fail(err)
	}
	return connect.NewResponse(&roomcityv1.HandledResponse{}), nil
}

// Send hands a file of the session to the room through Files and records, in
// the session's stream, what it was and the event that carries it. The call
// proves its session by its token; the bounds are checked here, since the host
// is not trusted with them, and then by the chat side, whose they also are.
// What the agent reads of a refusal is the message of the error.
func (h *Host) Send(ctx context.Context, req *connect.Request[roomcityv1.SendRequest]) (*connect.Response[roomcityv1.SendResponse], error) {
	m := req.Msg
	if err := h.authorize(m.Session, req.Header()); err != nil {
		return nil, err
	}
	switch {
	case len(m.Data) == 0:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the file is empty"))
	case m.Name == "" || m.Name != filepath.Base(m.Name) || m.Name == "." || m.Name == "..":
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%q is not the name of a file: it must be a name without a directory", m.Name))
	case int64(len(m.Data)) > h.maxFile():
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("the file is %d bytes, and the server takes at most %d", len(m.Data), h.maxFile()))
	}
	if h.Files == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the server has no chat system to send a file to"))
	}
	event, err := h.Files.SendFile(ctx, m.Session, File{Name: m.Name, MediaType: m.MediaType, Caption: m.Caption, Data: m.Data})
	switch {
	case errors.Is(err, ErrFileTooLarge), errors.Is(err, ErrFileKind):
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, ErrNoRoom):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, ErrChatDown):
		return nil, connect.NewError(connect.CodeUnavailable, err)
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// The file is in the room; its record is the transcript's, and no reason to
	// tell the agent that it was not sent.
	if _, err := h.Registry.RecordFile(ctx, m.Session, session.File{Name: m.Name, MediaType: m.MediaType, Size: int64(len(m.Data)), Event: event}); err != nil {
		h.logger().Error("recording a file sent to the room", "session", m.Session, "name", m.Name, "err", err)
	}
	return connect.NewResponse(&roomcityv1.SendResponse{Event: event}), nil
}

// Stage sends the bundle of the session's start: the manifest, then the
// archive in pieces. A start that kept no bundle gets an empty manifest.
func (h *Host) Stage(ctx context.Context, req *connect.Request[roomcityv1.StageRequest], stream *connect.ServerStream[roomcityv1.StageResponse]) error {
	if err := h.authorize(req.Msg.Session, req.Header()); err != nil {
		return err
	}
	manifest := &roomcityv1.StageManifest{}
	var archive []byte
	raw, archive, err := h.Registry.Bundle(ctx, req.Msg.Session)
	switch {
	case errors.Is(err, session.ErrNotFound):
	case err != nil:
		return fail(err)
	default:
		if err := proto.Unmarshal(raw, manifest); err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
	}
	if err := stream.Send(&roomcityv1.StageResponse{Part: &roomcityv1.StageResponse_Manifest{Manifest: manifest}}); err != nil {
		return err
	}
	for len(archive) > 0 {
		n := min(stageChunk, len(archive))
		if err := stream.Send(&roomcityv1.StageResponse{Part: &roomcityv1.StageResponse_Archive{Archive: archive[:n]}}); err != nil {
			return err
		}
		archive = archive[n:]
	}
	return nil
}
