package kube_test

import (
	"context"
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"sarak.sh/mikluko/roomcity/internal/kube"
	"sarak.sh/mikluko/roomcity/internal/operate"
)

func newJobs() (*kube.Jobs, *fake.Clientset) {
	c := fake.NewSimpleClientset()
	return kube.New(c, kube.Config{
		Namespace: "sandbox",
		Image:     "sarak.sh/mikluko/roomcity:0.2.0",
		RunAsUser: 1000,
		Secrets: []kube.Secret{
			{Name: "dolt", Key: "credentials", MountPath: "/run/roomcity/secrets/dolt/credentials", PathEnv: "BEADS_CREDENTIALS_FILE"},
		},
		NodeSelector:  map[string]string{"kubernetes.io/hostname": "bulgakov"},
		Tolerations:   []kube.Toleration{{Key: "sarak.sh/workstation", Operator: "Exists", Effect: "NoSchedule"}},
		CPURequest:    "250m",
		MemoryRequest: "512Mi",
	}, map[string][]kube.Secret{
		"worker": {{Name: "claude", Key: "token", MountPath: "/run/roomcity/secrets/claude/token"}},
		"guest":  {{Name: "kimi", Key: "api-key", MountPath: "/run/roomcity/secrets/kimi/api-key"}},
	}), c
}

func podOf(t *testing.T, jobs *kube.Jobs, c *fake.Clientset, spec operate.JobSpec) corev1.PodSpec {
	t.Helper()
	require.NoError(t, jobs.Create(context.Background(), spec))
	list, err := c.BatchV1().Jobs("sandbox").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	return list.Items[0].Spec.Template.Spec
}

func mountPaths(pod corev1.PodSpec) map[string]corev1.VolumeMount {
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range pod.Containers[0].VolumeMounts {
		mounts[m.MountPath] = m
	}
	return mounts
}

func TestCreateMakesAJobThatRunsTheHost(t *testing.T) {
	ctx := context.Background()
	jobs, c := newJobs()
	require.NoError(t, jobs.Create(ctx, operate.JobSpec{
		Session: "Worker_1", Agent: "worker", WorkDir: "/city/.gc/agents/worker",
		Env: map[string]string{"ROOMCITY_SESSION": "Worker_1", "ROOMCITY_TOKEN": "t", "PATH": "/city/bin"},
	}))

	list, err := c.BatchV1().Jobs("sandbox").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	job := list.Items[0]
	require.True(t, strings.HasPrefix(job.Name, "roomcity-worker-1-"), job.Name)
	require.Equal(t, "Worker_1", job.Annotations["roomcity.sarak.sh/session"])
	require.Zero(t, *job.Spec.BackoffLimit)

	pod := job.Spec.Template.Spec
	require.Equal(t, corev1.RestartPolicyNever, pod.RestartPolicy)
	require.False(t, *pod.AutomountServiceAccountToken)
	ctr := pod.Containers[0]
	require.Equal(t, []string{"/usr/bin/dumb-init", "--", "/usr/local/bin/roomcity", "host"}, ctr.Command)
	require.Equal(t, "/city/.gc/agents/worker", ctr.WorkingDir)
	require.Contains(t, ctr.Env, corev1.EnvVar{Name: "ROOMCITY_TOKEN", Value: "t"})
	for _, e := range ctr.Env {
		require.NotContains(t, e.Name, "SECRET", "a credential is a file, not a variable")
	}
	require.Contains(t, mountPaths(pod), "/city/.gc/agents/worker")
	require.Error(t, jobs.Create(ctx, operate.JobSpec{Session: "Worker_1"}), "a second Job of the same session")
}

func TestThePodGetsOnlyItsAgentsSecrets(t *testing.T) {
	for agent, want := range map[string]string{"worker": "/run/roomcity/secrets/claude/token", "guest": "/run/roomcity/secrets/kimi/api-key"} {
		jobs, c := newJobs()
		mounts := mountPaths(podOf(t, jobs, c, operate.JobSpec{Session: agent + "-1", Agent: agent, WorkDir: "/w"}))
		require.Contains(t, mounts, want, agent)
		require.Len(t, mounts, 3, "the work directory, bd's credentials and the agent's own: %v", agent)
		creds := mounts["/run/roomcity/secrets/dolt/credentials"]
		require.Equal(t, "credentials", creds.SubPath)
		require.True(t, creds.ReadOnly)
	}

	jobs, c := newJobs()
	mounts := mountPaths(podOf(t, jobs, c, operate.JobSpec{Session: "other-1", Agent: "other", WorkDir: "/w"}))
	require.Len(t, mounts, 2, "an agent that names none gets bd's credentials alone")
}

