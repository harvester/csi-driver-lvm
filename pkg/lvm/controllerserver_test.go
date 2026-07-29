package lvm

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	snapclient "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned"
	snaptypedv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned/typed/volumesnapshot/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

func strPointer(value string) *string {
	return &value
}

type fakeKubeClient struct {
	kubernetes.Interface
	volumes map[string]*v1.PersistentVolume
	nodes   map[string]*v1.Node
}

func (f *fakeKubeClient) CoreV1() corev1.CoreV1Interface {
	return &fakeCoreV1{volumes: f.volumes, nodes: f.nodes}
}

type fakeCoreV1 struct {
	corev1.CoreV1Interface
	volumes map[string]*v1.PersistentVolume
	nodes   map[string]*v1.Node
}

func (f *fakeCoreV1) PersistentVolumes() corev1.PersistentVolumeInterface {
	return &fakePersistentVolumes{volumes: f.volumes}
}

func (f *fakeCoreV1) Nodes() corev1.NodeInterface {
	return &fakeNodes{nodes: f.nodes}
}

type fakePersistentVolumes struct {
	corev1.PersistentVolumeInterface
	volumes map[string]*v1.PersistentVolume
}

type fakeNodes struct {
	corev1.NodeInterface
	nodes map[string]*v1.Node
}

func (f *fakeNodes) Get(
	_ context.Context,
	name string,
	_ metav1.GetOptions,
) (*v1.Node, error) {
	if node := f.nodes[name]; node != nil {
		return node.DeepCopy(), nil
	}
	return nil, k8serror.NewNotFound(schema.GroupResource{Resource: "nodes"}, name)
}

func (f *fakePersistentVolumes) Get(
	_ context.Context,
	name string,
	_ metav1.GetOptions,
) (*v1.PersistentVolume, error) {
	if volume := f.volumes[name]; volume != nil {
		return volume.DeepCopy(), nil
	}
	return nil, k8serror.NewNotFound(schema.GroupResource{Resource: "persistentvolumes"}, name)
}

type fakeSnapshotClient struct {
	snapclient.Interface
	contents []snapv1.VolumeSnapshotContent
}

func (f *fakeSnapshotClient) SnapshotV1() snaptypedv1.SnapshotV1Interface {
	return &fakeSnapshotV1{contents: f.contents}
}

type fakeSnapshotV1 struct {
	snaptypedv1.SnapshotV1Interface
	contents []snapv1.VolumeSnapshotContent
}

func (f *fakeSnapshotV1) VolumeSnapshotContents() snaptypedv1.VolumeSnapshotContentInterface {
	return &fakeSnapshotContents{contents: f.contents}
}

type fakeSnapshotContents struct {
	snaptypedv1.VolumeSnapshotContentInterface
	contents []snapv1.VolumeSnapshotContent
}

func (f *fakeSnapshotContents) List(
	_ context.Context,
	_ metav1.ListOptions,
) (*snapv1.VolumeSnapshotContentList, error) {
	return &snapv1.VolumeSnapshotContentList{Items: append([]snapv1.VolumeSnapshotContent(nil), f.contents...)}, nil
}

func (f *fakeSnapshotContents) Watch(
	_ context.Context,
	_ metav1.ListOptions,
) (watch.Interface, error) {
	return watch.NewEmptyWatch(), nil
}

func controllerWithFakeClients() *controllerServer {
	return &controllerServer{
		caps: getControllerServiceCapabilities([]csi.ControllerServiceCapability_RPC_Type{
			csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
			csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		}),
		kubeClient: &fakeKubeClient{
			volumes: map[string]*v1.PersistentVolume{},
			nodes:   map[string]*v1.Node{},
		},
		snapClient: &fakeSnapshotClient{},
	}
}

func TestCreateVolumeRejectsMissingTopology(t *testing.T) {
	cs := controllerWithFakeClients()
	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "volume",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1048576},
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
		Parameters: map[string]string{
			"type":   DmThinType,
			"vgName": "vg",
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestDeleteVolumeIsIdempotentWhenPVIsAbsent(t *testing.T) {
	cs := controllerWithFakeClients()
	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "missing"}); err != nil {
		t.Fatalf("idempotent delete failed: %v", err)
	}
}

func TestNodeAvailableForDeletion(t *testing.T) {
	cs := controllerWithFakeClients()

	available, err := cs.nodeAvailableForDeletion(context.Background(), "missing", "volume")
	if err != nil || available {
		t.Fatalf("expected missing node to be unavailable without error, got available=%t err=%v", available, err)
	}

	cs.kubeClient.(*fakeKubeClient).nodes["node-a"] = &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
	available, err = cs.nodeAvailableForDeletion(context.Background(), "node-a", "volume")
	if err != nil || !available {
		t.Fatalf("expected existing node to be available, got available=%t err=%v", available, err)
	}
}

