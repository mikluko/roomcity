package config

import (
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