func TestASecretThatNamesAVariableIsHandedToTheHostAsAPath(t *testing.T) {
	c := fake.NewSimpleClientset()
	jobs := kube.New(c, kube.Config{Namespace: "sandbox", Image: "i"}, map[string][]kube.Secret{
		"worker": {{Name: "claude", Key: "token", MountPath: "/run/roomcity/secrets/claude/token", Env: "CLAUDE_CODE_OAUTH_TOKEN"}},
	})
	pod := podOf(t, jobs, c, operate.JobSpec{Session: "w-1", Agent: "worker", WorkDir: "/w"})

	require.Contains(t, pod.Containers[0].Env, corev1.EnvVar{Name: "ROOMCITY_SECRET_CLAUDE_CODE_OAUTH_TOKEN", Value: "/run/roomcity/secrets/claude/token"})
	for _, e := range pod.Containers[0].Env {
		require.NotEqual(t, "CLAUDE_CODE_OAUTH_TOKEN", e.Name, "the value is never in the spec")
	}
}

func TestThePodGoesToTheSessionsNodes(t *testing.T) {
	jobs, c := newJobs()
	pod := podOf(t, jobs, c, operate.JobSpec{Session: "w-1", Agent: "worker", WorkDir: "/w"})
	require.Equal(t, map[string]string{"kubernetes.io/hostname": "bulgakov"}, pod.NodeSelector)
	require.Equal(t, []corev1.Toleration{{Key: "sarak.sh/workstation", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}, pod.Tolerations)
	req := pod.Containers[0].Resources.Requests
	require.Equal(t, "250m", req.Cpu().String())
	require.Equal(t, "512Mi", req.Memory().String())
}

func TestTheStandInGcComesFirstOnThePathWhateverTheStartConfigSays(t *testing.T) {
	jobs, c := newJobs()
	pod := podOf(t, jobs, c, operate.JobSpec{Session: "w-1", Agent: "worker", WorkDir: "/w", Env: map[string]string{"PATH": "/city/bin"}})
	var paths []string
	for _, e := range pod.Containers[0].Env {
		if e.Name == "PATH" {
			paths = append(paths, e.Value)
		}
	}
	require.Len(t, paths, 1)
	require.True(t, strings.HasPrefix(paths[0], "/usr/local/lib/roomcity/bin:"), paths[0])
	require.NotContains(t, paths[0], "/city/bin")
}

func TestThePodRunsAsTheCityDoes(t *testing.T) {
	jobs, c := newJobs()
	pod := podOf(t, jobs, c, operate.JobSpec{Session: "w-1", Agent: "worker", WorkDir: "/w"})
	require.Equal(t, int64(1000), *pod.SecurityContext.RunAsUser)
	require.True(t, *pod.SecurityContext.RunAsNonRoot)
	require.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, pod.SecurityContext.SeccompProfile.Type)
	sc := pod.Containers[0].SecurityContext
	require.False(t, *sc.AllowPrivilegeEscalation)
	require.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop)

	// A configuration that names no user gets the image's.
	c2 := fake.NewSimpleClientset()
	j := kube.New(c2, kube.Config{Namespace: "sandbox", Image: "i"}, nil)
	require.NoError(t, j.Create(context.Background(), operate.JobSpec{Session: "w-1", WorkDir: "/w"}))
	list, err := c2.BatchV1().Jobs("sandbox").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(1000), *list.Items[0].Spec.Template.Spec.SecurityContext.RunAsUser)
}

