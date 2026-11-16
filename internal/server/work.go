package server

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

// Work hands tasks between sessions: the foreman's dispatch, wait and result, a
// worker's claim and done. Its errors are said to the agent as they are.
type Work interface {
	// Dispatch gives the task to the workers and returns its id.
	Dispatch(ctx context.Context, session, task string) (id string, err error)
	// Wait has the session told when the task is closed.
	Wait(ctx context.Context, session, id string) error
	// Result says whether the task is closed, its state, and what was stored
	// in it when it was.
	Result(ctx context.Context, session, id string) (closed bool, status, result, outcome string, err error)
	// Claim takes the task the session was started for; found is false when
	// there is none.
	Claim(ctx context.Context, session string) (id, text string, found bool, err error)
	// Done closes the session's claimed task with result and ends the session.
	Done(ctx context.Context, session, result string) error
}

// has is the check of the tool for the session's agent, made after its token
// is proved: the tools are decided by the agent's role in the configuration.
func (h *Host) has(ctx context.Context, session, tool string) error {
	info, err := h.Registry.Get(ctx, session)
	if err != nil {
		return fail(err)
	}
	if h.Work == nil || h.Tools == nil || !slices.Contains(h.Tools(info.Agent), tool) {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("the agent %s has no tool %s", info.Agent, tool))
	}
	return nil
}

func workError(err error) error {
	return connect.NewError(connect.CodeFailedPrecondition, err)
}

func (h *Host) Dispatch(ctx context.Context, req *connect.Request[roomcityv1.DispatchRequest]) (*connect.Response[roomcityv1.DispatchResponse], error) {
	m := req.Msg
	if err := h.authorize(m.Session, req.Header()); err != nil {
		return nil, err
	}
	if err := h.has(ctx, m.Session, "dispatch"); err != nil {
		return nil, err
	}
	if m.Task == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the task is empty"))
	}
	id, err := h.Work.Dispatch(ctx, m.Session, m.Task)
	if err != nil {
		return nil, workError(err)
	}
	return connect.NewResponse(&roomcityv1.DispatchResponse{TaskId: id}), nil
}

func (h *Host) Wait(ctx context.Context, req *connect.Request[roomcityv1.WaitRequest]) (*connect.Response[roomcityv1.WaitResponse], error) {
	m := req.Msg
	if err := h.authorize(m.Session, req.Header()); err != nil {
		return nil, err
	}
	if err := h.has(ctx, m.Session, "wait"); err != nil {
		return nil, err
	}
	if m.TaskId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("no task given"))
	}
	if err := h.Work.Wait(ctx, m.Session, m.TaskId); err != nil {
		return nil, workError(err)
	}
	return connect.NewResponse(&roomcityv1.WaitResponse{}), nil
}

func (h *Host) Result(ctx context.Context, req *connect.Request[roomcityv1.ResultRequest]) (*connect.Response[roomcityv1.ResultResponse], error) {
	m := req.Msg
	if err := h.authorize(m.Session, req.Header()); err != nil {
		return nil, err
	}
	if err := h.has(ctx, m.Session, "result"); err != nil {
		return nil, err
	}
	if m.TaskId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("no task given"))
	}
	closed, status, result, outcome, err := h.Work.Result(ctx, m.Session, m.TaskId)
	if err != nil {
		return nil, workError(err)
	}
	return connect.NewResponse(&roomcityv1.ResultResponse{Closed: closed, Status: status, Result: result, Outcome: outcome}), nil
}

func (h *Host) Claim(ctx context.Context, req *connect.Request[roomcityv1.ClaimRequest]) (*connect.Response[roomcityv1.ClaimResponse], error) {
	m := req.Msg
	if err := h.authorize(m.Session, req.Header()); err != nil {
		return nil, err
	}
	if err := h.has(ctx, m.Session, "claim"); err != nil {
		return nil, err
	}
	id, text, found, err := h.Work.Claim(ctx, m.Session)
	if err != nil {
		return nil, workError(err)
	}
	return connect.NewResponse(&roomcityv1.ClaimResponse{Found: found, TaskId: id, Text: text}), nil
}

func (h *Host) Done(ctx context.Context, req *connect.Request[roomcityv1.DoneRequest]) (*connect.Response[roomcityv1.DoneResponse], error) {
	m := req.Msg
	if err := h.authorize(m.Session, req.Header()); err != nil {
		return nil, err
	}
	if err := h.has(ctx, m.Session, "done"); err != nil {
		return nil, err
	}
	if err := h.Work.Done(ctx, m.Session, m.Result); err != nil {
		return nil, workError(err)
	}
	return connect.NewResponse(&roomcityv1.DoneResponse{}), nil
}
