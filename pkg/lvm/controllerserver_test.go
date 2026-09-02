package lvm

import (
	"context"
	"errors"
	"strings"
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
	volumes            map[string]*v1.PersistentVolume
	nodes              map[string]*v1.Node
	configMaps         map[string]*v1.ConfigMap
	configMapCreateErr error
}

func (f *fakeKubeClient) CoreV1() corev1.CoreV1Interface {
	return &fakeCoreV1{
		volumes:            f.volumes,
		nodes:              f.nodes,
		configMaps:         f.configMaps,
		configMapCreateErr: f.configMapCreateErr,
	}
}

type fakeCoreV1 struct {
	corev1.CoreV1Interface
	volumes            map[string]*v1.PersistentVolume
	nodes              map[string]*v1.Node
	configMaps         map[string]*v1.ConfigMap
	configMapCreateErr error
}

func (f *fakeCoreV1) PersistentVolumes() corev1.PersistentVolumeInterface {
	return &fakePersistentVolumes{volumes: f.volumes}
}

func (f *fakeCoreV1) Nodes() corev1.NodeInterface {
	return &fakeNodes{nodes: f.nodes}
}

func (f *fakeCoreV1) ConfigMaps(_ string) corev1.ConfigMapInterface {
	return &fakeConfigMaps{configMaps: f.configMaps, createErr: f.configMapCreateErr}
}

// Pods lets tests drive an RPC past the point where it launches a provisioner
// pod: the RPC fails with an ordinary error instead of dereferencing the
// unimplemented embedded interface.
func (f *fakeCoreV1) Pods(_ string) corev1.PodInterface {
	return &fakePods{}
}

type fakePods struct {
	corev1.PodInterface
}

func (f *fakePods) Create(_ context.Context, _ *v1.Pod, _ metav1.CreateOptions) (*v1.Pod, error) {
	return nil, errors.New("the fake client does not run provisioner pods")
}

type fakePersistentVolumes struct {
	corev1.PersistentVolumeInterface
	volumes map[string]*v1.PersistentVolume
}

type fakeNodes struct {
	corev1.NodeInterface
	nodes map[string]*v1.Node
}

type fakeConfigMaps struct {
	corev1.ConfigMapInterface
	configMaps map[string]*v1.ConfigMap
	createErr  error
}

func (f *fakeConfigMaps) Get(
	_ context.Context,
	name string,
	_ metav1.GetOptions,
) (*v1.ConfigMap, error) {
	if configMap := f.configMaps[name]; configMap != nil {
		return configMap.DeepCopy(), nil
	}
	return nil, k8serror.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
}

func (f *fakeConfigMaps) Create(
	_ context.Context,
	configMap *v1.ConfigMap,
	_ metav1.CreateOptions,
) (*v1.ConfigMap, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.configMaps[configMap.Name] != nil {
		return nil, k8serror.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, configMap.Name)
	}
	f.configMaps[configMap.Name] = configMap.DeepCopy()
	return configMap.DeepCopy(), nil
}

