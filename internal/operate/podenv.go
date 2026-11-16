package operate

import (
	"regexp"
	"slices"
	"strings"
)

// podOwn are the variables of gc's own environment that name the supervisor's
// process, its user and its disk, and so cannot be true in a session's pod:
// the pod has its own.
var podOwn = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "PWD", "OLDPWD", "TMPDIR", "TMP", "TEMP", "HOSTNAME", "TERM", "LANG", "LANGUAGE",
}

// podOwnPrefixes are those of the supervisor's own that come by prefix: the
// directories of the XDG specification, which the supervisor's home holds, the
// service variables of the supervisor's pod, and the locale (LC_*, with LANG
// and LANGUAGE above): gc sends LANG=en_US.UTF-8 when its own environment
// names none, and the image has only C, C.utf8 and POSIX, so that every perl
// run in the agent's shell would warn that setting the locale failed.
var podOwnPrefixes = []string{"XDG_", "KUBERNETES_", "LC_"}

// knownSecrets are variables gc sets for the agent that are secrets, held back
// by their full names, not by a pattern: a secret reaches a pod as one of the
// Secrets of roomcity.toml, as bd's credentials do through
// BEADS_CREDENTIALS_FILE, and a Job's spec is readable to whoever may read
// Jobs.
var knownSecrets = []string{"GC_DOLT_PASSWORD"}

var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// podEnv is the environment of a session's pod: what the server sets for it,
// then everything gc set for the agent, less what the pod has of its own. A
// variable the server sets (ROOMCITY_*) is its own and wins; a value that is
// empty is gc asking for the variable to be unset, which a pod that has not
// got it already satisfies. heldBack names what gc set and the pod does not
// get, sorted: the pod's own, and the known secrets.
func podEnv(server map[string]string, gc map[string]string) (env map[string]string, heldBack []string) {
	env = make(map[string]string, len(server)+len(gc))
	for k, v := range gc {
		switch {
		case v == "":
			continue
		case !variableName.MatchString(k), strings.HasPrefix(k, "ROOMCITY_"), podOwnName(k), slices.Contains(knownSecrets, k):
			heldBack = append(heldBack, k)
			continue
		}
		env[k] = v
	}
	for k, v := range server {
		env[k] = v
	}
	slices.Sort(heldBack)
	return env, heldBack
}

func podOwnName(k string) bool {
	if slices.Contains(podOwn, k) {
		return true
	}
	for _, p := range podOwnPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}
