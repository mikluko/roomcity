package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/require"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"go.yaml.in/yaml/v3"
)

// examples/minimal is the configuration of a deployment as an example. These
// tests keep it from rotting where that is cheap: the server loads its own
// file, the city's files parse, and the pod and the file name the same image.
const example = "../../examples/minimal"

func TestTheExamplesRoomcityTomlLoads(t *testing.T) {
	cfg, err := Load(filepath.Join(example, "k8s/files/roomcity.toml"))

	require.NoError(t, err)
	require.NotEmpty(t, cfg.Kube.Image)
	require.NotEmpty(t, cfg.Chat.Holders)
}

func TestTheExamplesCityFilesParse(t *testing.T) {
	for _, f := range []string{"city/city.toml", "city/pack.toml", "city/agents/host/agent.toml", "city/agents/guest/agent.toml", "city/agents/worker/agent.toml"} {
		b, err := os.ReadFile(filepath.Join(example, f))
		require.NoError(t, err, f)
		var v map[string]any
		require.NoError(t, toml.Unmarshal(b, &v), f)
	}
}

func TestTheExamplesPodRunsTheImageItsConfigurationNames(t *testing.T) {
	cfg, err := Load(filepath.Join(example, "k8s/files/roomcity.toml"))
	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(example, "k8s/statefulset.yaml"))
	require.NoError(t, err)
	var sts struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string `yaml:"name"`
						Image string `yaml:"image"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(b, &sts))

	got := map[string]string{}
	for _, c := range sts.Spec.Template.Spec.Containers {
		got[c.Name] = c.Image
	}
	require.Equal(t, cfg.Kube.Image, got["gc"])
	require.Equal(t, cfg.Kube.Image, got["roomcity"])
}

// newestRelease returns the version of the first level-two heading of the
// changelog that names one ("[0.3.3] - 2026-11-16"; "[Unreleased]" names none),
// read from the parsed document.
func newestRelease(t *testing.T, changelog []byte) string {
	t.Helper()
	doc := goldmark.New().Parser().Parse(text.NewReader(changelog))
	version := regexp.MustCompile(`^\[?(\d+\.\d+\.\d+)`)
	var found string
	require.NoError(t, ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		h, ok := n.(*ast.Heading)
		if !entering || !ok || h.Level != 2 || found != "" {
			return ast.WalkContinue, nil
		}
		var words []byte
		_ = ast.Walk(h, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
			if t, ok := c.(*ast.Text); ok && entering {
				words = append(words, t.Segment.Value(changelog)...)
			}
			return ast.WalkContinue, nil
		})
		if m := version.FindSubmatch(words); m != nil {
			found = string(m[1])
		}
		return ast.WalkSkipChildren, nil
	}))
	require.NotEmpty(t, found, "CHANGELOG.md names no release")
	return found
}

func TestNewestReleaseIsTheFirstVersionHeading(t *testing.T) {
	log := []byte("# Changelog\n\n## [Unreleased]\n\n## [0.3.3] - 2026-11-16\n\n### Fixed\n\n## [0.3.2] - 2026-11-16\n\n[0.3.3]: https://example.org/v0.3.3\n[0.3.2]: https://example.org/v0.3.2\n")

	require.Equal(t, "0.3.3", newestRelease(t, log))
}

// The example names the release it is known to run on: the newest in
// CHANGELOG.md, so that each release's pull request has to move it.
func TestTheExamplesImageIsTheNewestRelease(t *testing.T) {
	log, err := os.ReadFile("../../CHANGELOG.md")
	require.NoError(t, err)
	cfg, err := Load(filepath.Join(example, "k8s/files/roomcity.toml"))
	require.NoError(t, err)

	require.Equal(t, "sarak.sh/mikluko/roomcity:"+newestRelease(t, log), cfg.Kube.Image,
		"examples/minimal names the image of the newest release in CHANGELOG.md: move it in the release's pull request")
}

// admits says whether a caret range such as "^0.4" admits the version: below
// 1.0 a caret holds the minor, from 1.0 on it holds the major.
func admits(t *testing.T, rng, version string) bool {
	t.Helper()
	var rmaj, rmin, vmaj, vmin, vpat int
	_, err := fmt.Sscanf(rng, "^%d.%d", &rmaj, &rmin)
	require.NoError(t, err, "range %q", rng)
	_, err = fmt.Sscanf(version, "%d.%d.%d", &vmaj, &vmin, &vpat)
	require.NoError(t, err, "version %q", version)
	if rmaj == 0 {
		return vmaj == 0 && vmin == rmin
	}
	return vmaj == rmaj && vmin >= rmin
}

func TestAdmitsReadsACaretRange(t *testing.T) {
	require.True(t, admits(t, "^0.4", "0.4.7"))
	require.False(t, admits(t, "^0.3", "0.4.0"))
	require.True(t, admits(t, "^1.2", "1.9.0"))
	require.False(t, admits(t, "^1.2", "2.0.0"))
}

// The example imports the pack of the release it names: its range admits the
// newest release in CHANGELOG.md, as the image does. A pack of an earlier minor
// is not taken to serve a newer server; what the pack holds is one runtime
// declaration, but the range is how the two are kept together.
func TestTheExamplesPackRangeAdmitsTheNewestRelease(t *testing.T) {
	log, err := os.ReadFile("../../CHANGELOG.md")
	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(example, "city/pack.toml"))
	require.NoError(t, err)
	var pack struct {
		Imports map[string]struct {
			Version string `toml:"version"`
		} `toml:"imports"`
	}
	require.NoError(t, toml.Unmarshal(b, &pack))
	rng := pack.Imports["roomcity"].Version
	release := newestRelease(t, log)

	require.True(t, admits(t, rng, release),
		"examples/minimal/city/pack.toml imports the pack as %s, which does not admit the newest release %s: move the range in the release's pull request", rng, release)
}
