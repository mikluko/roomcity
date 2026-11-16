package main

import (
	"context"
	"fmt"
	"log/slog"

	"sarak.sh/mikluko/roomcity/internal/host"
)

// runHost is the host of one session, in the session's pod.
func runHost(ctx context.Context, _ []string, p process) int {
	cfg, err := host.ConfigFromEnv(p.Env)
	if err != nil {
		_, _ = fmt.Fprintf(p.Stderr, "roomcity host: %v\n", err)
		return exitFailure
	}
	cfg.Log = slog.New(slog.NewTextHandler(p.Stderr, nil))
	if err := host.Run(ctx, cfg); err != nil {
		_, _ = fmt.Fprintf(p.Stderr, "roomcity host: %v\n", err)
		return exitFailure
	}
	return exitOK
}
