package procs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/procs"
)

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	require.Eventually(t, f, 5*time.Second, 20*time.Millisecond, what)
}

func newManager(t *testing.T) *procs.Manager {
	return &procs.Manager{LogDir: filepath.Join(t.TempDir(), "logs"), Grace: 500 * time.Millisecond}
}

func TestAProcessRunsInItsDirectoryWithItsEnvironmentAndLogsItsOutput(t *testing.T) {
	m := newManager(t)
	dir := t.TempDir()
	t.Setenv("PROCS_TEST_INHERITED", "yes")
	t.Setenv("PROCS_TEST_UNSET", "set")
	require.NoError(t, m.Start("svc", procs.Spec{
		Command: `echo "$PWD $GC_AGENT $PROCS_TEST_INHERITED [${PROCS_TEST_UNSET-unset}]"; sleep 30`,
		WorkDir: dir,
		Env:     map[string]string{"GC_AGENT": "core.dispatcher", "PROCS_TEST_UNSET": ""},
	}))
	require.True(t, m.Alive("svc"))
	require.Equal(t, []string{"svc"}, m.Running())

	logFile := filepath.Join(m.LogDir, "svc.log")
	eventually(t, "the output is logged", func() bool {
		b, _ := os.ReadFile(logFile)
		return strings.Contains(string(b), "core.dispatcher yes [unset]")
	})
	b, err := os.ReadFile(logFile)
	require.NoError(t, err)
	real, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.Contains(t, string(b), real)

	require.NoError(t, m.Stop(context.Background(), "svc"))
	require.False(t, m.Alive("svc"))
	require.Empty(t, m.Running())
}

func TestAProcessThatEndsByItselfFreesItsName(t *testing.T) {
	m := newManager(t)
	require.NoError(t, m.Start("svc", procs.Spec{Command: "exit 3", WorkDir: t.TempDir()}))
	eventually(t, "the process ends", func() bool { return !m.Alive("svc") })
	require.NoError(t, m.Start("svc", procs.Spec{Command: "sleep 30", WorkDir: t.TempDir()}))
	require.True(t, m.Alive("svc"))
	require.ErrorIs(t, m.Start("svc", procs.Spec{Command: "sleep 30", WorkDir: t.TempDir()}), procs.ErrExists)
	require.NoError(t, m.Stop(context.Background(), "svc"))
}

func TestStopEndsEverythingTheProcessStartedAndStopsAnythingThatIgnoresTerm(t *testing.T) {
	m := newManager(t)
	pids := filepath.Join(t.TempDir(), "pids")
	require.NoError(t, m.Start("svc", procs.Spec{
		Command: `trap '' TERM; sh -c 'trap "" TERM; echo $$ >> ` + pids + `; sleep 60' & echo $$ >> ` + pids + `; wait`,
		WorkDir: t.TempDir(),
	}))
	eventually(t, "both pids are written", func() bool {
		b, _ := os.ReadFile(pids)
		return strings.Count(string(b), "\n") == 2
	})

	started := time.Now()
	require.NoError(t, m.Stop(context.Background(), "svc"))
	require.Less(t, time.Since(started), 4*time.Second)
	require.False(t, m.Alive("svc"))

	b, err := os.ReadFile(pids)
	require.NoError(t, err)
	// Where there is a /proc to look in (Linux, where CI runs), no process of
	// the group lives; one that is dead and not yet reaped by init is no
	// process that lives.
	for pid := range strings.FieldsSeq(string(b)) {
		stat, err := os.ReadFile("/proc/" + pid + "/stat")
		if err != nil {
			continue
		}
		afterName := stat[strings.LastIndexByte(string(stat), ')')+2:]
		if afterName[0] != 'Z' {
			t.Errorf("process %s is still there: %s", pid, stat)
		}
	}
}

func TestAStopOfANameWithNoProcessSucceeds(t *testing.T) {
	m := newManager(t)
	require.NoError(t, m.Stop(context.Background(), "nothing"))
	require.False(t, m.Alive("nothing"))
}

func TestANameIsNotAPath(t *testing.T) {
	m := newManager(t)
	for _, name := range []string{"", ".", "..", "a/b", `a\b`} {
		require.Error(t, m.Start(name, procs.Spec{Command: "true", WorkDir: t.TempDir()}), name)
	}
}

func TestTheWorkingDirectoryIsMadeWhenNobodyMadeIt(t *testing.T) {
	m := newManager(t)
	dir := filepath.Join(t.TempDir(), ".gc", "agents", "dispatcher")
	require.NoError(t, m.Start("svc", procs.Spec{Command: "touch here; sleep 30", WorkDir: dir}))
	eventually(t, "the process runs in the directory", func() bool {
		_, err := os.Stat(filepath.Join(dir, "here"))
		return err == nil
	})
	require.NoError(t, m.Stop(context.Background(), "svc"))
}

func TestALogIsCutAtItsSizeAndKeepsTheOneBefore(t *testing.T) {
	m := newManager(t)
	m.MaxLog = 1000
	require.NoError(t, m.Start("svc", procs.Spec{
		Command: `i=0; while [ $i -lt 100 ]; do echo "line $i of the output of the service"; sleep 0.005; i=$((i+1)); done; sleep 30`,
		WorkDir: t.TempDir(),
	}))
	logFile := filepath.Join(m.LogDir, "svc.log")
	eventually(t, "the output is written", func() bool {
		b, _ := os.ReadFile(logFile)
		return strings.Contains(string(b), "line 99 ")
	})
	require.NoError(t, m.Stop(context.Background(), "svc"))

	now, err := os.Stat(logFile)
	require.NoError(t, err)
	before, err := os.Stat(logFile + ".1")
	require.NoError(t, err)
	require.LessOrEqual(t, now.Size(), int64(1000))
	require.LessOrEqual(t, before.Size(), int64(1000))
}