func TestAliveAndRunningFollowTheJobsConditions(t *testing.T) {
	ctx := context.Background()
	jobs, c := newJobs()
	for _, s := range []string{"a-1", "b-1", "c-1"} {
		require.NoError(t, jobs.Create(ctx, operate.JobSpec{Session: s, WorkDir: "/w"}))
	}
	alive, err := jobs.Alive(ctx, "nope")
	require.NoError(t, err)
	require.False(t, alive)

	list, err := c.BatchV1().Jobs("sandbox").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	for _, j := range list.Items {
		if j.Annotations["roomcity.sarak.sh/session"] == "b-1" {
			j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
			_, err := c.BatchV1().Jobs("sandbox").UpdateStatus(ctx, &j, metav1.UpdateOptions{})
			require.NoError(t, err)
		}
	}

	alive, err = jobs.Alive(ctx, "b-1")
	require.NoError(t, err)
	require.False(t, alive, "a failed Job")
	alive, err = jobs.Alive(ctx, "a-1")
	require.NoError(t, err)
	require.True(t, alive)

	running, err := jobs.Running(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"a-1", "c-1"}, running)
}

func TestDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	jobs, _ := newJobs()
	require.NoError(t, jobs.Create(ctx, operate.JobSpec{Session: "a-1", WorkDir: "/w"}))
	require.NoError(t, jobs.Delete(ctx, "a-1"))
	require.NoError(t, jobs.Delete(ctx, "a-1"))
	alive, err := jobs.Alive(ctx, "a-1")
	require.NoError(t, err)
	require.False(t, alive)
}

func TestNamesThatCollapseToTheSameStemStayApart(t *testing.T) {
	ctx := context.Background()
	jobs, c := newJobs()
	require.NoError(t, jobs.Create(ctx, operate.JobSpec{Session: "a_b", WorkDir: "/w"}))
	require.NoError(t, jobs.Create(ctx, operate.JobSpec{Session: "a-b", WorkDir: "/w"}))
	list, err := c.BatchV1().Jobs("sandbox").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 2)
}

func TestInfoListAndDeleteObjectDescribeAndDeleteTheExactJob(t *testing.T) {
	ctx := context.Background()
	jobs, c := newJobs()
	for _, s := range []string{"a-1", "b-1"} {
		require.NoError(t, jobs.Create(ctx, operate.JobSpec{Session: s, WorkDir: "/w"}))
	}
	// The fake cluster makes no UIDs; the real one does, and a Job that
	// ends has a condition.
	list, err := c.BatchV1().Jobs("sandbox").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	for _, j := range list.Items {
		j.UID = types.UID("uid-" + j.Annotations["roomcity.sarak.sh/session"])
		if j.Annotations["roomcity.sarak.sh/session"] == "b-1" {
			j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
		}
		_, err := c.BatchV1().Jobs("sandbox").Update(ctx, &j, metav1.UpdateOptions{})
		require.NoError(t, err)
	}

	in, held, err := jobs.Info(ctx, "a-1")
	require.NoError(t, err)
	require.True(t, held)
	require.Equal(t, "uid-a-1", in.ID)
	require.True(t, in.Alive)
	in, held, err = jobs.Info(ctx, "b-1")
	require.NoError(t, err)
	require.True(t, held)
	require.False(t, in.Alive, "a failed Job is held and not alive")
	_, held, err = jobs.Info(ctx, "nope")
	require.NoError(t, err)
	require.False(t, held)

	all, err := jobs.List(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)

	deleted, err := jobs.DeleteObject(ctx, "a-1", "uid-other")
	require.NoError(t, err)
	require.False(t, deleted, "another object's id")
	_, held, _ = jobs.Info(ctx, "a-1")
	require.True(t, held)
	deleted, err = jobs.DeleteObject(ctx, "a-1", "uid-a-1")
	require.NoError(t, err)
	require.True(t, deleted)
	deleted, err = jobs.DeleteObject(ctx, "a-1", "uid-a-1")
	require.NoError(t, err)
	require.False(t, deleted, "gone")
}

