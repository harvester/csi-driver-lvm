package lvm

import (
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func mountCapability(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

func validPersistentVolume(name string) *v1.PersistentVolume {
	return &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver: "lvm.driver.harvesterhci.io",
					VolumeAttributes: map[string]string{
						"vgName": "vg",
						"type":   DmThinType,
					},
				},
			},
			NodeAffinity: &v1.VolumeNodeAffinity{
				Required: &v1.NodeSelector{
					NodeSelectorTerms: []v1.NodeSelectorTerm{{
						MatchExpressions: []v1.NodeSelectorRequirement{{
							Key:      topologyKeyNode,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"node-a"},
						}},
					}},
				},
			},
		},
	}
}

func TestParseLVMParameters(t *testing.T) {
	tests := []struct {
		name       string
		parameters map[string]string
		wantType   string
		wantVG     string
		wantErr    bool
	}{
		{
			name:       "striped",
			parameters: map[string]string{"type": StripedType, "vgName": "vg-a"},
			wantType:   StripedType,
			wantVG:     "vg-a",
		},
		{
			name:       "dm-thin",
			parameters: map[string]string{"type": DmThinType, "vgName": "vg-b"},
			wantType:   DmThinType,
			wantVG:     "vg-b",
		},
		{
			name:       "missing type",
			parameters: map[string]string{"vgName": "vg-a"},
			wantErr:    true,
		},
		{
			name:       "unsupported type",
			parameters: map[string]string{"type": "linear", "vgName": "vg-a"},
			wantErr:    true,
		},
		{
			name:       "missing vg",
			parameters: map[string]string{"type": DmThinType},
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotVG, err := parseLVMParameters(tt.parameters)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected validation error, got type=%q vg=%q", gotType, gotVG)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLVMParameters failed: %v", err)
			}
			if gotType != tt.wantType || gotVG != tt.wantVG {
				t.Fatalf(
					"unexpected parameters: want type=%q vg=%q, got type=%q vg=%q",
					tt.wantType,
					tt.wantVG,
					gotType,
					gotVG,
				)
			}
		})
	}
}

func TestValidateCapacityRange(t *testing.T) {
	tests := []struct {
		name          string
		capacityRange *csi.CapacityRange
		want          int64
		wantErr       bool
	}{
		{name: "nil range", wantErr: true},
		{
			name:          "zero required bytes",
			capacityRange: &csi.CapacityRange{},
			wantErr:       true,
		},
		{
			name:          "negative required bytes",
			capacityRange: &csi.CapacityRange{RequiredBytes: -1},
			wantErr:       true,
		},
		{
			name:          "valid required bytes",
			capacityRange: &csi.CapacityRange{RequiredBytes: 1048576},
			want:          1048576,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateCapacityRange(tt.capacityRange)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected validation error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateCapacityRange failed: %v", err)
			}
			if got != tt.want {
				t.Fatalf("unexpected capacity: want %d, got %d", tt.want, got)
			}
		})
	}
}

func TestValidateCreateSnapshotRequest(t *testing.T) {
	tests := []struct {
		name         string
		request      *csi.CreateSnapshotRequest
		wantSnapshot string
		wantVolume   string
		wantErr      bool
	}{
		{name: "nil request", wantErr: true},
		{
			name:    "missing source volume",
			request: &csi.CreateSnapshotRequest{Name: "snapshot"},
			wantErr: true,
		},
		{
			name:    "missing snapshot name",
			request: &csi.CreateSnapshotRequest{SourceVolumeId: "volume"},
			wantErr: true,
		},
		{
			name: "valid request",
			request: &csi.CreateSnapshotRequest{
				Name:           "snapshot",
				SourceVolumeId: "volume",
			},
			wantSnapshot: "snapshot",
			wantVolume:   "volume",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshotName, volumeID, err := validateCreateSnapshotRequest(tt.request)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected validation error, got snapshot=%q volume=%q", snapshotName, volumeID)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateCreateSnapshotRequest failed: %v", err)
			}
			if snapshotName != tt.wantSnapshot || volumeID != tt.wantVolume {
				t.Fatalf(
					"unexpected values: want snapshot=%q volume=%q, got snapshot=%q volume=%q",
					tt.wantSnapshot,
					tt.wantVolume,
					snapshotName,
					volumeID,
				)
			}
		})
	}
}