func (f *fakeConfigMaps) Delete(
	_ context.Context,
	name string,
	_ metav1.DeleteOptions,
) error {
	if f.configMaps[name] == nil {
		return k8serror.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	delete(f.configMaps, name)
	return nil
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
			volumes:    map[string]*v1.PersistentVolume{},
			nodes:      map[string]*v1.Node{},
			configMaps: map[string]*v1.ConfigMap{},
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

func TestDeleteDynamicSnapshotFallsBackToRecordedLocation(t *testing.T) {
	tests := []struct {
		name   string
		volume *v1.PersistentVolume
	}{
		{
			name: "source PV is missing",
		},
		{
			name: "source PV metadata is invalid",
			volume: &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "source-volume"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			cs := controllerWithFakeClients()
			if test.volume != nil {
				cs.kubeClient.(*fakeKubeClient).volumes[test.volume.Name] = test.volume
			}
			if err := cs.recordSnapshotLocation(ctx, "snapshot-id", "recorded-node", "recorded-vg", false); err != nil {
				t.Fatalf("failed to record snapshot location: %v", err)
			}

			action, err := cs.deleteDynamicSnapshotAction(ctx, "snapshot-id", "source-volume")
			if err != nil {
				t.Fatalf("recorded-location fallback failed: %v", err)
			}
			if action == nil || action.nodeName != "recorded-node" || action.vgName != "recorded-vg" {
				t.Fatalf("unexpected fallback action: %#v", action)
			}
		})
	}
}

func TestPreExistingSnapshotResolvesLocation(t *testing.T) {
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
		t.Fatalf("expected unrecorded snapshot to return FailedPrecondition, got %v", err)
	}

	content.Annotations = map[string]string{snapshotNodeAnnotation: "node-a"}
	if _, err := cs.deleteSnapshotAction(context.Background(), "snapshot-id", content); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected incomplete annotations to return FailedPrecondition, got %v", err)
	}

	content.Annotations = map[string]string{
		snapshotNodeAnnotation: "node-a",
		snapshotVGAnnotation:   "--config",
	}
	if _, err := cs.deleteSnapshotAction(context.Background(), "snapshot-id", content); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected an option-like volume group to return FailedPrecondition, got %v", err)
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

	content.Annotations = nil
	if err := cs.recordSnapshotLocation(context.Background(), "snapshot-id", "recorded-node", "recorded-vg", false); err != nil {
		t.Fatalf("failed to record snapshot location: %v", err)
	}
	action, err = cs.deleteSnapshotAction(context.Background(), "snapshot-id", content)
	if err != nil {
		t.Fatalf("recorded pre-existing snapshot failed: %v", err)
	}
	if action.nodeName != "recorded-node" || action.vgName != "recorded-vg" {
		t.Fatalf("unexpected recorded-location action: %#v", action)
	}

	content.Annotations = map[string]string{
		snapshotNodeAnnotation: "override-node",
		snapshotVGAnnotation:   "override-vg",
	}
	action, err = cs.deleteSnapshotAction(context.Background(), "snapshot-id", content)
	if err != nil {
		t.Fatalf("annotation override failed: %v", err)
	}
	if action.nodeName != "override-node" || action.vgName != "override-vg" {
		t.Fatalf("annotations did not override the recorded location: %#v", action)
	}
}

func TestSnapshotLocationRecordLifecycle(t *testing.T) {
	cs := controllerWithFakeClients()
	ctx := context.Background()
	handle := "snapshot/with unicode 雪 and characters + not valid in a ConfigMap key"
	name := snapshotLocationConfigMapName(handle)
	if err := cs.recordSnapshotLocation(ctx, handle, "node-a", "--config", false); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected an option-like volume group to return FailedPrecondition, got %v", err)
	}

	if name == handle || len(name) > 253 {
		t.Fatalf("snapshot location name is not a safe derived name: %q", name)
	}
	if err := cs.recordSnapshotLocation(ctx, handle, "node-a", "vg-a", false); err != nil {
		t.Fatalf("recordSnapshotLocation failed: %v", err)
	}
	location := cs.kubeClient.(*fakeKubeClient).configMaps[name]
	if location == nil || location.Immutable == nil || !*location.Immutable {
		t.Fatalf("snapshot location ConfigMap is not immutable: %#v", location)
	}
	// CreateSnapshot is idempotent, so recording the same location again must be
	// idempotent as well.
	if err := cs.recordSnapshotLocation(ctx, handle, "node-a", "vg-a", false); err != nil {
		t.Fatalf("idempotent recordSnapshotLocation failed: %v", err)
	}

	recorded, found, err := cs.lookupSnapshotLocation(ctx, handle)
	if err != nil || !found || recorded.nodeName != "node-a" || recorded.vgName != "vg-a" {
		t.Fatalf("lookupSnapshotLocation = (%#v, %t, %v)", recorded, found, err)
	}
	if recorded.encrypted != "false" {
		t.Fatalf("expected the record to persist the source encryption state, got %q", recorded.encrypted)
	}

	err = cs.recordSnapshotLocation(ctx, handle, "node-b", "vg-b", false)
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected AlreadyExists for a conflicting location record, got %v", err)
	}
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("conflict error does not include ConfigMap name %q: %v", name, err)
	}
	if err := cs.forgetSnapshotLocation(ctx, handle); err != nil {
		t.Fatalf("forgetSnapshotLocation failed: %v", err)
	}
	if _, found, err := cs.lookupSnapshotLocation(ctx, handle); err != nil || found {
		t.Fatalf("expected forgotten location to be absent, found=%t err=%v", found, err)
	}
	if err := cs.forgetSnapshotLocation(ctx, handle); err != nil {
		t.Fatalf("idempotent forgetSnapshotLocation failed: %v", err)
	}
}

