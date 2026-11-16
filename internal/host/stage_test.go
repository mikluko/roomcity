package host

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/claudeacp/claudeacp"
	"sarak.sh/mikluko/claudeacp/claudeagent"
	"sarak.sh/mikluko/claudeacp/claudeagent/claudetest"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

type entry struct {
	name, body, link string
	dir              bool
}

func archiveOf(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		switch {
		case e.dir:
			h.Typeflag, h.Mode, h.Size = tar.TypeDir, 0o755, 0
		case e.link != "":
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.link, 0
		}
		require.NoError(t, tw.WriteHeader(h))
		_, err := tw.Write([]byte(e.body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

func TestStageFilesCommandsAndArguments(t *testing.T) {
	work := t.TempDir()
	server, url := newFakeServer(t)
	server.stageWith(&roomcityv1.StageManifest{
		Command:            []string{"claude", "--settings", filepath.Join(work, ".gc", "settings.json")},
		PreStart:           []string{"echo pre > pre.txt"},
		SessionSetup:       []string{"echo setup > setup.txt", "false"},
		SessionSetupScript: "echo script > script.txt\n",
		SessionLive:        []string{"echo live > live.txt"},
	}, archiveOf(t,
		entry{name: ".gc/", dir: true},
		entry{name: ".gc/settings.json", body: `{"hooks":{}}`},
		entry{name: "AGENTS.md", body: "role"},
		entry{name: "latest", link: "AGENTS.md"},
	))
	spawned := make(chan claudeagent.Spec, 1)
	f := claude()
	cfg := Config{
		Session: "s", Token: testToken, Server: url, WorkDir: work,
		Claude: claudeacp.Config{CLIPath: "/claude", ConfigDir: t.TempDir(), Spawn: func(ctx context.Context, s claudeagent.Spec) (*claudeagent.Proc, error) {
			spawned <- s
			return f.Spawn(ctx, s)
		}},
		Retry: time.Millisecond,
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- Run(ctx, cfg) }()

	// Claude is spawned at the first session, so ask for one by a prompt.
	server.instruct(prompt("hi"))
	var spec claudeagent.Spec
	select {
	case spec = <-spawned:
	case <-time.After(10 * time.Second):
		t.Fatal("claude never started")
	}
	require.NotContains(t, spec.Args, "claude", "the program of the command is not an argument")
	require.Contains(t, spec.Args, "--settings")
	require.Contains(t, spec.Args, filepath.Join(work, ".gc", "settings.json"))
	for file, want := range map[string]string{
		".gc/settings.json": `{"hooks":{}}`, "AGENTS.md": "role", "latest": "role",
		"pre.txt": "pre\n", "setup.txt": "setup\n", "script.txt": "script\n", "live.txt": "live\n",
	} {
		// The setup commands run once the session is up, which is after
		// claude was spawned.
		require.Eventually(t, func() bool {
			got, err := os.ReadFile(filepath.Join(work, file))
			return err == nil && string(got) == want
		}, 10*time.Second, time.Millisecond, file)
	}
	require.Equal(t, 1, server.stageCalls())
	cancel()
	<-done
}

func TestStageRefusesAnArchiveThatLeavesTheWorkingDirectory(t *testing.T) {
	for name, es := range map[string][]entry{
		"a name outside":   {{name: "../escape", body: "x"}},
		"an absolute name": {{name: "/etc/escape", body: "x"}},
		"a link out":       {{name: "out", link: "../../etc"}},
		"links through links": {
			{name: "d/", dir: true},
			{name: "d/l", link: ".."},
			{name: "d/l/m", link: ".."},
			{name: "d/l/m/evil", body: "x"},
		},
		"an absolute link": {{name: "abs", link: "/etc"}},
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			work := filepath.Join(parent, "work")
			require.NoError(t, os.Mkdir(work, 0o755))
			server, url := newFakeServer(t)
			server.stageWith(&roomcityv1.StageManifest{}, archiveOf(t, es...))
			f := claudetest.New()

			err := Run(context.Background(), Config{
				Session: "s", Token: testToken, Server: url, WorkDir: work,
				Claude: claudeacp.Config{CLIPath: "/claude", ConfigDir: t.TempDir(), Spawn: f.Spawn}, Retry: time.Millisecond,
			})

			require.ErrorContains(t, err, "staged archive")
			// What came before the refused entry may stay; nothing lands
			// outside the working directory.
			outside, _ := os.ReadDir(parent)
			require.Len(t, outside, 1)
			require.Equal(t, "work", outside[0].Name())
		})
	}
}

func TestFailingPreStartEndsTheHostBeforeClaudeStarts(t *testing.T) {
	server, url := newFakeServer(t)
	server.stageWith(&roomcityv1.StageManifest{PreStart: []string{"exit 3"}}, nil)
	started := false
	err := Run(context.Background(), Config{
		Session: "s", Token: testToken, Server: url, WorkDir: t.TempDir(),
		Claude: claudeacp.Config{CLIPath: "/claude", ConfigDir: t.TempDir(), Spawn: func(context.Context, claudeagent.Spec) (*claudeagent.Proc, error) {
			started = true
			return nil, os.ErrInvalid
		}},
		Retry: time.Millisecond,
	})

	require.ErrorContains(t, err, "exit 3")
	require.False(t, started)
}
