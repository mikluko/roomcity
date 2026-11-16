// Package kube is the Kubernetes side of a session: one Job in the server's
// own namespace for each session.
package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"sarak.sh/mikluko/roomcity/internal/operate"
)

const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedBy      = "roomcity"
	labelSession   = "roomcity.sarak.sh/session-id"
	annotSession   = "roomcity.sarak.sh/session"
	annotAgent     = "roomcity.sarak.sh/agent"
	containerName  = "host"
)

// EnvSecretPrefix starts the name of a variable that tells the session host
// which environment variable of the agent a mounted file is the value of:
// ROOMCITY_SECRET_<VARIABLE> holds the path of the file.
const EnvSecretPrefix = "ROOMCITY_SECRET_"

// Secret is a Secret of the namespace mounted in each session's pod. A
// credential is a file in the pod and never a variable of the pod's spec.
type Secret struct {
	// Name is the Secret.
	Name string `toml:"name"`
	// Key, when set, mounts that key alone as the file at MountPath; else
	// the Secret's keys are the files of the directory MountPath.
	Key       string `toml:"key"`
	MountPath string `toml:"mount_path"`
	// Env, when set, names the variable the agent gets the file's content
	// in. The Job carries EnvSecretPrefix+Env with the file's path, and the
	// host reads the file. It needs Key: a variable has one value.
	Env string `toml:"env"`
	// PathEnv, when set, names a variable the pod's environment holds the
	// path of the mounted file in, for a program that takes its file from
	// one (bd's BEADS_CREDENTIALS_FILE). It needs Key.
	PathEnv string `toml:"path_env"`
}

var envName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// Validate reports the first thing wrong with the Secret.
func (s Secret) Validate() error {
	switch {
	case s.Name == "" || s.MountPath == "":
		return fmt.Errorf("a name and a mount_path are required")
	case s.Env != "" && s.Key == "":
		return fmt.Errorf("env %q needs a key", s.Env)
	case s.Env != "" && !envName.MatchString(s.Env):
		return fmt.Errorf("env %q is not a variable name of capitals, digits and underscores", s.Env)
	case s.PathEnv != "" && s.Key == "":
		return fmt.Errorf("path_env %q needs a key", s.PathEnv)
	case s.PathEnv != "" && !envName.MatchString(s.PathEnv):
		return fmt.Errorf("path_env %q is not a variable name of capitals, digits and underscores", s.PathEnv)
	}
	return nil
}

// DefaultHome is the home of the user of the image.
const DefaultHome = "/home/city"

func (c Config) home() string {
	if c.Home != "" {
		return c.Home
	}
	return DefaultHome
}

// CheckMount refuses a Secret mounted inside the session's home. The
// kubelet makes the directories above a file it mounts, as root and not
// writable by the user, so a Secret at ~/.config/beads/credentials makes
// ~/.config the user's no more: an agent that makes its own directory there
// fails. Mount the Secret outside the home, and point the program at it with
// path_env.
func (c Config) CheckMount(s Secret) error {
	home := path.Clean(c.home())
	mount := path.Clean(s.MountPath)
	if mount == home || strings.HasPrefix(mount, home+"/") {
		return fmt.Errorf("%s is inside the home %s, where the mount would make its parent directories root's: mount it elsewhere and name path_env", s.MountPath, home)
	}
	return nil
}

// Toleration lets a pod onto the nodes with a matching taint.
type Toleration struct {
	Key      string `toml:"key"`
	Operator string `toml:"operator"`
	Value    string `toml:"value"`
	Effect   string `toml:"effect"`
}

// Config is how a session's Job is made.
type Config struct {
	Namespace string `toml:"namespace"`
	// Image runs the session host: the image of the server itself.
	Image string `toml:"image"`
	// Home is the home directory of the user of the image; "" is /home/city.
	Home string `toml:"home"`
	// RunAsUser is the user the pod runs as; 0 is 1000, the image's own.
	RunAsUser int64 `toml:"run_as_user"`
	// Secrets are mounted in every session's pod; an agent's own are in
	// its table of the configuration.
	Secrets []Secret `toml:"secrets"`
	// NodeSelector and Tolerations put the pods on the nodes the sessions
	// belong on.
	NodeSelector map[string]string `toml:"node_selector"`
	Tolerations  []Toleration      `toml:"tolerations"`
	// CPURequest and MemoryRequest are what a pod asks for, such as "250m"
	// and "512Mi"; "" asks for nothing.
	CPURequest    string `toml:"cpu_request"`
	MemoryRequest string `toml:"memory_request"`
}

// Validate reports the first thing wrong with the configuration.
func (c Config) Validate() error {
	for _, q := range []string{c.CPURequest, c.MemoryRequest} {
		if q == "" {
			continue
		}
		if _, err := resource.ParseQuantity(q); err != nil {
			return fmt.Errorf("kube: %w", err)
		}
	}
	return nil
}

// defaultUser is the user of the image.
const defaultUser = 1000