func TestSnapshotLocationRejectsMalformedRecord(t *testing.T) {
	cs := controllerWithFakeClients()
	handle := "snapshot-id"
	configMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   snapshotLocationConfigMapName(handle),
			Labels: map[string]string{snapshotLocationLabel: "true"},
		},
		Data: map[string]string{
			snapshotLocationHandleKey: handle,
			snapshotLocationNodeKey:   "node-a",
		},
	}
	cs.kubeClient.(*fakeKubeClient).configMaps[snapshotLocationConfigMapName(handle)] = configMap

	if _, _, err := cs.lookupSnapshotLocation(context.Background(), handle); err == nil {
		t.Fatal("expected an incomplete location record to fail")
	}
	configMap.Data[snapshotLocationVGKey] = "--config"
	if _, _, err := cs.lookupSnapshotLocation(context.Background(), handle); err == nil {
		t.Fatal("expected a location record with an option-like volume group to fail")
	}
	if err := cs.recordSnapshotLocation(context.Background(), handle, "node-a", "vg-a", false); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition for an invalid location record, got %v", err)
	}
}

func TestSnapshotLocationReturnsUnavailableForAPIFailure(t *testing.T) {
	cs := controllerWithFakeClients()
	cs.kubeClient.(*fakeKubeClient).configMapCreateErr = k8serror.NewServiceUnavailable("API server unavailable")

	err := cs.recordSnapshotLocation(context.Background(), "snapshot-id", "node-a", "vg-a", false)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable for an API failure, got %v", err)
	}
}

func TestCloneFromSnapshotSourceResolvesContentByHandle(t *testing.T) {
	cs := controllerWithFakeClients()
	restoreSize := int64(2 << 30)
	cs.snapClient = &fakeSnapshotClient{contents: []snapv1.VolumeSnapshotContent{{
		ObjectMeta: metav1.ObjectMeta{
			Name: "data-mover-content",
			Annotations: map[string]string{
				snapshotNodeAnnotation: "node-a",
				snapshotVGAnnotation:   "vg",
			},
		},
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
		context.Background(),
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

func TestPreProvisionedSnapshotCloneActionUsesRecordedLocation(t *testing.T) {
	cs := controllerWithFakeClients()
	if err := cs.recordSnapshotLocation(context.Background(), "snapshot-id", "node-a", "source-vg", false); err != nil {
		t.Fatalf("recordSnapshotLocation failed: %v", err)
	}
	zeroSize := int64(0)
	content := &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "pre-provisioned"},
		Spec: snapv1.VolumeSnapshotContentSpec{
			Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer("snapshot-id")},
		},
		Status: &snapv1.VolumeSnapshotContentStatus{RestoreSize: &zeroSize},
	}

	action, err := cs.preProvisionedSnapshotCloneAction(
		context.Background(),
		content,
		"destination",
		"node-a",
		DmThinType,
		"destination-vg",
		1<<30,
	)
	if err != nil {
		t.Fatalf("recorded-location clone action failed: %v", err)
	}
	if action.srcInfo == nil || action.srcInfo.srcVGName != "source-vg" || action.nodeName != "node-a" || action.size != 1<<30 {
		t.Fatalf("unexpected recorded-location action: %#v", action)
	}
}

func TestPreProvisionedSnapshotCloneActionRejectsMissingLocation(t *testing.T) {
	cs := controllerWithFakeClients()
	content := &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "pre-existing"},
		Spec: snapv1.VolumeSnapshotContentSpec{
			Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer("snapshot-id")},
		},
	}

	_, err := cs.preProvisionedSnapshotCloneAction(
		context.Background(),
		content,
		"destination",
		"node-a",
		DmThinType,
		"vg",
		1<<30,
	)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected a missing source location to return FailedPrecondition, got %v", err)
	}
}

