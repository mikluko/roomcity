package kube_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			{Name: "dolt", Key: "credentials", MountPath: "/home/city/.config/beads/credentials"},
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
		creds := mounts["/home/city/.config/beads/credentials"]
		require.Equal(t, "credentials", creds.SubPath)
		require.True(t, creds.ReadOnly)
	}

	jobs, c := newJobs()
	mounts := mountPaths(podOf(t, jobs, c, operate.JobSpec{Session: "other-1", Agent: "other", WorkDir: "/w"}))
	require.Len(t, mounts, 2, "an agent that names none gets bd's credentials alone")
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