// podPath puts the stand-in gc's directory ahead of the image's PATH, which
// has none of its own: the agent and the hooks call gc by name and must find
// the stand-in, not the real gc.
const podPath = "/usr/local/lib/roomcity/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Jobs implements operate.Jobs on a Kubernetes cluster.
type Jobs struct {
	client kubernetes.Interface
	cfg    Config
	agent  map[string][]Secret
}

var _ operate.Jobs = (*Jobs)(nil)

// New returns the Jobs of the namespace cfg names. A session's pod gets the
// Secrets of cfg and those agent holds for its agent, and no other agent's.
func New(client kubernetes.Interface, cfg Config, agent map[string][]Secret) *Jobs {
	return &Jobs{client: client, cfg: cfg, agent: agent}
}

// InCluster returns the client of the cluster the server runs in, with the
// permissions of its pod's service account.
func InCluster() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("kube: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}

var unsafe = regexp.MustCompile(`[^a-z0-9]+`)

// id is what names a session's Job and labels it: a label value is at most
// 63 characters of a narrower set than a session's name.
func id(session string) string {
	sum := sha256.Sum256([]byte(session))
	stem := strings.Trim(unsafe.ReplaceAllString(strings.ToLower(session), "-"), "-")
	if len(stem) > 30 {
		stem = strings.TrimRight(stem[:30], "-")
	}
	if stem == "" {
		stem = "s"
	}
	return stem + "-" + hex.EncodeToString(sum[:4])
}

func jobName(session string) string { return "roomcity-" + id(session) }

// Create makes the session's Job, which runs the session host once.
func (j *Jobs) Create(ctx context.Context, spec operate.JobSpec) error {
	_, err := j.client.BatchV1().Jobs(j.cfg.Namespace).Create(ctx, j.job(spec), metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("a Job of session %s exists", spec.Session)
	}
	return err
}

func (j *Jobs) job(spec operate.JobSpec) *batchv1.Job {
	names := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		names = append(names, k)
	}
	sort.Strings(names)
	env := make([]corev1.EnvVar, 0, len(names)+1)
	for _, k := range names {
		if k != "PATH" {
			env = append(env, corev1.EnvVar{Name: k, Value: spec.Env[k]})
		}
	}
	env = append(env, corev1.EnvVar{Name: "PATH", Value: podPath})

	mounts := []corev1.VolumeMount{{Name: "work", MountPath: spec.WorkDir}}
	volumes := []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	for i, s := range append(slices.Clone(j.cfg.Secrets), j.agent[spec.Agent]...) {
		vol := fmt.Sprintf("secret-%d", i)
		src := &corev1.SecretVolumeSource{SecretName: s.Name}
		m := corev1.VolumeMount{Name: vol, MountPath: s.MountPath, ReadOnly: true}
		if s.Key != "" {
			src.Items = []corev1.KeyToPath{{Key: s.Key, Path: s.Key}}
			m.SubPath = s.Key
		}
		volumes = append(volumes, corev1.Volume{Name: vol, VolumeSource: corev1.VolumeSource{Secret: src}})
		mounts = append(mounts, m)
		if s.Env != "" {
			env = append(env, corev1.EnvVar{Name: EnvSecretPrefix + s.Env, Value: s.MountPath})
		}
		if s.PathEnv != "" {
			env = append(env, corev1.EnvVar{Name: s.PathEnv, Value: s.MountPath})
		}
	}

	labels := map[string]string{labelManagedBy: managedBy, labelSession: id(spec.Session)}
	user := j.cfg.RunAsUser
	if user == 0 {
		user = defaultUser
	}
	zero, noToken, yes, no := int32(0), false, true, false
	var requests corev1.ResourceList
	if j.cfg.CPURequest != "" || j.cfg.MemoryRequest != "" {
		requests = corev1.ResourceList{}
		if j.cfg.CPURequest != "" {
			requests[corev1.ResourceCPU] = resource.MustParse(j.cfg.CPURequest)
		}
		if j.cfg.MemoryRequest != "" {
			requests[corev1.ResourceMemory] = resource.MustParse(j.cfg.MemoryRequest)
		}
	}
	var tolerations []corev1.Toleration
	for _, t := range j.cfg.Tolerations {
		tolerations = append(tolerations, corev1.Toleration{
			Key: t.Key, Operator: corev1.TolerationOperator(t.Operator), Value: t.Value, Effect: corev1.TaintEffect(t.Effect),
		})
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        jobName(spec.Session),
			Namespace:   j.cfg.Namespace,
			Labels:      labels,
			Annotations: map[string]string{annotSession: spec.Session, annotAgent: spec.Agent},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &zero,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &noToken,
					NodeSelector:                 j.cfg.NodeSelector,
					Tolerations:                  tolerations,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser:      &user,
						RunAsNonRoot:   &yes,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:         containerName,
						Image:        j.cfg.Image,
						Command:      []string{"/usr/bin/dumb-init", "--", "/usr/local/bin/roomcity", "host"},
						WorkingDir:   spec.WorkDir,
						Env:          env,
						VolumeMounts: mounts,
						Resources:    corev1.ResourceRequirements{Requests: requests},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &no,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// Delete removes the session's Job and, with it, its pod.
func (j *Jobs) Delete(ctx context.Context, session string) error {
	policy := metav1.DeletePropagationBackground
	err := j.client.BatchV1().Jobs(j.cfg.Namespace).Delete(ctx, jobName(session), metav1.DeleteOptions{PropagationPolicy: &policy})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// Alive reports whether the session has a Job that has neither failed nor
// completed.
func (j *Jobs) Alive(ctx context.Context, session string) (bool, error) {
	job, err := j.client.BatchV1().Jobs(j.cfg.Namespace).Get(ctx, jobName(session), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return !finished(job), nil
}

// Running lists the sessions whose Jobs have neither failed nor completed.
func (j *Jobs) Running(ctx context.Context) ([]string, error) {
	list, err := j.client.BatchV1().Jobs(j.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=" + managedBy})
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range list.Items {
		if name := list.Items[i].Annotations[annotSession]; name != "" && !finished(&list.Items[i]) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}

func info(job *batchv1.Job) operate.JobInfo {
	return operate.JobInfo{
		Session: job.Annotations[annotSession],
		ID:      string(job.UID),
		Created: job.CreationTimestamp.Time,
		Alive:   !finished(job),
	}
}

// Info describes the session's Job; the zero JobInfo is no Job.
func (j *Jobs) Info(ctx context.Context, session string) (operate.JobInfo, bool, error) {
	job, err := j.client.BatchV1().Jobs(j.cfg.Namespace).Get(ctx, jobName(session), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return operate.JobInfo{}, false, nil
	case err != nil:
		return operate.JobInfo{}, false, err
	}
	return info(job), true, nil
}

// List describes every Job of the namespace that is a session's, those
// that have failed or completed included, with one read of the cluster.
func (j *Jobs) List(ctx context.Context) ([]operate.JobInfo, error) {
	list, err := j.client.BatchV1().Jobs(j.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=" + managedBy})
	if err != nil {
		return nil, err
	}
	var out []operate.JobInfo
	for i := range list.Items {
		if in := info(&list.Items[i]); in.Session != "" {
			out = append(out, in)
		}
	}
	return out, nil
}

// DeleteObject deletes the session's Job if it is the one with that id, and
// reports whether it did: a Job that is gone, or that is another's under the
// name, is left as it is. The cluster refuses the delete of a Job whose UID
// is not the one asked for.
func (j *Jobs) DeleteObject(ctx context.Context, session, id string) (bool, error) {
	policy := metav1.DeletePropagationBackground
	uid := types.UID(id)
	// The Job is read first, and the delete still carries the UID, so that a
	// Job made under the name between the two is not the one deleted.
	cur, err := j.client.BatchV1().Jobs(j.cfg.Namespace).Get(ctx, jobName(session), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, err
	case cur.UID != uid:
		return false, nil
	}
	err = j.client.BatchV1().Jobs(j.cfg.Namespace).Delete(ctx, jobName(session), metav1.DeleteOptions{
		PropagationPolicy: &policy,
		Preconditions:     &metav1.Preconditions{UID: &uid},
	})
	switch {
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// failureLogLines is how much of the end of a pod's log Failure reports.
const failureLogLines = 15

// Failure says why the session's pod ended: its state as the cluster reports
// it, and the last lines of its log.
func (j *Jobs) Failure(ctx context.Context, session string) (string, error) {
	pods, err := j.client.CoreV1().Pods(j.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSession + "=" + id(session)})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "no pod was made for it", nil
	}
	pod := pods.Items[len(pods.Items)-1]
	var why []string
	if pod.Status.Reason != "" || pod.Status.Message != "" {
		why = append(why, strings.TrimSpace(pod.Status.Reason+" "+pod.Status.Message))
	}
	for _, c := range pod.Status.ContainerStatuses {
		switch {
		case c.State.Terminated != nil:
			t := c.State.Terminated
			why = append(why, fmt.Sprintf("%s exited %d %s %s", c.Name, t.ExitCode, t.Reason, t.Message))
		case c.State.Waiting != nil:
			why = append(why, fmt.Sprintf("%s waits: %s %s", c.Name, c.State.Waiting.Reason, c.State.Waiting.Message))
		}
	}
	out := strings.TrimSpace(strings.Join(why, "; "))
	if out == "" {
		out = "pod " + pod.Name + " is " + string(pod.Status.Phase)
	}
	tail := int64(failureLogLines)
	log, err := j.client.CoreV1().Pods(j.cfg.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{TailLines: &tail}).DoRaw(ctx)
	if err == nil && len(strings.TrimSpace(string(log))) > 0 {
		out += "\nits log ends:\n" + strings.TrimRight(string(log), "\n")
	}
	return out, nil
}

func finished(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