func TestPreProvisionedSnapshotCloneActionRejectsInvalidVG(t *testing.T) {
	cs := controllerWithFakeClients()
	content := &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pre-existing",
			Annotations: map[string]string{
				snapshotNodeAnnotation: "node-a",
				snapshotVGAnnotation:   "--config",
			},
		},
		Spec: snapv1.VolumeSnapshotContentSpec{
			Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer("snapshot-id")},
		},
	}

	_, err := cs.preProvisionedSnapshotCloneAction(
		context.Background(),
		content,
		"destination",
		"node-a",
		DmThinType,
		"vg",
		1<<30,
	)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected an option-like volume group to return FailedPrecondition, got %v", err)
	}
}

func TestPreProvisionedSnapshotCloneActionRejectsNodeMismatch(t *testing.T) {
	cs := controllerWithFakeClients()
	content := &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pre-existing",
			Annotations: map[string]string{
				snapshotNodeAnnotation: "node-b",
				snapshotVGAnnotation:   "vg",
			},
		},
		Spec: snapv1.VolumeSnapshotContentSpec{
			Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer("snapshot-id")},
		},
	}

	_, err := cs.preProvisionedSnapshotCloneAction(
		context.Background(),
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

// lvmPersistentVolume builds a PV the way the controller records one, so the
// restore-validation paths can read back node, VG, size and encryption state.
func lvmPersistentVolume(name, node, vgName string, encrypted bool) *v1.PersistentVolume {
	attributes := map[string]string{
		"type":          DmThinType,
		"vgName":        vgName,
		"RequiredBytes": "1073741824",
	}
	if encrypted {
		attributes[encryptedParam] = "true"
	}
	return &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{VolumeAttributes: attributes},
			},
			NodeAffinity: &v1.VolumeNodeAffinity{
				Required: &v1.NodeSelector{
					NodeSelectorTerms: []v1.NodeSelectorTerm{{
						MatchExpressions: []v1.NodeSelectorRequirement{{
							Key:      topologyKeyNode,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{node},
						}},
					}},
				},
			},
		},
	}
}

func volumeContentSource(volumeID string) *csi.VolumeContentSource {
	return &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
		Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: volumeID},
	}}
}

func snapshotContentSource(snapshotID string) *csi.VolumeContentSource {
	return &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
		Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: snapshotID},
	}}
}

func luksSecret() map[string]string {
	return map[string]string{cryptoKeyValue: "source-passphrase"}
}

