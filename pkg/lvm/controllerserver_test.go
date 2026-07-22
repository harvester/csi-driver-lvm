package lvm

import (
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func strPtr(s string) *string { return &s }

// TestSnapshotSourceVolumeHandle covers harvester/harvester#11105: the snapshot
// delete path used to dereference SnapshotHandle / Spec.Source.VolumeHandle
// unconditionally while scanning every VolumeSnapshotContent, panicking the plugin
// (which then CrashLooped on sidecar retries). Malformed contents must be skipped,
// and a well-formed one must still resolve.
func TestSnapshotSourceVolumeHandle(t *testing.T) {
	// Contents that previously caused the panic must resolve to "" (skipped), not crash:
	malformed := []snapv1.VolumeSnapshotContent{
		// nil Status
		{ObjectMeta: metav1.ObjectMeta{Name: "no-status"}},
		// Status present but SnapshotHandle nil (never became Ready / abandoned CDI clone)
		{ObjectMeta: metav1.ObjectMeta{Name: "nil-handle"}, Status: &snapv1.VolumeSnapshotContentStatus{}},
		// Matching handle, but source is a snapshot -> VolumeHandle nil (restore-from-snapshot clone).
		{
			ObjectMeta: metav1.ObjectMeta{Name: "clone-nil-volhandle"},
			Spec:       snapv1.VolumeSnapshotContentSpec{Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPtr("snapshot-xyz")}},
			Status:     &snapv1.VolumeSnapshotContentStatus{SnapshotHandle: strPtr("snapshot-xyz")},
		},
	}
	if got := snapshotSourceVolumeHandle(malformed, "snapshot-xyz"); got != "" {
		t.Fatalf("expected empty (unresolvable) volume handle, got %q", got)
	}

	// A well-formed content resolves to its source volume handle.
	good := []snapv1.VolumeSnapshotContent{{
		ObjectMeta: metav1.ObjectMeta{Name: "good"},
		Spec:       snapv1.VolumeSnapshotContentSpec{Source: snapv1.VolumeSnapshotContentSource{VolumeHandle: strPtr("pvc-123")}},
		Status:     &snapv1.VolumeSnapshotContentStatus{SnapshotHandle: strPtr("snapshot-xyz")},
	}}
	if got := snapshotSourceVolumeHandle(good, "snapshot-xyz"); got != "pvc-123" {
		t.Fatalf("expected pvc-123, got %q", got)
	}
}

func TestDeleteSnapshotRejectsEmptyID(t *testing.T) {
	cs := &controllerServer{}
	_, err := cs.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for empty snapshot ID, got: %v", err)
	}
}