func TestNewDeleteVolumeAction(t *testing.T) {
	cs := controllerWithFakeClients()
	action := cs.newDeleteVolumeAction("volume", "node-a", "vg-a", DmThinType)

	if action.action != actionTypeDelete || action.name != "volume" || action.nodeName != "node-a" {
		t.Fatalf("unexpected delete action: %#v", action)
	}
	if action.srcInfo == nil || action.srcInfo.srcLVName != "volume" || action.srcInfo.srcVGName != "vg-a" || action.srcInfo.srcType != DmThinType {
		t.Fatalf("unexpected delete source info: %#v", action.srcInfo)
	}
}

func TestCreateSnapshotDoesNotPanicWhenPVIsAbsent(t *testing.T) {
	cs := controllerWithFakeClients()
	_, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name:           "snapshot-id",
		SourceVolumeId: "missing",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestCloneFromSnapshotRejectsIncompleteContent(t *testing.T) {
	cs := controllerWithFakeClients()
	err := cs.cloneFromSnapshot(
		context.Background(),
		&snapv1.VolumeSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "incomplete"}},
		"destination",
		"node-a",
		DmThinType,
		"vg",
		1048576,
	)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

func TestCloneFromVolumeRejectsMalformedPersistentVolume(t *testing.T) {
	cs := controllerWithFakeClients()
	err := cs.cloneFromVolume(
		context.Background(),
		&v1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "malformed"}},
		"destination",
		"node-a",
		DmThinType,
		"vg",
		1048576,
	)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

func TestCloneFromContentSourceRejectsInvalidSources(t *testing.T) {
	cs := controllerWithFakeClients()
	tests := []struct {
		name   string
		source *csi.VolumeContentSource
	}{
		{name: "nil source"},
		{name: "missing source type", source: &csi.VolumeContentSource{}},
		{
			name: "empty snapshot ID",
			source: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
				Snapshot: &csi.VolumeContentSource_SnapshotSource{},
			}},
		},
		{
			name: "empty volume ID",
			source: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
				Volume: &csi.VolumeContentSource_VolumeSource{},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := cs.cloneFromContentSource(
				context.Background(),
				tt.source,
				"destination",
				"node-a",
				DmThinType,
				"vg",
				1048576,
			)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
		})
	}
}

func TestDeleteSnapshotIsNilSafeAndIdempotent(t *testing.T) {
	t.Run("unrelated incomplete contents are ignored", func(t *testing.T) {
		cs := controllerWithFakeClients()
		cs.snapClient = &fakeSnapshotClient{contents: []snapv1.VolumeSnapshotContent{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "no-status"},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "nil-handle"},
				Status:     &snapv1.VolumeSnapshotContentStatus{},
			},
		}}

		if _, err := cs.DeleteSnapshot(
			context.Background(),
			&csi.DeleteSnapshotRequest{SnapshotId: "missing"},
		); err != nil {
			t.Fatalf("idempotent snapshot delete failed: %v", err)
		}
	})

	t.Run("missing source PV is idempotent", func(t *testing.T) {
		cs := controllerWithFakeClients()
		cs.snapClient = &fakeSnapshotClient{contents: []snapv1.VolumeSnapshotContent{{
			ObjectMeta: metav1.ObjectMeta{Name: "dynamic"},
			Spec: snapv1.VolumeSnapshotContentSpec{
				Source: snapv1.VolumeSnapshotContentSource{
					VolumeHandle: strPointer("missing-volume"),
				},
			},
			Status: &snapv1.VolumeSnapshotContentStatus{
				SnapshotHandle: strPointer("snapshot-id"),
			},
		}}}

		if _, err := cs.DeleteSnapshot(
			context.Background(),
			&csi.DeleteSnapshotRequest{SnapshotId: "snapshot-id"},
		); err != nil {
			t.Fatalf("snapshot delete with missing source PV failed: %v", err)
		}
	})
}

func TestPreExistingSnapshotRequiresLocationAnnotations(t *testing.T) {
	cs := controllerWithFakeClients()
	if _, err := cs.deleteSnapshotAction(context.Background(), "snapshot-id", nil); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected nil content to return FailedPrecondition, got %v", err)
	}

	content := &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "pre-existing"},
		Spec: snapv1.VolumeSnapshotContentSpec{
			Source: snapv1.VolumeSnapshotContentSource{
				SnapshotHandle: strPointer("snapshot-id"),
			},
		},
	}
	if _, err := cs.deleteSnapshotAction(context.Background(), "snapshot-id", content); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}

	content.Annotations = map[string]string{
		snapshotNodeAnnotation: "node-a",
		snapshotVGAnnotation:   "vg",
	}
	action, err := cs.deleteSnapshotAction(context.Background(), "snapshot-id", content)
	if err != nil {
		t.Fatalf("annotated pre-existing snapshot failed: %v", err)
	}
	if action.nodeName != "node-a" || action.vgName != "vg" {
		t.Fatalf("unexpected action: %#v", action)
	}
}