// A restore is a block-level clone, so it cannot convert encryption state.
// Both mismatch directions have to be refused before any LV is created:
// unencrypted -> encrypted would LUKS-format restored data, and
// encrypted -> unencrypted would expose the raw LUKS container.
func TestValidateRestoreEncryptionRejectsStateMismatch(t *testing.T) {
	tests := []struct {
		name         string
		srcEncrypted bool
		dstEncrypted bool
		secrets      map[string]string
		wantCode     codes.Code
	}{
		{name: "plain to plain", wantCode: codes.OK},
		{
			name:         "encrypted to encrypted",
			srcEncrypted: true,
			dstEncrypted: true,
			secrets:      luksSecret(),
			wantCode:     codes.OK,
		},
		{name: "plain to encrypted", dstEncrypted: true, secrets: luksSecret(), wantCode: codes.InvalidArgument},
		{name: "encrypted to plain", srcEncrypted: true, wantCode: codes.InvalidArgument},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := controllerWithFakeClients()
			cs.kubeClient.(*fakeKubeClient).volumes["source"] = lvmPersistentVolume("source", "node-a", "vg", tt.srcEncrypted)

			err := cs.validateRestoreEncryption(
				context.Background(),
				volumeContentSource("source"),
				tt.dstEncrypted,
				tt.secrets,
			)
			if status.Code(err) != tt.wantCode {
				t.Fatalf("expected %v, got %v", tt.wantCode, err)
			}
		})
	}
}

// The restored LV keeps its source's LUKS header, so only the source's
// passphrase can open it. Missing credentials must fail in CreateVolume with an
// actionable message instead of surfacing much later on the node.
func TestValidateRestoreEncryptionRequiresCredential(t *testing.T) {
	tests := []struct {
		name    string
		secrets map[string]string
	}{
		{name: "no secret at all"},
		{name: "empty secret", secrets: map[string]string{}},
		{name: "secret without passphrase", secrets: map[string]string{cryptoKeyCipher: "aes-xts-plain64"}},
		{name: "empty passphrase", secrets: map[string]string{cryptoKeyValue: ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := controllerWithFakeClients()
			cs.kubeClient.(*fakeKubeClient).volumes["source"] = lvmPersistentVolume("source", "node-a", "vg", true)

			err := cs.validateRestoreEncryption(context.Background(), volumeContentSource("source"), true, tt.secrets)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
			if !strings.Contains(err.Error(), cryptoKeyValue) {
				t.Fatalf("error should name the missing secret field: %v", err)
			}
		})
	}
}

// The credential error must stay useful without echoing anything secret.
func TestValidateRestoreEncryptionErrorDoesNotLeakSecrets(t *testing.T) {
	const passphrase = "super-secret-passphrase"
	cs := controllerWithFakeClients()
	cs.kubeClient.(*fakeKubeClient).volumes["source"] = lvmPersistentVolume("source", "node-a", "vg", false)

	err := cs.validateRestoreEncryption(
		context.Background(),
		volumeContentSource("source"),
		true,
		map[string]string{cryptoKeyValue: passphrase},
	)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
	if strings.Contains(err.Error(), passphrase) {
		t.Fatalf("passphrase leaked into the error: %v", err)
	}
}

