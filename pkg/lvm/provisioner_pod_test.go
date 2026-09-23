package lvm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

type fakePods struct {
	corev1.PodInterface
	createErr        error
	created          bool
	getErr           error
	existingPod      *v1.Pod
	listItems        []v1.Pod
	listErr          error
	listCalled       bool
	phase            v1.PodPhase
	phases           []v1.PodPhase
	podStatus        v1.PodStatus
	deleteErr        error
	deleted          bool
	deleteOptions    metav1.DeleteOptions
	deleteContextErr error
	onGet            func()
}

func (f *fakePods) Create(
	_ context.Context,
	pod *v1.Pod,
	_ metav1.CreateOptions,
) (*v1.Pod, error) {
	if f.createErr == nil {
		f.created = true
		f.existingPod = pod.DeepCopy()
	}
	return pod, f.createErr
}

func (f *fakePods) Get(
	_ context.Context,
	name string,
	_ metav1.GetOptions,
) (*v1.Pod, error) {
	if f.onGet != nil {
		f.onGet()
		f.onGet = nil
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.existingPod == nil {
		return nil, k8serror.NewNotFound(
			schema.GroupResource{Resource: "pods"},
			name,
		)
	}
	status := f.podStatus
	if len(f.phases) > 0 {
		status.Phase = f.phases[0]
		f.phases = f.phases[1:]
	}
	if status.Phase == "" {
		status.Phase = f.phase
	}
	pod := f.existingPod.DeepCopy()
	pod.Status = status
	return pod, nil
}

func (f *fakePods) List(
	_ context.Context,
	_ metav1.ListOptions,
) (*v1.PodList, error) {
	f.listCalled = true
	return &v1.PodList{Items: f.listItems}, f.listErr
}

func (f *fakePods) Delete(
	ctx context.Context,
	_ string,
	options metav1.DeleteOptions,
) error {
	f.deleted = true
	f.deleteOptions = options
	f.deleteContextErr = ctx.Err()
	if f.deleteErr == nil {
		f.existingPod = nil
	}
	return f.deleteErr
}

func TestVolumeHelperArgs(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		args, err := volumeHelperArgs(volumeAction{
			action:   actionTypeCreate,
			name:     "volume",
			nodeName: "node-a",
			size:     1048576,
			lvmType:  DmThinType,
			vgName:   "vg",
		})
		if err != nil {
			t.Fatalf("volumeHelperArgs failed: %v", err)
		}
		want := []string{
			"createlv", "--lvsize", "1048576", "--lvmtype", DmThinType,
			"--vgname", "vg", "--lvname", "volume",
		}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("unexpected arguments: want %#v, got %#v", want, args)
		}
	})

	t.Run("delete requires source metadata", func(t *testing.T) {
		_, err := volumeHelperArgs(volumeAction{
			action:   actionTypeDelete,
			name:     "volume",
			nodeName: "node-a",
		})
		if err == nil {
			t.Fatal("expected missing source metadata to fail")
		}
	})
}

func TestSnapshotHelperArgs(t *testing.T) {
	args, err := snapshotHelperArgs(snapshotAction{
		action:       actionTypeDelete,
		snapshotName: "snapshot",
		nodeName:     "node-a",
		vgName:       "vg",
	})
	if err != nil {
		t.Fatalf("snapshotHelperArgs failed: %v", err)
	}
	want := []string{"deletesnap", "--snapname", "snapshot", "--vgname", "vg"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("unexpected arguments: want %#v, got %#v", want, args)
	}
}

func TestHelperPodTerminationFallback(t *testing.T) {
	pod := newHelperPod(
		helperParams{
			name:             "volume",
			resource:         "volume",
			action:           actionTypeCreate,
			nodeName:         "node-a",
			vgName:           "vg-a",
			hostWritePath:    "/var/lib/lvm",
			provisionerImage: "provisioner:latest",
			pullPolicy:       v1.PullIfNotPresent,
			config:           DefaultHelperConfig(),
		},
		[]string{"createlv"},
	)
	if pod.Name != "lvm-create-volume" {
		t.Fatalf("unexpected helper pod name %q", pod.Name)
	}
	if pod.Labels[helperOperationLabel] != string(actionTypeCreate) {
		t.Fatalf("missing operation label: %#v", pod.Labels)
	}
	if pod.Labels[helperResourceLabel] != "volume" {
		t.Fatalf("missing resource label: %#v", pod.Labels)
	}
	if pod.Annotations[helperVGAnnotation] != "vg-a" {
		t.Fatalf("missing volume group annotation: %#v", pod.Annotations)
	}
	deadline := pod.Spec.ActiveDeadlineSeconds
	if deadline == nil || *deadline != 210 {
		t.Fatalf("unexpected active deadline: %v", deadline)
	}
	grace := pod.Spec.TerminationGracePeriodSeconds
	if grace == nil || *grace != 10 {
		t.Fatalf("unexpected termination grace period: %v", grace)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected one provisioner container, got %d", len(pod.Spec.Containers))
	}
	container := pod.Spec.Containers[0]
	wantArgs := []string{
		"--command-timeout=3m0s",
		"--thin-pool-create-timeout=15m0s",
		"createlv",
	}
	if !reflect.DeepEqual(container.Args, wantArgs) {
		t.Fatalf("unexpected helper arguments: want %#v, got %#v", wantArgs, container.Args)
	}
	if container.TerminationMessagePath != "/termination.log" {
		t.Fatalf("unexpected termination message path %q", container.TerminationMessagePath)
	}
	if container.TerminationMessagePolicy != v1.TerminationMessageFallbackToLogsOnError {
		t.Fatalf("unexpected termination message policy %q", container.TerminationMessagePolicy)
	}
}

