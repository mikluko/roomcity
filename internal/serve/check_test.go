package serve_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/config"
	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
	"sarak.sh/mikluko/roomcity/internal/operate"
	"sarak.sh/mikluko/roomcity/internal/serve"
)

// gcSuite runs one of gc's own conformance commands against the built
// command and a server on real listeners whose pods are goroutines. It runs
// when ROOMCITY_GC names a gc, the image's for instance:
//
//	ROOMCITY_GC=/usr/local/bin/gc go test ./internal/serve -run TestGc -v
func gcSuite(t *testing.T, command string) {
	gc := os.Getenv("ROOMCITY_GC")
	if gc == "" {
		t.Skip("ROOMCITY_GC does not name a gc")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	bin := filepath.Join(t.TempDir(), "gc-runtime-room")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "../../cmd/roomcity")
	build.Stderr = os.Stderr
	require.NoError(t, build.Run())

	ls := serve.Listeners{Network: listen(t), Runtime: listen(t), Page: listen(t)}
	cfg, err := config.Parse([]byte(`
[server]
data = "` + filepath.Join(t.TempDir(), "data") + `"
city_dir = "` + t.TempDir() + `"
url = "http://` + ls.Network.Addr().String() + `"
start_timeout = "30s"

[kube]
namespace = "sandbox"
image = "roomcity"
`))
	require.NoError(t, err)

	prompts := make(chan *roomcityv1.AttachResponse, 64)
	go func() {
		for range prompts {
		}
	}()
	staged := make(chan *roomcityv1.StageManifest, 64)
	go func() {
		for range staged {
		}
	}()
	j := &jobs{alive: map[string]bool{}}
	j.fn = func(spec operate.JobSpec) { pod(t, ctx, spec, prompts, staged) }

	runCtx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- serve.Run(runCtx, serve.Options{Config: cfg, Listeners: ls, Jobs: j}) }()

	check := exec.CommandContext(ctx, gc, "runtime", command, bin)
	check.Env = append(os.Environ(), "ROOMCITY_RUNTIME_URL=http://"+ls.Runtime.Addr().String())
	var out bytes.Buffer
	check.Stdout, check.Stderr = &out, &out
	err = check.Run()
	t.Logf("gc runtime %s:\n%s", command, out.String())
	require.NoError(t, err)

	stop()
	require.NoError(t, <-served)
}

func TestGcRuntimeCheck(t *testing.T) { gcSuite(t, "check") }

func TestGcRuntimeConformance(t *testing.T) { gcSuite(t, "conformance") }
