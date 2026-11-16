package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"sarak.sh/mikluko/roomcity/internal/config"
	"sarak.sh/mikluko/roomcity/internal/kube"
	"sarak.sh/mikluko/roomcity/internal/runtimecmd"
	"sarak.sh/mikluko/roomcity/internal/serve"
)

// envConfig names the variable that holds the path of roomcity.toml.
const envConfig = "ROOMCITY_CONFIG"

// runRuntime is gc's exec session runtime: one operation, carried out by the
// server.
func runRuntime(ctx context.Context, args []string, p process) int {
	return runtimecmd.Run(ctx, args, p.Env, runtimecmd.Streams{Stdin: p.Stdin, Stdout: p.Stdout, Stderr: p.Stderr})
}

// runServer is the server beside gc's supervisor.
func runServer(ctx context.Context, args []string, p process) int {
	fs := flag.NewFlagSet("roomcity server", flag.ContinueOnError)
	fs.SetOutput(p.Stderr)
	path := fs.String("config", configPath(p.Env), "the path of roomcity.toml")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	fail := func(err error) int {
		_, _ = fmt.Fprintf(p.Stderr, "roomcity server: %v\n", err)
		return exitFailure
	}

	cfg, err := config.Load(*path)
	if err != nil {
		return fail(err)
	}
	client, err := kube.InCluster()
	if err != nil {
		return fail(err)
	}
	ls, err := serve.Listen(cfg)
	if err != nil {
		return fail(err)
	}
	defer ls.Close()
	log := slog.New(slog.NewTextHandler(p.Stderr, nil))
	if err := serve.Run(ctx, serve.Options{Config: cfg, Listeners: ls, Jobs: kube.New(client, cfg.Kube, cfg.AgentSecrets()), Log: log}); err != nil {
		return fail(err)
	}
	return exitOK
}

func configPath(env []string) string {
	for _, kv := range env {
		if len(kv) > len(envConfig)+1 && kv[:len(envConfig)+1] == envConfig+"=" {
			return kv[len(envConfig)+1:]
		}
	}
	if _, err := os.Stat("roomcity.toml"); err == nil {
		return "roomcity.toml"
	}
	return "/etc/roomcity/roomcity.toml"
}