// CreateVolume is the gate an unsafe restore has to pass, so assert the
// rejection there and not only in the helper.
func TestCreateVolumeRejectsUnencryptedRestoreIntoEncryptedClass(t *testing.T) {
	cs := controllerWithFakeClients()
	cs.kubeClient.(*fakeKubeClient).volumes["source"] = lvmPersistentVolume("source", "node-a", "vg", false)

	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "destination",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
		Parameters: map[string]string{
			"type":         DmThinType,
			"vgName":       "vg",
			encryptedParam: "true",
		},
		Secrets:             luksSecret(),
		VolumeContentSource: volumeContentSource("source"),
		AccessibilityRequirements: &csi.TopologyRequirement{
			Preferred: []*csi.Topology{{Segments: map[string]string{topologyKeyNode: "node-a"}}},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

// Snapshot restores resolve the source state from the still-present source PV.
func TestSnapshotRestoreEncryptionStateFromSourceVolume(t *testing.T) {
	cs := controllerWithFakeClients()
	cs.kubeClient.(*fakeKubeClient).volumes["source"] = lvmPersistentVolume("source", "node-a", "vg", true)
	cs.snapClient = &fakeSnapshotClient{contents: []snapv1.VolumeSnapshotContent{{
		ObjectMeta: metav1.ObjectMeta{Name: "dynamic"},
		Spec: snapv1.VolumeSnapshotContentSpec{
			Source: snapv1.VolumeSnapshotContentSource{VolumeHandle: strPointer("source")},
		},
		Status: &snapv1.VolumeSnapshotContentStatus{SnapshotHandle: strPointer("snapshot-id")},
	}}}

	ctx := context.Background()
	if err := cs.validateRestoreEncryption(ctx, snapshotContentSource("snapshot-id"), true, luksSecret()); err != nil {
		t.Fatalf("encrypted snapshot into an encrypted class must be allowed, got %v", err)
	}
	if err := cs.validateRestoreEncryption(ctx, snapshotContentSource("snapshot-id"), false, nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for an encrypted snapshot into a plain class, got %v", err)
	}
}

// When the source PV is gone the recorded (non-secret) encryption state, or an
// explicit annotation, has to carry the decision.
func TestSnapshotRestoreEncryptionStateWithoutSourceVolume(t *testing.T) {
	newController := func(t *testing.T, content snapv1.VolumeSnapshotContent) *controllerServer {
		t.Helper()
		cs := controllerWithFakeClients()
		cs.snapClient = &fakeSnapshotClient{contents: []snapv1.VolumeSnapshotContent{content}}
		return cs
	}
	preProvisioned := snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "pre-provisioned"},
		Spec: snapv1.VolumeSnapshotContentSpec{
			Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer("snapshot-id")},
		},
	}

	t.Run("recorded state blocks a mismatched restore", func(t *testing.T) {
		cs := newController(t, preProvisioned)
		if err := cs.recordSnapshotLocation(context.Background(), "snapshot-id", "node-a", "vg", true); err != nil {
			t.Fatalf("recordSnapshotLocation failed: %v", err)
		}
		err := cs.validateRestoreEncryption(context.Background(), snapshotContentSource("snapshot-id"), false, nil)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument, got %v", err)
		}
	})

	t.Run("recorded state allows a matching restore", func(t *testing.T) {
		cs := newController(t, preProvisioned)
		if err := cs.recordSnapshotLocation(context.Background(), "snapshot-id", "node-a", "vg", true); err != nil {
			t.Fatalf("recordSnapshotLocation failed: %v", err)
		}
		if err := cs.validateRestoreEncryption(context.Background(), snapshotContentSource("snapshot-id"), true, luksSecret()); err != nil {
			t.Fatalf("matching encrypted restore failed: %v", err)
		}
	})

	t.Run("annotation overrides the record", func(t *testing.T) {
		annotated := *preProvisioned.DeepCopy()
		annotated.Annotations = map[string]string{snapshotEncryptedAnnotation: "false"}
		cs := newController(t, annotated)
		if err := cs.recordSnapshotLocation(context.Background(), "snapshot-id", "node-a", "vg", true); err != nil {
			t.Fatalf("recordSnapshotLocation failed: %v", err)
		}
		if err := cs.validateRestoreEncryption(context.Background(), snapshotContentSource("snapshot-id"), false, nil); err != nil {
			t.Fatalf("annotation-declared plain snapshot into a plain class failed: %v", err)
		}
	})

	t.Run("malformed annotation is rejected", func(t *testing.T) {
		annotated := *preProvisioned.DeepCopy()
		annotated.Annotations = map[string]string{snapshotEncryptedAnnotation: "maybe"}
		cs := newController(t, annotated)
		err := cs.validateRestoreEncryption(context.Background(), snapshotContentSource("snapshot-id"), false, nil)
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("expected FailedPrecondition, got %v", err)
		}
	})
}

