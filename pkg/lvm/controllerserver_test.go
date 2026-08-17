package lvm

import (
	"context"
	"encoding/json"
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

func strPointer(value string) *string {
	return &value
}

type fakeKubeClient struct {
	kubernetes.Interface
	volumes    map[string]*v1.PersistentVolume
	nodes      map[string]*v1.Node
	configmaps map[string]*v1.ConfigMap
}

func (f *fakeKubeClient) CoreV1() corev1.CoreV1Interface {
	return &fakeCoreV1{volumes: f.volumes, nodes: f.nodes, configmaps: f.configmaps}
}

type fakeCoreV1 struct {
	corev1.CoreV1Interface
	volumes    map[string]*v1.PersistentVolume
	nodes      map[string]*v1.Node
	configmaps map[string]*v1.ConfigMap
}

func (f *fakeCoreV1) PersistentVolumes() corev1.PersistentVolumeInterface {
	return &fakePersistentVolumes{volumes: f.volumes}
}

func (f *fakeCoreV1) Nodes() corev1.NodeInterface {
	return &fakeNodes{nodes: f.nodes}
}

func (f *fakeCoreV1) ConfigMaps(_ string) corev1.ConfigMapInterface {
	return &fakeConfigMaps{configmaps: f.configmaps}
}

// fakeConfigMaps implements just enough of ConfigMapInterface for the snapshot
// location store: Get, Create, and a JSON merge Patch (RFC 7386) over .data,
// where a null value deletes the key.
type fakeConfigMaps struct {
	corev1.ConfigMapInterface
	configmaps map[string]*v1.ConfigMap
}

func (f *fakeConfigMaps) Get(_ context.Context, name string, _ metav1.GetOptions) (*v1.ConfigMap, error) {
	if cm := f.configmaps[name]; cm != nil {
		return cm.DeepCopy(), nil
	}
	return nil, k8serror.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
}

func (f *fakeConfigMaps) Create(_ context.Context, cm *v1.ConfigMap, _ metav1.CreateOptions) (*v1.ConfigMap, error) {
	if _, ok := f.configmaps[cm.Name]; ok {
		return nil, k8serror.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, cm.Name)
	}
	f.configmaps[cm.Name] = cm.DeepCopy()
	return cm.DeepCopy(), nil
}

