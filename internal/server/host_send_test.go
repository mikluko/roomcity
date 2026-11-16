package server_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

func TestHostSendKnowsTheSessionAndSaysItIsNotDoneYet(t *testing.T) {
	f := newHostFixture(t)
	ctx := context.Background()
	req := &roomcityv1.SendRequest{Session: "s", Name: "chart.png", MediaType: "image/png", Data: []byte("x")}

	_, err := f.anon.Send(ctx, connect.NewRequest(req))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

	_, err = f.client.Send(ctx, connect.NewRequest(req))
	require.Equal(t, connect.CodeUnimplemented, connect.CodeOf(err))
}
