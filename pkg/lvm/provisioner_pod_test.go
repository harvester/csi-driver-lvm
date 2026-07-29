package lvm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

type fakeProvisionerPods struct {
	corev1.PodInterface
	createErr        error
	getErr           error
	phase            v1.PodPhase
	podStatus        v1.PodStatus
	deleteErr        error
	deleted          bool
	deleteContextErr error
	onGet            func()
}

func (f *fakeProvisionerPods) Create(
	_ context.Context,
	pod *v1.Pod,
	_ metav1.CreateOptions,
) (*v1.Pod, error) {
	return pod, f.createErr
}

func (f *fakeProvisionerPods) Get(
	_ context.Context,
	name string,
	_ metav1.GetOptions,
) (*v1.Pod, error) {
	if f.onGet != nil {
		f.onGet()
		f.onGet = nil
	}
	status := f.podStatus
	if status.Phase == "" {
		status.Phase = f.phase
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     status,
	}, f.getErr
}

func (f *fakeProvisionerPods) Delete(
	ctx context.Context,
	_ string,
	_ metav1.DeleteOptions,
) error {
	f.deleted = true
	f.deleteContextErr = ctx.Err()
	return f.deleteErr
}

func TestVolumeProvisionerArgs(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		args, err := volumeProvisionerArgs(volumeAction{
			action:   actionTypeCreate,
			name:     "volume",
			nodeName: "node-a",
			size:     1048576,
			lvmType:  DmThinType,
			vgName:   "vg",
		})
		if err != nil {
			t.Fatalf("volumeProvisionerArgs failed: %v", err)
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
		_, err := volumeProvisionerArgs(volumeAction{
			action:   actionTypeDelete,
			name:     "volume",
			nodeName: "node-a",
		})
		if err == nil {
			t.Fatal("expected missing source metadata to fail")
		}
	})
}

func TestSnapshotProvisionerArgs(t *testing.T) {
	args, err := snapshotProvisionerArgs(snapshotAction{
		action:       actionTypeDelete,
		snapshotName: "snapshot",
		nodeName:     "node-a",
		vgName:       "vg",
	})
	if err != nil {
		t.Fatalf("snapshotProvisionerArgs failed: %v", err)
	}
	want := []string{"deletesnap", "--snapname", "snapshot", "--vgname", "vg"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("unexpected arguments: want %#v, got %#v", want, args)
	}
}

func TestProvisionerPodFallsBackToLogsForTerminationMessage(t *testing.T) {
	pod := genProvisionerPodContent(
		"lvm-create",
		"volume",
		"node-a",
		"/var/lib/lvm",
		"provisioner:latest",
		v1.PullIfNotPresent,
		[]string{"createlv"},
	)
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected one provisioner container, got %d", len(pod.Spec.Containers))
	}
	container := pod.Spec.Containers[0]
	if container.TerminationMessagePath != "/termination.log" {
		t.Fatalf("unexpected termination message path %q", container.TerminationMessagePath)
	}
	if container.TerminationMessagePolicy != v1.TerminationMessageFallbackToLogsOnError {
		t.Fatalf("unexpected termination message policy %q", container.TerminationMessagePolicy)
	}
}

func TestRunProvisionerPod(t *testing.T) {
	t.Run("success uses independent cleanup context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		pods := &fakeProvisionerPods{
			phase: v1.PodSucceeded,
			onGet: cancel,
		}

		err := runProvisionerPod(
			ctx,
			pods,
			&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "helper"}},
			"volume",
			actionTypeCreate,
		)
		if err != nil {
			t.Fatalf("runProvisionerPod failed: %v", err)
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
		pods := &fakeProvisionerPods{
			phase: v1.PodPending,
			onGet: cancel,
		}

		err := runProvisionerPod(
			ctx,
			pods,
			&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "helper"}},
			"volume",
			actionTypeDelete,
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
		if err := runProvisionerPod(
			context.Background(),
			pods,
			&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "helper"}},
			"volume",
			actionTypeDelete,
		); err != nil {
			t.Fatalf("retry did not reuse retained helper pod: %v", err)
		}
		if !pods.deleted {
			t.Fatal("terminal helper pod should be cleaned up")
		}
	})

	t.Run("failed pod maps to internal and is cleaned up", func(t *testing.T) {
		pods := &fakeProvisionerPods{podStatus: v1.PodStatus{
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
		err := runProvisionerPod(
			context.Background(),
			pods,
			&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "helper"}},
			"snapshot",
			actionTypeDelete,
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
		pods := &fakeProvisionerPods{
			createErr: k8serror.NewAlreadyExists(
				schema.GroupResource{Resource: "pods"},
				"helper",
			),
			phase: v1.PodSucceeded,
		}
		if err := runProvisionerPod(
			context.Background(),
			pods,
			&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "helper"}},
			"volume",
			actionTypeCreate,
		); err != nil {
			t.Fatalf("existing helper pod was not reused: %v", err)
		}
	})

	t.Run("create failure does not attempt cleanup", func(t *testing.T) {
		pods := &fakeProvisionerPods{createErr: errors.New("api unavailable")}
		if err := runProvisionerPod(
			context.Background(),
			pods,
			&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "helper"}},
			"volume",
			actionTypeCreate,
		); err == nil {
			t.Fatal("expected create error")
		}
		if pods.deleted {
			t.Fatal("unexpected cleanup for a pod that was not created")
		}
	})
}
