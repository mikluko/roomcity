package operate

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/shlex"
	"google.golang.org/protobuf/proto"

	roomcityv1 "sarak.sh/mikluko/roomcity/internal/gen/roomcity/v1"
)

// settingsInPod is where the file gc's --settings names is put in the
// session's working directory.
const settingsInPod = ".roomcity/settings.json"

// copyEntry is a file or directory to copy into the working directory.
type copyEntry struct {
	Src    string `json:"src"`
	RelDst string `json:"rel_dst,omitempty"`
}

// startConfig is the JSON gc sends to a runtime's start, as far as the
// server reads it.
type startConfig struct {
	WorkDir            string            `json:"work_dir,omitempty"`
	Command            string            `json:"command,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	ProcessNames       []string          `json:"process_names,omitempty"`
	Nudge              string            `json:"nudge,omitempty"`
	PreStart           []string          `json:"pre_start,omitempty"`
	SessionSetup       []string          `json:"session_setup,omitempty"`
	SessionSetupScript string            `json:"session_setup_script,omitempty"`
	SessionLive        []string          `json:"session_live,omitempty"`
	PackOverlayDirs    []string          `json:"pack_overlay_dirs,omitempty"`
	OverlayDir         string            `json:"overlay_dir,omitempty"`
	CopyFiles          []copyEntry       `json:"copy_files,omitempty"`
}

// notCarriedOutForAProcess names what the start config holds that the start
// of a process does not carry out.
func (sc startConfig) notCarriedOutForAProcess() []string {
	var out []string
	for name, set := range map[string]bool{
		"pre_start":            len(sc.PreStart) > 0,
		"session_setup":        len(sc.SessionSetup) > 0,
		"session_setup_script": sc.SessionSetupScript != "",
		"session_live":         len(sc.SessionLive) > 0,
		"pack_overlay_dirs":    len(sc.PackOverlayDirs) > 0,
		"overlay_dir":          sc.OverlayDir != "",
		"copy_files":           len(sc.CopyFiles) > 0,
		"nudge":                sc.Nudge != "",
	} {
		if set {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func parseStartConfig(b []byte) (startConfig, error) {
	var sc startConfig
	if len(bytes.TrimSpace(b)) == 0 {
		return sc, fmt.Errorf("start config: empty")
	}
	if err := json.Unmarshal(b, &sc); err != nil {
		return sc, fmt.Errorf("start config: %w", err)
	}
	return sc, nil
}

// bundle builds what the session host fetches with Stage: the manifest,
// and the archive of the files that gc's start config names on the
// supervisor's disk. The archive's names are relative to the working
// directory.
func (sc startConfig) bundle() (manifest, archive []byte, err error) {
	files := map[string]entry{}
	add := func(src, dstDir string) error { return collect(files, src, dstDir) }
	for _, d := range sc.PackOverlayDirs {
		if err := add(d, ""); err != nil {
			return nil, nil, err
		}
	}
	if sc.OverlayDir != "" {
		if err := add(sc.OverlayDir, ""); err != nil {
			return nil, nil, err
		}
	}
	for _, c := range sc.CopyFiles {
		dst := c.RelDst
		if dst == "" {
			dst = filepath.Base(c.Src)
		}
		if !filepath.IsLocal(dst) {
			return nil, nil, fmt.Errorf("copy_files: %q is outside the working directory", dst)
		}
		if err := collect(files, c.Src, filepath.ToSlash(filepath.Clean(dst))); err != nil {
			return nil, nil, err
		}
	}

	words, err := shlex.Split(sc.Command)
	if err != nil {
		return nil, nil, fmt.Errorf("command: %w", err)
	}
	words, settings := replaceSettings(words, path.Join(sc.WorkDir, settingsInPod))
	if settings != "" {
		b, err := os.ReadFile(settings)
		if err != nil {
			return nil, nil, fmt.Errorf("settings file: %w", err)
		}
		files[settingsInPod] = entry{content: b, mode: 0o600}
	}

	m := &roomcityv1.StageManifest{
		Command:      words,
		PreStart:     sc.PreStart,
		SessionSetup: sc.SessionSetup,
		SessionLive:  sc.SessionLive,
	}
	if sc.SessionSetupScript != "" {
		b, err := os.ReadFile(sc.SessionSetupScript)
		if err != nil {
			return nil, nil, fmt.Errorf("session_setup_script: %w", err)
		}
		m.SessionSetupScript = string(b)
	}
	manifest, err = proto.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	archive, err = writeTar(files)
	return manifest, archive, err
}

// replaceSettings returns words with the value of --settings replaced by
// pod, and the value it replaced, "" when there is none.
func replaceSettings(words []string, pod string) ([]string, string) {
	out := slices.Clone(words)
	for i, w := range out {
		switch {
		case w == "--settings" && i+1 < len(out):
			old := out[i+1]
			out[i+1] = pod
			return out, old
		case strings.HasPrefix(w, "--settings="):
			old := strings.TrimPrefix(w, "--settings=")
			out[i] = "--settings=" + pod
			return out, old
		}
	}
	return out, ""
}

// entry is one archive member. A directory has no content and no link.
type entry struct {
	content []byte
	link    string
	mode    fs.FileMode
	dir     bool
}

// collect adds src, a file or the tree of a directory, to files under dst
// ("" puts a directory's contents at the top). A later add of the same name
// replaces the earlier. Entries that are neither directories, regular
// files nor links that stay inside the tree are left out.
func collect(files map[string]entry, src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if dst == "" {
			dst = filepath.Base(src)
		}
		files[dst] = entry{content: b, mode: info.Mode().Perm()}
		return nil
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		name := path.Join(dst, filepath.ToSlash(rel))
		if rel == "." {
			if dst != "" {
				files[dst] = entry{dir: true, mode: 0o755}
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			files[name] = entry{dir: true, mode: info.Mode().Perm()}
		case info.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			files[name] = entry{content: b, mode: info.Mode().Perm()}
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(link) && filepath.IsLocal(filepath.Join(filepath.Dir(rel), link)) {
				files[name] = entry{link: link}
			}
		}
		return nil
	})
}

func writeTar(files map[string]entry) ([]byte, error) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range names {
		e := files[n]
		hdr := &tar.Header{Name: n, Mode: int64(e.mode)}
		switch {
		case e.dir:
			hdr.Typeflag = tar.TypeDir
			hdr.Name += "/"
		case e.link != "":
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = e.link
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(e.content); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