func TestCloneFromSnapshotSourceResolvesContentByHandle(t *testing.T) {
	cs := controllerWithFakeClients()
	restoreSize := int64(2 << 30)
	cs.snapClient = &fakeSnapshotClient{contents: []snapv1.VolumeSnapshotContent{{
		ObjectMeta: metav1.ObjectMeta{Name: "data-mover-content"},
		Spec: snapv1.VolumeSnapshotContentSpec{
			Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer("snapshot-id")},
		},
		Status: &snapv1.VolumeSnapshotContentStatus{
			SnapshotHandle: strPointer("snapshot-id"),
			RestoreSize:    &restoreSize,
		},
	}}}

	err := cs.cloneFromContentSource(
		context.Background(),
		&csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "snapshot-id"},
		}},
		"destination",
		"node-a",
		DmThinType,
		"vg",
		1<<30,
	)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected size validation after handle-based resolution, got %v", err)
	}
}

func TestCloneFromSnapshotSourceReturnsNotFound(t *testing.T) {
	cs := controllerWithFakeClients()
	err := cs.cloneFromContentSource(
		context.Background(),
		&csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "missing"},
		}},
		"destination",
		"node-a",
		DmThinType,
		"vg",
		1<<30,
	)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestPreProvisionedSnapshotCloneAction(t *testing.T) {
	cs := controllerWithFakeClients()
	restoreSize := int64(1 << 30)
	content := &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pre-existing",
			Annotations: map[string]string{
				snapshotNodeAnnotation: "node-a",
				snapshotVGAnnotation:   "source-vg",
			},
		},
		Spec: snapv1.VolumeSnapshotContentSpec{
			Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer("snapshot-id")},
		},
		Status: &snapv1.VolumeSnapshotContentStatus{RestoreSize: &restoreSize},
	}

	action, err := cs.preProvisionedSnapshotCloneAction(
		content,
		"destination",
		"node-a",
		DmThinType,
		"destination-vg",
		2<<30,
	)
	if err != nil {
		t.Fatalf("pre-provisioned clone action failed: %v", err)
	}
	if action.action != actionTypeClone || action.name != "destination" || action.size != 2<<30 {
		t.Fatalf("unexpected clone action: %#v", action)
	}
	if action.srcInfo == nil ||
		action.srcInfo.srcLVName != "lvm-snapshot-id" ||
		action.srcInfo.srcVGName != "source-vg" ||
		action.srcInfo.srcType != DmThinType {
		t.Fatalf("unexpected clone source: %#v", action.srcInfo)
	}
}

func TestPreProvisionedSnapshotCloneActionUsesDestinationLocationFallback(t *testing.T) {
	cs := controllerWithFakeClients()
	zeroSize := int64(0)
	content := &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "pre-provisioned"},
		Spec: snapv1.VolumeSnapshotContentSpec{
			Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer("snapshot-id")},
		},
		Status: &snapv1.VolumeSnapshotContentStatus{RestoreSize: &zeroSize},
	}

	action, err := cs.preProvisionedSnapshotCloneAction(
		content,
		"destination",
		"node-a",
		DmThinType,
		"vg",
		1<<30,
	)
	if err != nil {
		t.Fatalf("fallback clone action failed: %v", err)
	}
	if action.srcInfo == nil || action.srcInfo.srcVGName != "vg" || action.nodeName != "node-a" || action.size != 1<<30 {
		t.Fatalf("unexpected fallback action: %#v", action)
	}
}

func TestPreProvisionedSnapshotCloneActionRejectsNodeMismatch(t *testing.T) {
	cs := controllerWithFakeClients()
	content := &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pre-existing",
			Annotations: map[string]string{snapshotNodeAnnotation: "node-b"},
		},
		Spec: snapv1.VolumeSnapshotContentSpec{
			Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer("snapshot-id")},
		},
	}

	_, err := cs.preProvisionedSnapshotCloneAction(
		content,
		"destination",
		"node-a",
		DmThinType,
		"vg",
		1<<30,
	)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected node mismatch to fail, got %v", err)
	}
}

func TestCloneFromDynamicSnapshotRejectsIncompleteStatus(t *testing.T) {
	cs := controllerWithFakeClients()
	content := &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "dynamic"},
		Spec: snapv1.VolumeSnapshotContentSpec{
			Source: snapv1.VolumeSnapshotContentSource{VolumeHandle: strPointer("source-volume")},
		},
	}

	err := cs.cloneFromSnapshot(
		context.Background(),
		content,
		"destination",
		"node-a",
		DmThinType,
		"vg",
		1<<30,
	)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}
