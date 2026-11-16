package server

import (
	"context"
	"errors"
	"net/http"

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
		out.Instruction = &roomcityv1.AttachResponse_Cancel{Cancel: &roomcityv1.Cancel{}}
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
