package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"sarak.sh/mikluko/roomcity/internal/config"
)

func TestResetAsksGcForTheSessionBeadInTheCityDirectory(t *testing.T) {
	dir := t.TempDir()
	gc := filepath.Join(t.TempDir(), "gc")
	require.NoError(t, os.WriteFile(gc, []byte("#!/bin/sh\npwd -P > \"$0.pwd\"\nprintf '%s ' \"$@\" > \"$0.args\"\n[ \"$3\" != refused ] || { echo 'no such session' >&2; exit 4; }\n"), 0o755))
	cfg := config.Config{Server: config.Server{Gc: gc, CityDir: dir}}

	require.NoError(t, resetter(cfg)(context.Background(), "sa-wisp-1tmb"))
	args, err := os.ReadFile(gc + ".args")
	require.NoError(t, err)
	require.Equal(t, "session reset sa-wisp-1tmb ", string(args))
	pwd, err := os.ReadFile(gc + ".pwd")
	require.NoError(t, err)
	real, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.Equal(t, real+"\n", string(pwd))

	err = resetter(cfg)(context.Background(), "refused")
	require.ErrorContains(t, err, "gc session reset refused")
	require.ErrorContains(t, err, "no such session", "what gc said is in the error")
}