func TestThinPoolHelperDeadline(t *testing.T) {
	pod := newHelperPod(
		helperParams{
			name:             "volume",
			resource:         "volume",
			action:           actionTypeCreate,
			nodeName:         "node-a",
			vgName:           "vg-a",
			lvmType:          DmThinType,
			hostWritePath:    "/var/lib/lvm",
			provisionerImage: "provisioner:latest",
			pullPolicy:       v1.PullIfNotPresent,
			config:           DefaultHelperConfig(),
		},
		[]string{"createlv"},
	)

	deadline := pod.Spec.ActiveDeadlineSeconds
	if deadline == nil || *deadline != 18*60 {
		t.Fatalf("unexpected thin-pool helper deadline: %v", deadline)
	}
}

func TestRunHelper(t *testing.T) {
	t.Run("success uses independent cleanup context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		pods := &fakePods{
			phase: v1.PodSucceeded,
			onGet: cancel,
		}

		err := runHelper(
			ctx,
			pods,
			testHelper("helper", "node-a", "vg-a", ""),
			testRunParams("volume", actionTypeCreate),
		)
		if err != nil {
			t.Fatalf("runHelper failed: %v", err)
		}
		if !pods.deleted {
			t.Fatal("expected helper pod cleanup")
		}
		if pods.deleteContextErr != nil {
			t.Fatalf("cleanup inherited canceled request context: %v", pods.deleteContextErr)
		}
	})

	t.Run("canceled request retains pending pod for retry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		pods := &fakePods{
			phase: v1.PodPending,
			onGet: cancel,
		}

		err := runHelper(
			ctx,
			pods,
			testHelper("helper", "node-a", "vg-a", ""),
			testRunParams("volume", actionTypeDelete),
		)
		if status.Code(err) != codes.Canceled {
			t.Fatalf("expected canceled request, got %v", err)
		}
		if pods.deleted {
			t.Fatal("pending helper pod should be retained")
		}

		pods.createErr = k8serror.NewAlreadyExists(
			schema.GroupResource{Resource: "pods"},
			"helper",
		)
		pods.phase = v1.PodSucceeded
		if err := runHelper(
			context.Background(),
			pods,
			testHelper("helper", "node-a", "vg-a", ""),
			testRunParams("volume", actionTypeDelete),
		); err != nil {
			t.Fatalf("retry did not reuse retained helper pod: %v", err)
		}
		if !pods.deleted {
			t.Fatal("terminal helper pod should be cleaned up")
		}
	})

	t.Run("failed pod maps to internal and is cleaned up", func(t *testing.T) {
		pods := &fakePods{podStatus: v1.PodStatus{
			Phase:   v1.PodFailed,
			Reason:  "ContainerFailure",
			Message: "helper container failed",
			ContainerStatuses: []v1.ContainerStatus{{
				Name: "provisioner",
				State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{
					ExitCode: 1,
					Reason:   "Error",
					Message:  "lvcreate failed: insufficient free space",
				}},
			}},
		}}
		err := runHelper(
			context.Background(),
			pods,
			testHelper("helper", "node-a", "vg-a", ""),
			testRunParams("snapshot", actionTypeDelete),
		)
		if status.Code(err) != codes.Internal {
			t.Fatalf("expected Internal, got %v", err)
		}
		for _, detail := range []string{
			"ContainerFailure",
			"container provisioner exited with code 1",
			"lvcreate failed: insufficient free space",
		} {
			if !strings.Contains(err.Error(), detail) {
				t.Fatalf("expected failure to contain %q, got %v", detail, err)
			}
		}
		if !pods.deleted {
			t.Fatal("expected failed helper pod cleanup")
		}
	})

	t.Run("existing helper pod is reused", func(t *testing.T) {
		desired := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "helper"},
			Spec: v1.PodSpec{Containers: []v1.Container{{
				Name: "provisioner",
				Args: []string{"createlv", "--lvsize", "20Gi"},
			}}},
		}
		existing := desired.DeepCopy()
		existing.Spec.DNSPolicy = v1.DNSClusterFirst
		pods := &fakePods{
			createErr: k8serror.NewAlreadyExists(
				schema.GroupResource{Resource: "pods"},
				"helper",
			),
			existingPod: existing,
			phase:       v1.PodSucceeded,
		}
		if err := runHelper(
			context.Background(),
			pods,
			desired,
			testRunParams("volume", actionTypeCreate),
		); err != nil {
			t.Fatalf("existing helper pod was not reused: %v", err)
		}
	})

	t.Run("terminating helper pod is force deleted", func(t *testing.T) {
		desired := reusableHelper("20Gi")
		existing := desired.DeepCopy()
		now := metav1.Now()
		existing.DeletionTimestamp = &now
		pods := podsWithHelper(existing)

		err := ensureHelper(context.Background(), pods, desired, DefaultMaxActiveHelpers)
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("expected Unavailable, got %v", err)
		}
		if !pods.deleted {
			t.Fatal("terminating helper pod should be force deleted")
		}
		gracePeriod := pods.deleteOptions.GracePeriodSeconds
		if gracePeriod == nil || *gracePeriod != 0 {
			t.Fatalf("expected zero deletion grace period, got %v", gracePeriod)
		}
		pods.createErr = nil
		if err := ensureHelper(
			context.Background(), pods, desired, DefaultMaxActiveHelpers,
		); err != nil {
			t.Fatalf("retry did not create the desired helper pod: %v", err)
		}
	})

	t.Run("different running helper pod is retained", func(t *testing.T) {
		desired := reusableHelper("20Gi")
		pods := podsWithHelper(reusableHelper("10Gi"))
		pods.phase = v1.PodRunning

		err := ensureHelper(context.Background(), pods, desired, DefaultMaxActiveHelpers)
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("expected Unavailable, got %v", err)
		}
		if pods.deleted {
			t.Fatal("running helper pod should not be deleted")
		}
	})

	t.Run("different terminal helper pod is removed", func(t *testing.T) {
		desired := reusableHelper("20Gi")
		pods := podsWithHelper(reusableHelper("10Gi"))
		pods.phase = v1.PodSucceeded

		err := ensureHelper(context.Background(), pods, desired, DefaultMaxActiveHelpers)
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("expected Unavailable, got %v", err)
		}
		if !pods.deleted {
			t.Fatal("terminal helper pod should be deleted before retry")
		}
		pods.createErr = nil
		if err := ensureHelper(
			context.Background(), pods, desired, DefaultMaxActiveHelpers,
		); err != nil {
			t.Fatalf("retry did not create the desired helper pod: %v", err)
		}
	})

	t.Run("existing helper pod lookup failure is retryable", func(t *testing.T) {
		desired := reusableHelper("20Gi")
		pods := podsWithHelper(desired.DeepCopy())
		pods.getErr = errors.New("api unavailable")

		err := ensureHelper(context.Background(), pods, desired, DefaultMaxActiveHelpers)
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("expected Unavailable, got %v", err)
		}
	})

	t.Run("create failure does not attempt cleanup", func(t *testing.T) {
		pods := &fakePods{createErr: errors.New("api unavailable")}
		if err := runHelper(
			context.Background(),
			pods,
			testHelper("helper", "node-a", "vg-a", ""),
			testRunParams("volume", actionTypeCreate),
		); err == nil {
			t.Fatal("expected create error")
		}
		if pods.deleted {
			t.Fatal("unexpected cleanup for a pod that was not created")
		}
	})
}