// Location records written before encryption support carry no state. Restoring
// them into a plain class stays allowed (nothing formats, and the node still
// checks for a stray LUKS header); into an encrypted class it must be refused.
func TestSnapshotRestoreEncryptionStateUnknownForLegacyRecord(t *testing.T) {
	newController := func() *controllerServer {
		cs := controllerWithFakeClients()
		cs.snapClient = &fakeSnapshotClient{contents: []snapv1.VolumeSnapshotContent{{
			ObjectMeta: metav1.ObjectMeta{Name: "pre-provisioned"},
			Spec: snapv1.VolumeSnapshotContentSpec{
				Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer("snapshot-id")},
			},
		}}}
		name := snapshotLocationConfigMapName("snapshot-id")
		cs.kubeClient.(*fakeKubeClient).configMaps[name] = &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: map[string]string{snapshotLocationLabel: "true"},
			},
			Data: map[string]string{
				snapshotLocationHandleKey: "snapshot-id",
				snapshotLocationNodeKey:   "node-a",
				snapshotLocationVGKey:     "vg",
			},
		}
		return cs
	}

	if err := newController().validateRestoreEncryption(
		context.Background(),
		snapshotContentSource("snapshot-id"),
		false,
		nil,
	); err != nil {
		t.Fatalf("legacy record into a plain class must stay allowed, got %v", err)
	}

	err := newController().validateRestoreEncryption(
		context.Background(),
		snapshotContentSource("snapshot-id"),
		true,
		luksSecret(),
	)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for an unknown source state, got %v", err)
	}
}

// A legacy record must not make an otherwise idempotent CreateSnapshot retry
// fail just because it now also records the encryption state.
func TestRecordSnapshotLocationToleratesLegacyRecordWithoutEncryptionState(t *testing.T) {
	cs := controllerWithFakeClients()
	name := snapshotLocationConfigMapName("snapshot-id")
	cs.kubeClient.(*fakeKubeClient).configMaps[name] = &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{snapshotLocationLabel: "true"},
		},
		Data: map[string]string{
			snapshotLocationHandleKey: "snapshot-id",
			snapshotLocationNodeKey:   "node-a",
			snapshotLocationVGKey:     "vg",
		},
	}

	if err := cs.recordSnapshotLocation(context.Background(), "snapshot-id", "node-a", "vg", true); err != nil {
		t.Fatalf("legacy record must not break an idempotent re-record: %v", err)
	}

	err := cs.recordSnapshotLocation(context.Background(), "snapshot-id", "node-a", "vg", false)
	if err != nil {
		t.Fatalf("legacy record must not break an idempotent re-record: %v", err)
	}
}

// A genuine disagreement about encryption state is a conflict, not a retry.
func TestRecordSnapshotLocationRejectsConflictingEncryptionState(t *testing.T) {
	cs := controllerWithFakeClients()
	ctx := context.Background()
	if err := cs.recordSnapshotLocation(ctx, "snapshot-id", "node-a", "vg", true); err != nil {
		t.Fatalf("recordSnapshotLocation failed: %v", err)
	}
	err := cs.recordSnapshotLocation(ctx, "snapshot-id", "node-a", "vg", false)
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected AlreadyExists, got %v", err)
	}
}

// CreateSnapshot has to persist the source's encryption state so a later
// restore can validate the destination even after the source PV is gone.
func TestCreateSnapshotRecordsEncryptionState(t *testing.T) {
	cs := controllerWithFakeClients()
	cs.kubeClient.(*fakeKubeClient).volumes["source"] = lvmPersistentVolume("source", "node-a", "vg", true)

	// createSnapshotterPod needs a live cluster, so the RPC fails after the
	// record is written; only the record is under test here.
	_, _ = cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name:           "snapshot-id",
		SourceVolumeId: "source",
	})

	recorded, found, err := cs.lookupSnapshotLocation(context.Background(), "snapshot-id")
	if err != nil || !found {
		t.Fatalf("lookupSnapshotLocation = (%#v, %t, %v)", recorded, found, err)
	}
	if recorded.encrypted != "true" {
		t.Fatalf("expected the record to persist encryption state \"true\", got %q", recorded.encrypted)
	}
}