func TestBuildVolumeContext(t *testing.T) {
	parameters := map[string]string{
		"type":   DmThinType,
		"vgName": "vg-a",
	}

	got := buildVolumeContext(parameters, 1048576)
	if got["type"] != DmThinType ||
		got["vgName"] != "vg-a" ||
		got["RequiredBytes"] != "1048576" {
		t.Fatalf("unexpected volume context: %#v", got)
	}
	if _, exists := parameters["RequiredBytes"]; exists {
		t.Fatal("buildVolumeContext must not mutate request parameters")
	}
}

func TestValidateVolumeCapabilitiesRejectsMultiNodeModes(t *testing.T) {
	err := validateVolumeCapabilities([]*csi.VolumeCapability{
		mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
	})
	if err == nil {
		t.Fatal("expected multi-node access mode to be rejected")
	}
}

func TestValidateNodePublishCapabilityRequiresReadonlyForReader(t *testing.T) {
	capability := mountCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY)
	if err := validateNodePublishCapability(capability, false); err == nil {
		t.Fatal("expected readonly=false to be rejected for SINGLE_NODE_READER_ONLY")
	}
	if err := validateNodePublishCapability(capability, true); err != nil {
		t.Fatalf("expected readonly reader capability to be accepted: %v", err)
	}
}

func TestValidateNodePublishRequest(t *testing.T) {
	validRequest := func() *csi.NodePublishVolumeRequest {
		return &csi.NodePublishVolumeRequest{
			VolumeId:         "volume",
			TargetPath:       "/var/lib/kubelet/pods/target",
			VolumeCapability: mountCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
			VolumeContext:    map[string]string{"vgName": "vg-a"},
		}
	}

	t.Run("valid", func(t *testing.T) {
		vgName, err := validateNodePublishRequest(validRequest())
		if err != nil || vgName != "vg-a" {
			t.Fatalf("expected vg-a, got vg=%q err=%v", vgName, err)
		}
	})

	t.Run("missing volume group", func(t *testing.T) {
		req := validRequest()
		delete(req.VolumeContext, "vgName")
		if _, err := validateNodePublishRequest(req); err == nil {
			t.Fatal("expected missing volume group to fail")
		}
	})

	t.Run("relative target path", func(t *testing.T) {
		req := validRequest()
		req.TargetPath = "relative/path"
		if _, err := validateNodePublishRequest(req); err == nil {
			t.Fatal("expected relative target path to fail")
		}
	})
}

func TestValidateNodeUnpublishRequest(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		volumeID, targetPath, err := validateNodeUnpublishRequest(&csi.NodeUnpublishVolumeRequest{
			VolumeId:   "volume",
			TargetPath: "/var/lib/kubelet/pods/target",
		})
		if err != nil || volumeID != "volume" || targetPath != "/var/lib/kubelet/pods/target" {
			t.Fatalf("unexpected result: volumeID=%q targetPath=%q err=%v", volumeID, targetPath, err)
		}
	})

	tests := map[string]*csi.NodeUnpublishVolumeRequest{
		"missing volume ID":    {TargetPath: "/target"},
		"missing target path":  {VolumeId: "volume"},
		"relative target path": {VolumeId: "volume", TargetPath: "relative/path"},
	}
	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := validateNodeUnpublishRequest(req); err == nil {
				t.Fatal("expected validation to fail")
			}
		})
	}
}

