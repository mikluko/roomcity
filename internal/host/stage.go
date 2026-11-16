package host

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

// stage asks the server for what gc's start of the session names on the
// server's side, extracts the files under the working directory and returns
// the manifest. A server that does not stage anything (Stage unimplemented)
// is an empty manifest. A call that fails is made again until ctx ends.
func (h *host) stage(ctx context.Context) (*roomcityv1.StageManifest, error) {
	for {
		m, err := h.fetchStage(ctx)
		switch {
		case err == nil:
			return m, nil
		case errors.As(err, new(badArchive)):
			return nil, fmt.Errorf("host: %w", err)
		case connect.CodeOf(err) == connect.CodeUnimplemented:
			return &roomcityv1.StageManifest{}, nil
		case connect.CodeOf(err) == connect.CodeUnauthenticated:
			return nil, fmt.Errorf("host: the server refused the session's token: %w", err)
		case ctx.Err() != nil:
			return nil, ctx.Err()
		}
		h.log.Warn("staging failed; trying again", "err", err)
		if !wait(ctx, h.cfg.Retry) {
			return nil, ctx.Err()
		}
	}
}

// badArchive is an archive that no new call will mend.
type badArchive struct{ error }

func (e badArchive) Unwrap() error { return e.error }

func (h *host) fetchStage(ctx context.Context) (*roomcityv1.StageManifest, error) {
	stream, err := h.client.Stage(ctx, connect.NewRequest(&roomcityv1.StageRequest{Session: h.cfg.Session}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	var manifest *roomcityv1.StageManifest
	var archive bytes.Buffer
	for stream.Receive() {
		switch part := stream.Msg().Part.(type) {
		case *roomcityv1.StageResponse_Manifest:
			if manifest != nil {
				return nil, connect.NewError(connect.CodeInternal, errors.New("a second manifest"))
			}
			manifest = part.Manifest
		case *roomcityv1.StageResponse_Archive:
			archive.Write(part.Archive)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	if manifest == nil {
		manifest = &roomcityv1.StageManifest{}
	}
	// The files are extracted only from a stream that arrived whole, so a
	// call that failed halfway leaves nothing behind to meet its retry.
	if err := extract(&archive, h.cfg.WorkDir); err != nil {
		return nil, badArchive{err}
	}
	return manifest, nil
}

// extract writes the files of the tar stream r under dir. A name that is not
// local to dir, and a link that points out of it, are refused; every step
// goes through an os.Root, which refuses a path that leaves dir by way of
// links the archive made.
func extract(r io.Reader, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("staged archive: %w", err)
		}
		name := filepath.Clean(hdr.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("staged archive: %q is outside the working directory", hdr.Name)
		}
		mode := os.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, mode|0o700)
		case tar.TypeReg:
			err = writeFile(root, name, tr, mode)
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) || !filepath.IsLocal(filepath.Join(filepath.Dir(name), hdr.Linkname)) {
				return fmt.Errorf("staged archive: link %q points out of the working directory", hdr.Name)
			}
			if err = root.MkdirAll(filepath.Dir(name), 0o755); err == nil {
				_ = root.Remove(name)
				err = root.Symlink(hdr.Linkname, name)
			}
		default:
			return fmt.Errorf("staged archive: %q is of a kind that is not staged (%c)", hdr.Name, hdr.Typeflag)
		}
		if err != nil {
			return fmt.Errorf("staged archive: %s: %w", hdr.Name, err)
		}
	}
}

func writeFile(root *os.Root, name string, r io.Reader, mode os.FileMode) error {
	if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	_ = root.Remove(name)
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// runCommands runs each command with sh -c in the working directory, with
// the host's environment. It returns the first failure when fatal is set,
// and logs failures otherwise.
func (h *host) runCommands(ctx context.Context, commands []string, fatal bool) error {
	for _, c := range commands {
		if err := h.sh(ctx, "-c", c); err != nil {
			if fatal {
				return fmt.Errorf("host: %q: %w", c, err)
			}
			h.log.Warn("a setup command failed", "command", c, "err", err)
		}
	}
	return nil
}

// runScript runs script's text with sh, as a failing setup command does.
func (h *host) runScript(ctx context.Context, script string) {
	if script == "" {
		return
	}
	cmd := exec.CommandContext(ctx, "sh", "-s")
	cmd.Dir = h.cfg.WorkDir
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		h.log.Warn("the setup script failed", "err", err, "output", string(out))
	}
}

func (h *host) sh(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "sh", args...)
	cmd.Dir = h.cfg.WorkDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
