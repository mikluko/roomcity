package standin

import (
	"context"

	"connectrpc.com/connect"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

// spy is a GcService that reports the session and directory of each call.
type spy struct{ saw func(session, cwd string) }

func (s spy) Run(_ context.Context, req *connect.Request[roomcityv1.RunRequest]) (*connect.Response[roomcityv1.RunResponse], error) {
	s.saw(req.Msg.Session, req.Msg.Cwd)
	return connect.NewResponse(&roomcityv1.RunResponse{}), nil
}