func TestValidateNodeExpandRequest(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		volumeID, volumePath, capacity, err := validateNodeExpandRequest(&csi.NodeExpandVolumeRequest{
			VolumeId:      "volume",
			VolumePath:    "/var/lib/kubelet/pods/volume",
			CapacityRange: &csi.CapacityRange{RequiredBytes: 1048576},
		})
		if err != nil || volumeID != "volume" || volumePath != "/var/lib/kubelet/pods/volume" || capacity != 1048576 {
			t.Fatalf("unexpected result: volumeID=%q volumePath=%q capacity=%d err=%v", volumeID, volumePath, capacity, err)
		}
	})

	tests := map[string]*csi.NodeExpandVolumeRequest{
		"missing volume ID": {
			VolumePath:    "/volume",
			CapacityRange: &csi.CapacityRange{RequiredBytes: 1048576},
		},
		"missing volume path": {
			VolumeId:      "volume",
			CapacityRange: &csi.CapacityRange{RequiredBytes: 1048576},
		},
		"missing capacity": {
			VolumeId:   "volume",
			VolumePath: "/volume",
		},
		"non-positive capacity": {
			VolumeId:      "volume",
			VolumePath:    "/volume",
			CapacityRange: &csi.CapacityRange{},
		},
	}
	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := validateNodeExpandRequest(req); err == nil {
				t.Fatal("expected validation to fail")
			}
		})
	}
}

func TestValidateDeleteVolumeRequest(t *testing.T) {
	if err := validateDeleteVolumeRequest(&csi.DeleteVolumeRequest{VolumeId: "volume"}); err != nil {
		t.Fatalf("valid request failed: %v", err)
	}
	if err := validateDeleteVolumeRequest(&csi.DeleteVolumeRequest{}); err == nil {
		t.Fatal("expected missing volume ID to fail")
	}
}

func TestNodeFromAccessibility(t *testing.T) {
	t.Run("preferred node", func(t *testing.T) {
		node, err := nodeFromAccessibility(&csi.TopologyRequirement{
			Preferred: []*csi.Topology{{
				Segments: map[string]string{topologyKeyNode: "node-a"},
			}},
		})
		if err != nil || node != "node-a" {
			t.Fatalf("expected node-a, got node=%q err=%v", node, err)
		}
	})

	t.Run("single requisite fallback", func(t *testing.T) {
		node, err := nodeFromAccessibility(&csi.TopologyRequirement{
			Requisite: []*csi.Topology{{
				Segments: map[string]string{topologyKeyNode: "node-b"},
			}},
		})
		if err != nil || node != "node-b" {
			t.Fatalf("expected node-b, got node=%q err=%v", node, err)
		}
	})

	t.Run("missing requirement", func(t *testing.T) {
		if _, err := nodeFromAccessibility(nil); err == nil {
			t.Fatal("expected nil requirement to fail")
		}
	})

	t.Run("preferred node must be requisite", func(t *testing.T) {
		_, err := nodeFromAccessibility(&csi.TopologyRequirement{
			Preferred: []*csi.Topology{{
				Segments: map[string]string{topologyKeyNode: "node-a"},
			}},
			Requisite: []*csi.Topology{{
				Segments: map[string]string{topologyKeyNode: "node-b"},
			}},
		})
		if err == nil {
			t.Fatal("expected preferred node outside requisite topology to fail")
		}
	})
}

func TestTopologyFromAccessibility(t *testing.T) {
	node, topology, err := topologyFromAccessibility(&csi.TopologyRequirement{
		Preferred: []*csi.Topology{{
			Segments: map[string]string{topologyKeyNode: "node-a"},
		}},
	})
	if err != nil {
		t.Fatalf("topologyFromAccessibility failed: %v", err)
	}
	if node != "node-a" {
		t.Fatalf("expected node-a, got %q", node)
	}
	if len(topology) != 1 || topology[0].GetSegments()[topologyKeyNode] != "node-a" {
		t.Fatalf("unexpected accessible topology: %#v", topology)
	}
}