func TestWaitForHelperPastOldLimit(t *testing.T) {
	originalInterval := podPollInterval
	podPollInterval = time.Millisecond
	t.Cleanup(func() {
		podPollInterval = originalInterval
	})

	phases := make([]v1.PodPhase, 61, 62)
	for index := range phases {
		phases[index] = v1.PodRunning
	}
	phases = append(phases, v1.PodSucceeded)
	pods := &fakePods{
		existingPod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "helper"}},
		phases:      phases,
	}

	terminal, err := waitForHelper(
		context.Background(),
		pods,
		"helper",
		"volume",
		actionTypeCreate,
	)
	if err != nil || !terminal {
		t.Fatalf("expected helper to succeed after 61 polls, terminal=%v err=%v", terminal, err)
	}
}

func TestWaitForMissingHelperFailsFast(t *testing.T) {
	terminal, err := waitForHelper(
		context.Background(),
		&fakePods{},
		"missing-helper",
		"volume",
		actionTypeCreate,
	)
	if terminal {
		t.Fatal("missing helper should not be terminal")
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "missing-helper") {
		t.Fatalf("expected helper name in error, got %v", err)
	}
}

func TestHelperAdmission(t *testing.T) {
	desired := testHelper("desired", "node-a", "vg-a", v1.PodPending)

	t.Run("counts retained volume and snapshot helpers", func(t *testing.T) {
		legacy := reusableHelper("10Gi")
		legacy.Name = "legacy"
		legacy.Spec.NodeName = "node-a"
		legacy.Spec.Containers[0].Command = []string{"csi-lvmplugin-provisioner"}
		legacy.Spec.Containers[0].Args = []string{"deletelv", "--srcvgname", "vg-a"}
		legacy.Status.Phase = v1.PodRunning

		pods := &fakePods{listItems: []v1.Pod{
			*testHelper("volume", "node-a", "vg-a", v1.PodPending),
			*testHelper("snapshot", "node-a", "vg-a", v1.PodRunning),
			*legacy,
		}}
		err := ensureHelper(context.Background(), pods, desired, 3)
		if status.Code(err) != codes.Aborted {
			t.Fatalf("expected active helper limit to abort, got %v", err)
		}
		if pods.created {
			t.Fatal("helper pod was created after reaching the active limit")
		}
	})

	t.Run("scopes the limit by node and volume group", func(t *testing.T) {
		pods := &fakePods{listItems: []v1.Pod{
			*testHelper("other-node", "node-b", "vg-a", v1.PodRunning),
			*testHelper("other-vg", "node-a", "vg-b", v1.PodRunning),
			*testHelper("terminal", "node-a", "vg-a", v1.PodSucceeded),
		}}
		if err := ensureHelper(context.Background(), pods, desired, 1); err != nil {
			t.Fatalf("unrelated helpers blocked admission: %v", err)
		}
		if !pods.created {
			t.Fatal("expected desired helper pod to be created")
		}
	})

	t.Run("same operation rejoins without consuming a new slot", func(t *testing.T) {
		existing := desired.DeepCopy()
		existing.Status.Phase = v1.PodRunning
		pods := &fakePods{
			existingPod: existing,
			listItems: []v1.Pod{
				*testHelper("other", "node-a", "vg-a", v1.PodRunning),
			},
		}
		if err := ensureHelper(context.Background(), pods, desired, 1); err != nil {
			t.Fatalf("retry did not rejoin its existing helper: %v", err)
		}
		if pods.listCalled {
			t.Fatal("retry should reuse its named helper before checking admission")
		}
	})

	t.Run("clone is limited by its source volume group", func(t *testing.T) {
		clone := testHelper("clone", "node-a", "vg-b", v1.PodPending)
		clone.Spec.Containers[0].Args = []string{
			"clonelv",
			"--srcvgname", "vg-a",
			"--vgname", "vg-b",
		}
		pods := &fakePods{listItems: []v1.Pod{
			*testHelper("source-op", "node-a", "vg-a", v1.PodRunning),
		}}
		err := ensureHelper(context.Background(), pods, clone, 1)
		if status.Code(err) != codes.Aborted {
			t.Fatalf("expected source VG limit to abort clone, got %v", err)
		}
	})
}