func TestNoMountMakesAParentOfTheDirectoriesTheAgentWritesInTheHome(t *testing.T) {
	jobs, c := newJobs()
	pod := podOf(t, jobs, c, operate.JobSpec{Session: "w-1", Agent: "worker", WorkDir: "/home/city/city/.gc/agents/worker"})
	home := "/home/city"
	for _, dir := range []string{".config", ".claude", ".local", ".cache", ".config/goose", ".config/beads"} {
		for p := range mountPaths(pod) {
			require.False(t, p == path.Join(home, dir) || strings.HasPrefix(p, path.Join(home, dir)+"/"),
				"%s is mounted inside ~/%s: the kubelet would make its parents root's", p, dir)
		}
	}

	// bd's credentials are a file outside the home, and bd is told where.
	require.Contains(t, pod.Containers[0].Env, corev1.EnvVar{Name: "BEADS_CREDENTIALS_FILE", Value: "/run/roomcity/secrets/dolt/credentials"})
	require.Contains(t, mountPaths(pod), "/run/roomcity/secrets/dolt/credentials")
}

func TestASecretInsideTheHomeIsRefused(t *testing.T) {
	cfg := kube.Config{}
	require.ErrorContains(t, cfg.CheckMount(kube.Secret{Name: "d", Key: "k", MountPath: "/home/city/.config/beads/credentials"}), "inside the home")
	require.ErrorContains(t, cfg.CheckMount(kube.Secret{Name: "d", Key: "k", MountPath: "/home/city"}), "inside the home")
	require.NoError(t, cfg.CheckMount(kube.Secret{Name: "d", Key: "k", MountPath: "/home/citybank/x"}))
	require.NoError(t, cfg.CheckMount(kube.Secret{Name: "d", Key: "k", MountPath: "/run/roomcity/secrets/d/k"}))
	require.ErrorContains(t, kube.Config{Home: "/root"}.CheckMount(kube.Secret{Name: "d", Key: "k", MountPath: "/root/.ssh/id"}), "inside the home")
	require.ErrorContains(t, kube.Secret{Name: "d", MountPath: "/x", PathEnv: "BEADS_CREDENTIALS_FILE"}.Validate(), "needs a key")
	require.ErrorContains(t, kube.Secret{Name: "d", Key: "k", MountPath: "/x", PathEnv: "beads"}.Validate(), "variable name")
}

func TestAFailedPodIsDescribedByItsStateAndTheEndOfItsLog(t *testing.T) {
	ctx := context.Background()
	jobs, c := newJobs()
	require.NoError(t, jobs.Create(ctx, operate.JobSpec{Session: "w-1", Agent: "worker", WorkDir: "/w"}))

	why, err := jobs.Failure(ctx, "w-1")
	require.NoError(t, err)
	require.Equal(t, "no pod was made for it", why)

	_, err = c.CoreV1().Pods("sandbox").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "roomcity-w-1-abc", Namespace: "sandbox", Labels: map[string]string{"roomcity.sarak.sh/session-id": jobID(t, c)}},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "host",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
			}},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	why, err = jobs.Failure(ctx, "w-1")
	require.NoError(t, err)
	require.Contains(t, why, "host exited 1 Error")
	require.Contains(t, why, "its log ends:", "the fake cluster's log is 'fake logs'")
}

func jobID(t *testing.T, c *fake.Clientset) string {
	t.Helper()
	list, err := c.BatchV1().Jobs("sandbox").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	return list.Items[0].Labels["roomcity.sarak.sh/session-id"]
}

func TestTheJobNeverMakesASecondPodForAFailedStart(t *testing.T) {
	ctx := context.Background()
	jobs, c := newJobs()
	require.NoError(t, jobs.Create(ctx, operate.JobSpec{Session: "guest-1", Agent: "guest", WorkDir: "/w"}))
	list, err := c.BatchV1().Jobs("sandbox").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	job := list.Items[0]

	// A session that ended is ended: whether to start it again is gc's
	// decision, never the Job controller's, and a pod that replaced the
	// failed one would attach as the same session, with a fresh agent
	// behind gc's back.
	require.NotNil(t, job.Spec.BackoffLimit)
	require.Zero(t, *job.Spec.BackoffLimit, "no retry of a failed pod")
	require.Equal(t, corev1.RestartPolicyNever, job.Spec.Template.Spec.RestartPolicy, "no restart of a failed container")
	require.Nil(t, job.Spec.PodFailurePolicy, "no rule that ignores a failure and makes another pod")
	require.Nil(t, job.Spec.Parallelism, "one pod at a time")
	require.Nil(t, job.Spec.Completions)
	require.Nil(t, job.Spec.ActiveDeadlineSeconds)
}