func TestMetadataFromPersistentVolume(t *testing.T) {
	nodeName, vgName, lvmType, err := metadataFromPV(validPersistentVolume("volume"))
	if err != nil {
		t.Fatalf("valid metadata failed: %v", err)
	}
	if nodeName != "node-a" || vgName != "vg" || lvmType != DmThinType {
		t.Fatalf("unexpected metadata: node=%q vg=%q type=%q", nodeName, vgName, lvmType)
	}

	malformed := validPersistentVolume("malformed")
	malformed.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Values = nil
	if _, _, _, err := metadataFromPV(malformed); err == nil {
		t.Fatal("expected empty topology values to fail")
	}
}

func TestPreProvisionedSnapshotMetadata(t *testing.T) {
	restoreSize := int64(2 << 30)

	t.Run("returns matching status handle and restore size", func(t *testing.T) {
		content := &snapv1.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: "content"},
			Spec: snapv1.VolumeSnapshotContentSpec{
				Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer("snapshot-id")},
			},
			Status: &snapv1.VolumeSnapshotContentStatus{
				SnapshotHandle: strPointer("snapshot-id"),
				RestoreSize:    &restoreSize,
			},
		}
		handle, size, err := preProvisionedSnapshotMetadata(content)
		if err != nil || handle != "snapshot-id" || size != restoreSize {
			t.Fatalf("unexpected metadata: handle=%q size=%d err=%v", handle, size, err)
		}
	})

	t.Run("falls back to source handle without restore size", func(t *testing.T) {
		content := &snapv1.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: "content"},
			Spec: snapv1.VolumeSnapshotContentSpec{
				Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer("snapshot-id")},
			},
		}
		handle, size, err := preProvisionedSnapshotMetadata(content)
		if err != nil || handle != "snapshot-id" || size != 0 {
			t.Fatalf("unexpected fallback metadata: handle=%q size=%d err=%v", handle, size, err)
		}
	})

	t.Run("falls back to destination size when restore size is zero", func(t *testing.T) {
		zeroSize := int64(0)
		content := &snapv1.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: "content"},
			Spec: snapv1.VolumeSnapshotContentSpec{
				Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer("snapshot-id")},
			},
			Status: &snapv1.VolumeSnapshotContentStatus{RestoreSize: &zeroSize},
		}
		handle, size, err := preProvisionedSnapshotMetadata(content)
		if err != nil || handle != "snapshot-id" || size != 0 {
			t.Fatalf("unexpected zero-size metadata: handle=%q size=%d err=%v", handle, size, err)
		}
	})

	t.Run("rejects conflicting handles", func(t *testing.T) {
		content := &snapv1.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: "content"},
			Spec: snapv1.VolumeSnapshotContentSpec{
				Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer("spec-handle")},
			},
			Status: &snapv1.VolumeSnapshotContentStatus{SnapshotHandle: strPointer("status-handle")},
		}
		if _, _, err := preProvisionedSnapshotMetadata(content); err == nil {
			t.Fatal("expected conflicting handles to fail")
		}
	})

	t.Run("rejects missing handle", func(t *testing.T) {
		content := &snapv1.VolumeSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "content"}}
		if _, _, err := preProvisionedSnapshotMetadata(content); err == nil {
			t.Fatal("expected missing handle to fail")
		}
	})

	t.Run("rejects invalid restore size", func(t *testing.T) {
		invalidSize := int64(-1)
		content := &snapv1.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: "content"},
			Spec: snapv1.VolumeSnapshotContentSpec{
				Source: snapv1.VolumeSnapshotContentSource{SnapshotHandle: strPointer("snapshot-id")},
			},
			Status: &snapv1.VolumeSnapshotContentStatus{RestoreSize: &invalidSize},
		}
		if _, _, err := preProvisionedSnapshotMetadata(content); err == nil {
			t.Fatal("expected invalid restore size to fail")
		}
	})
}