func testRunParams(resource string, action actionType) helperParams {
	return helperParams{
		resource: resource,
		action:   action,
		config:   DefaultHelperConfig(),
	}
}

func testHelper(name, node, vg string, phase v1.PodPhase) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{helperPodLabel: "true"},
			Annotations: map[string]string{helperVGAnnotation: vg},
		},
		Spec: v1.PodSpec{
			NodeName: node,
			Containers: []v1.Container{{
				Name:    "provisioner",
				Command: []string{"csi-lvmplugin-provisioner"},
				Args:    []string{"createlv", "--vgname", vg},
			}},
		},
		Status: v1.PodStatus{Phase: phase},
	}
}

func reusableHelper(size string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "helper",
			Labels:      map[string]string{helperPodLabel: "true"},
			Annotations: map[string]string{helperVGAnnotation: "vg-a"},
		},
		Spec: v1.PodSpec{Containers: []v1.Container{{
			Name:    "provisioner",
			Command: []string{"csi-lvmplugin-provisioner"},
			Args:    []string{"createlv", "--lvsize", size, "--vgname", "vg-a"},
		}}},
	}
}

func podsWithHelper(existing *v1.Pod) *fakePods {
	return &fakePods{
		createErr: k8serror.NewAlreadyExists(
			schema.GroupResource{Resource: "pods"},
			"helper",
		),
		existingPod: existing,
	}
}