func (f *fakeConfigMaps) Patch(_ context.Context, name string, _ types.PatchType, data []byte, _ metav1.PatchOptions, _ ...string) (*v1.ConfigMap, error) {
	cm := f.configmaps[name]
	if cm == nil {
		return nil, k8serror.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	var patch struct {
		Data map[string]*string `json:"data"`
	}
	if err := json.Unmarshal(data, &patch); err != nil {
		return nil, err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	for k, v := range patch.Data {
		if v == nil {
			delete(cm.Data, k)
			continue
		}
		cm.Data[k] = *v
	}
	return cm.DeepCopy(), nil
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

func (f *fakePersistentVolumes) List(
	_ context.Context,
	_ metav1.ListOptions,
) (*v1.PersistentVolumeList, error) {
	list := &v1.PersistentVolumeList{}
	for _, volume := range f.volumes {
		list.Items = append(list.Items, *volume.DeepCopy())
	}
	return list, nil
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
			configmaps: map[string]*v1.ConfigMap{},
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

func handleOnlyContent(name, handle string, annotations map[string]string) *snapv1.VolumeSnapshotContent {
	return &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		Spec: snapv1.VolumeSnapshotContentSpec{
			Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer(handle)},
		},
	}
}

func TestDeleteSnapshotActionNilContent(t *testing.T) {
	cs := controllerWithFakeClients()
	if _, err := cs.deleteSnapshotAction(context.Background(), "snapshot-id", nil); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected nil content to return FailedPrecondition, got %v", err)
	}
}

func TestPreExistingSnapshotUsesAnnotationOverride(t *testing.T) {
	cs := controllerWithFakeClients()
	content := handleOnlyContent("pre-existing", "snapshot-id", map[string]string{
		snapshotNodeAnnotation: "node-a",
		snapshotVGAnnotation:   "vg",
	})

	actions, err := cs.deleteSnapshotAction(context.Background(), "snapshot-id", content)
	if err != nil {
		t.Fatalf("annotated pre-existing snapshot failed: %v", err)
	}
	if len(actions) != 1 || actions[0].nodeName != "node-a" || actions[0].vgName != "vg" {
		t.Fatalf("expected single action for node-a/vg, got %#v", actions)
	}
}

// A statically-referenced (handle-only) content with no annotations — the shape a
// backup data-mover leaves on the Delete path — must NOT wedge. The driver
// resolves exactly one delete target from the location it recorded for the
// handle at CreateSnapshot.
func TestPreExistingSnapshotUsesRecordedLocation(t *testing.T) {
	cs := controllerWithFakeClients()
	if err := cs.recordSnapshotLocation(context.Background(), "snapshot-id", "node-a", "vmvg"); err != nil {
		t.Fatalf("recordSnapshotLocation failed: %v", err)
	}

	actions, err := cs.deleteSnapshotAction(context.Background(), "snapshot-id",
		handleOnlyContent("velero-exposer", "snapshot-id", nil))
	if err != nil {
		t.Fatalf("expected recorded-location resolution to succeed, got %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected exactly 1 delete target, got %d: %#v", len(actions), actions)
	}
	a := actions[0]
	if a.action != actionTypeDelete || a.snapshotName != "snapshot-id" || a.nodeName != "node-a" || a.vgName != "vmvg" {
		t.Fatalf("unexpected action: %#v", a)
	}
}

// Annotations take precedence over any recorded location (explicit override).
func TestPreExistingSnapshotAnnotationBeatsRecordedLocation(t *testing.T) {
	cs := controllerWithFakeClients()
	if err := cs.recordSnapshotLocation(context.Background(), "snapshot-id", "recorded-node", "recorded-vg"); err != nil {
		t.Fatalf("recordSnapshotLocation failed: %v", err)
	}

	actions, err := cs.deleteSnapshotAction(context.Background(), "snapshot-id",
		handleOnlyContent("annotated", "snapshot-id", map[string]string{
			snapshotNodeAnnotation: "override-node",
			snapshotVGAnnotation:   "override-vg",
		}))
	if err != nil {
		t.Fatalf("annotated delete failed: %v", err)
	}
	if len(actions) != 1 || actions[0].nodeName != "override-node" || actions[0].vgName != "override-vg" {
		t.Fatalf("expected annotation override to win, got %#v", actions)
	}
}

// When neither annotations nor a recorded location exist, the snapshot cannot be
// located, so delete must return success (no actions) rather than a retryable
// error that would permanently jam snapshot reconciliation.
func TestPreExistingSnapshotIsIdempotentWhenUnresolvable(t *testing.T) {
	cs := controllerWithFakeClients()
	actions, err := cs.deleteSnapshotAction(context.Background(), "snapshot-id",
		handleOnlyContent("orphan", "snapshot-id", nil))
	if err != nil {
		t.Fatalf("expected idempotent success, got %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected no actions when unresolvable, got %#v", actions)
	}
}

// The location store must survive lazy ConfigMap creation and round-trip through
// lookup, and forget must remove the entry so it no longer resolves.
func TestSnapshotLocationRecordLookupForget(t *testing.T) {
	cs := controllerWithFakeClients()
	ctx := context.Background()

	// First record creates the ConfigMap; a second records a distinct key without
	// clobbering the first (merge-patch semantics).
	if err := cs.recordSnapshotLocation(ctx, "snap-1", "node-a", "vg-a"); err != nil {
		t.Fatalf("first record failed: %v", err)
	}
	if err := cs.recordSnapshotLocation(ctx, "snap-2", "node-b", "vg-b"); err != nil {
		t.Fatalf("second record failed: %v", err)
	}

	node, vg, found, err := cs.lookupSnapshotLocation(ctx, "snap-1")
	if err != nil || !found || node != "node-a" || vg != "vg-a" {
		t.Fatalf("lookup snap-1 = (%q,%q,%t,%v); want node-a/vg-a", node, vg, found, err)
	}
	if _, _, found, _ := cs.lookupSnapshotLocation(ctx, "snap-2"); !found {
		t.Fatalf("snap-2 entry was clobbered by snap-1 record")
	}

	cs.forgetSnapshotLocation(ctx, "snap-1")
	if _, _, found, _ := cs.lookupSnapshotLocation(ctx, "snap-1"); found {
		t.Fatalf("snap-1 still resolvable after forget")
	}
	if _, _, found, _ := cs.lookupSnapshotLocation(ctx, "snap-2"); !found {
		t.Fatalf("forget snap-1 removed unrelated snap-2 entry")
	}
}

// A missing store must read as "not found", never as an error, so an
// unresolvable delete stays idempotent instead of wedging.
func TestSnapshotLocationLookupMissingStore(t *testing.T) {
	cs := controllerWithFakeClients()
	if _, _, found, err := cs.lookupSnapshotLocation(context.Background(), "absent"); found || err != nil {
		t.Fatalf("expected (found=false, err=nil) for missing store, got found=%t err=%v", found, err)
	}
}

func TestPreExistingSnapshotRejectsMissingHandle(t *testing.T) {
	cs := controllerWithFakeClients()
	content := &snapv1.VolumeSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "no-handle"}}
	if _, err := cs.deleteSnapshotAction(context.Background(), "snapshot-id", content); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition for content without a source handle, got %v", err)
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
