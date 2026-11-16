package chat_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/chat"
	"sarak.sh/mikluko/roomcity/internal/chat/chattest"
	"sarak.sh/mikluko/roomcity/internal/session"
)

func TestServiceRemovesThoseWhoHoldNoRoleOnceAtStartBeforeAnyRoomIsMade(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver, handle := runService(t, sessions)
	awaitRoom(t, sessions, handle)

	driver.mu.Lock()
	defer driver.mu.Unlock()
	require.Equal(t, [][]chat.UserID{{owner}}, driver.pruned, "once, keeping the owners")
}

func TestServiceRemovesNoOneWhenNoOneHoldsTheRole(t *testing.T) {
	sessions := &chattest.Sessions{}
	sessions.Add(alwaysOn("host", session.Running))
	driver := newFakeDriver()
	svc := &chat.Service{Driver: driver, Sessions: sessions, Log: slog.New(slog.DiscardHandler), City: "testcity", RetryEvery: 10 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- svc.Run(t.Context()) }()
	<-driver.handler

	require.Eventually(t, func() bool { return len(driver.roomSpecs()) == 1 }, 5*time.Second, 10*time.Millisecond)
	driver.mu.Lock()
	defer driver.mu.Unlock()
	require.Empty(t, driver.pruned, "an empty list is a mistake, not a wish to empty the city")
}
