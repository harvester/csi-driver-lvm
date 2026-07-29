package lvm

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/container-storage-interface/spec/lib/go/csi"
	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	v1 "k8s.io/api/core/v1"
)

func parseLVMParameters(parameters map[string]string) (string, string, error) {
	lvmType := parameters["type"]
	if lvmType != StripedType && lvmType != DmThinType {
		return "", "", fmt.Errorf("lvmType is incorrect: %s", lvmType)
	}

	vgName := parameters["vgName"]
	if vgName == "" {
		return "", "", fmt.Errorf("vgName is missing, please check the storage class")
	}

	return lvmType, vgName, nil
}

func validateCapacityRange(capacityRange *csi.CapacityRange) (int64, error) {
	if capacityRange == nil || capacityRange.GetRequiredBytes() <= 0 {
		return 0, fmt.Errorf("capacity range with required bytes greater than zero is required")
	}
	return capacityRange.GetRequiredBytes(), nil
}

func validateCreateSnapshotRequest(req *csi.CreateSnapshotRequest) (string, string, error) {
	if req.GetSourceVolumeId() == "" {
		return "", "", fmt.Errorf("source volume ID missing in request")
	}
	if req.GetName() == "" {
		return "", "", fmt.Errorf("snapshot name missing in request")
	}
	return req.GetName(), req.GetSourceVolumeId(), nil
}

func validateDeleteVolumeRequest(req *csi.DeleteVolumeRequest) error {
	if req.GetVolumeId() == "" {
		return fmt.Errorf("volume ID missing in request")
	}
	return nil
}

func buildVolumeContext(parameters map[string]string, requiredBytes int64) map[string]string {
	volumeContext := make(map[string]string, len(parameters)+1)
	for key, value := range parameters {
		volumeContext[key] = value
	}
	volumeContext["RequiredBytes"] = strconv.FormatInt(requiredBytes, 10)
	return volumeContext
}

func validateVolumeCapabilities(capabilities []*csi.VolumeCapability) error {
	if len(capabilities) == 0 {
		return fmt.Errorf("volume capabilities are required")
	}

	var hasBlock, hasMount bool
	for _, capability := range capabilities {
		if capability == nil {
			return fmt.Errorf("volume capability must not be nil")
		}

		block := capability.GetBlock() != nil
		mount := capability.GetMount() != nil
		if block == mount {
			return fmt.Errorf("volume capability must specify exactly one of block or mount access type")
		}
		hasBlock = hasBlock || block
		hasMount = hasMount || mount

		if capability.GetAccessMode() == nil {
			return fmt.Errorf("volume access mode is required")
		}
		switch capability.GetAccessMode().GetMode() {
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER:
		default:
			return fmt.Errorf("unsupported access mode %s", capability.GetAccessMode().GetMode())
		}
	}

	if hasBlock && hasMount {
		return fmt.Errorf("cannot combine block and mount access types")
	}
	return nil
}

func validateNodePublishCapability(capability *csi.VolumeCapability, readOnly bool) error {
	if err := validateVolumeCapabilities([]*csi.VolumeCapability{capability}); err != nil {
		return err
	}
	if capability.GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY && !readOnly {
		return fmt.Errorf("SINGLE_NODE_READER_ONLY requires readonly=true")
	}
	return nil
}

func validateNodePublishRequest(req *csi.NodePublishVolumeRequest) (string, error) {
	if req.GetVolumeCapability() == nil {
		return "", fmt.Errorf("volume capability missing in request")
	}
	if req.GetVolumeId() == "" {
		return "", fmt.Errorf("volume ID missing in request")
	}
	if req.GetTargetPath() == "" {
		return "", fmt.Errorf("target path missing in request")
	}
	if !filepath.IsAbs(req.GetTargetPath()) {
		return "", fmt.Errorf("target path must be absolute")
	}
	if err := validateNodePublishCapability(req.GetVolumeCapability(), req.GetReadonly()); err != nil {
		return "", err
	}

	vgName := req.GetVolumeContext()["vgName"]
	if vgName == "" {
		return "", fmt.Errorf("vgName is missing from volume context")
	}
	return vgName, nil
}

func validateNodeUnpublishRequest(req *csi.NodeUnpublishVolumeRequest) (string, string, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return "", "", fmt.Errorf("volume ID missing in request")
	}

	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return "", "", fmt.Errorf("target path missing in request")
	}
	if !filepath.IsAbs(targetPath) {
		return "", "", fmt.Errorf("target path must be absolute")
	}
	return volumeID, targetPath, nil
}

func validateNodeExpandRequest(req *csi.NodeExpandVolumeRequest) (string, string, int64, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return "", "", 0, fmt.Errorf("volume ID missing in request")
	}

	volumePath := req.GetVolumePath()
	if volumePath == "" {
		return "", "", 0, fmt.Errorf("volume path not provided")
	}

	capacity, err := validateCapacityRange(req.GetCapacityRange())
	if err != nil {
		return "", "", 0, err
	}
	return volumeID, volumePath, capacity, nil
}

func nodeFromAccessibility(requirement *csi.TopologyRequirement) (string, error) {
	if requirement == nil {
		return "", fmt.Errorf("accessibility requirements are required for local LVM volumes")
	}

	requisiteNodes := map[string]struct{}{}
	for _, topology := range requirement.GetRequisite() {
		if node := topology.GetSegments()[topologyKeyNode]; node != "" {
			requisiteNodes[node] = struct{}{}
		}
	}

	for _, topology := range requirement.GetPreferred() {
		node := topology.GetSegments()[topologyKeyNode]
		if node == "" {
			continue
		}
		_, allowed := requisiteNodes[node]
		if len(requisiteNodes) > 0 && !allowed {
			return "", fmt.Errorf("preferred node %s is not present in requisite topologies", node)
		}
		return node, nil
	}

	if len(requisiteNodes) == 0 {
		return "", fmt.Errorf("accessibility requirements do not contain %s", topologyKeyNode)
	}
	if len(requisiteNodes) > 1 {
		return "", fmt.Errorf("multiple requisite nodes are unsupported without a preferred node")
	}
	for node := range requisiteNodes {
		return node, nil
	}
	return "", fmt.Errorf("unable to select a topology node")
}

func topologyFromAccessibility(requirement *csi.TopologyRequirement) (string, []*csi.Topology, error) {
	node, err := nodeFromAccessibility(requirement)
	if err != nil {
		return "", nil, err
	}
	return node, []*csi.Topology{{
		Segments: map[string]string{topologyKeyNode: node},
	}}, nil
}

func metadataFromPV(volume *v1.PersistentVolume) (string, string, string, error) {
	if volume == nil {
		return "", "", "", fmt.Errorf("persistent volume is nil")
	}

	vgName, lvmType, err := lvmAttributesFromPV(volume.Name, volume.Spec.CSI)
	if err != nil {
		return "", "", "", err
	}
	nodeName, err := nodeFromPVAffinity(volume.Name, volume.Spec.NodeAffinity)
	if err != nil {
		return "", "", "", err
	}
	return nodeName, vgName, lvmType, nil
}

func lvmAttributesFromPV(name string, source *v1.CSIPersistentVolumeSource) (string, string, error) {
	if source == nil {
		return "", "", fmt.Errorf("persistent volume %s has no CSI source", name)
	}

	vgName := source.VolumeAttributes["vgName"]
	if vgName == "" {
		return "", "", fmt.Errorf("persistent volume %s has no vgName attribute", name)
	}
	lvmType := source.VolumeAttributes["type"]
	if lvmType != StripedType && lvmType != DmThinType {
		return "", "", fmt.Errorf("persistent volume %s has invalid LVM type %q", name, lvmType)
	}
	return vgName, lvmType, nil
}

func nodeFromPVAffinity(name string, affinity *v1.VolumeNodeAffinity) (string, error) {
	if affinity == nil || affinity.Required == nil {
		return "", fmt.Errorf("persistent volume %s has no required node affinity", name)
	}
	nodes := map[string]struct{}{}
	for _, expression := range matchExpressionsFromTerms(affinity.Required.NodeSelectorTerms) {
		if expression.Key != topologyKeyNode {
			continue
		}
		if expression.Operator != v1.NodeSelectorOpIn {
			return "", fmt.Errorf(
				"persistent volume %s topology expression must use operator In",
				name,
			)
		}
		for _, node := range expression.Values {
			if node == "" {
				continue
			}
			nodes[node] = struct{}{}
		}
	}
	if len(nodes) != 1 {
		return "", fmt.Errorf(
			"persistent volume %s must reference exactly one %s node, found %d",
			name,
			topologyKeyNode,
			len(nodes),
		)
	}

	for node := range nodes {
		return node, nil
	}
	return "", fmt.Errorf("unable to select node for persistent volume %s", name)
}

func matchExpressionsFromTerms(terms []v1.NodeSelectorTerm) []v1.NodeSelectorRequirement {
	expressions := make([]v1.NodeSelectorRequirement, 0)
	for _, term := range terms {
		expressions = append(expressions, term.MatchExpressions...)
	}
	return expressions
}

func requiredBytesFromPersistentVolume(volume *v1.PersistentVolume) (int64, error) {
	if volume == nil {
		return 0, fmt.Errorf("persistent volume is nil")
	}
	if volume.Spec.CSI == nil {
		return 0, fmt.Errorf("persistent volume %s has no CSI source", volume.Name)
	}
	value := volume.Spec.CSI.VolumeAttributes["RequiredBytes"]
	size, err := strconv.ParseInt(value, 10, 64)
	if err != nil || size <= 0 {
		return 0, fmt.Errorf("persistent volume %s has invalid RequiredBytes attribute %q", volume.Name, value)
	}
	return size, nil
}

func metadataFromSnapshotContent(content *snapv1.VolumeSnapshotContent) (string, string, int64, error) {
	if content == nil {
		return "", "", 0, fmt.Errorf("snapshot content is nil")
	}
	if content.Spec.Source.VolumeHandle == nil || *content.Spec.Source.VolumeHandle == "" {
		return "", "", 0, fmt.Errorf("snapshot content %s has no source volume handle", content.Name)
	}
	if content.Status == nil ||
		content.Status.RestoreSize == nil ||
		content.Status.SnapshotHandle == nil ||
		*content.Status.SnapshotHandle == "" ||
		*content.Status.RestoreSize <= 0 {
		return "", "", 0, fmt.Errorf("snapshot content %s is not ready to restore", content.Name)
	}
	return *content.Spec.Source.VolumeHandle,
		*content.Status.SnapshotHandle,
		*content.Status.RestoreSize,
		nil
}

func preProvisionedSnapshotMetadata(content *snapv1.VolumeSnapshotContent) (string, int64, error) {
	if content == nil {
		return "", 0, fmt.Errorf("snapshot content is nil")
	}

	statusHandle := ""
	if content.Status != nil && content.Status.SnapshotHandle != nil {
		statusHandle = *content.Status.SnapshotHandle
	}
	sourceHandle := ""
	if content.Spec.Source.SnapshotHandle != nil {
		sourceHandle = *content.Spec.Source.SnapshotHandle
	}
	if statusHandle != "" && sourceHandle != "" && statusHandle != sourceHandle {
		return "", 0, fmt.Errorf(
			"pre-provisioned snapshot content %s has conflicting snapshot handles %q and %q",
			content.Name,
			statusHandle,
			sourceHandle,
		)
	}

	snapshotID := statusHandle
	if snapshotID == "" {
		snapshotID = sourceHandle
	}
	if snapshotID == "" {
		return "", 0, fmt.Errorf("pre-provisioned snapshot content %s has no snapshot handle", content.Name)
	}

	if content.Status == nil || content.Status.RestoreSize == nil {
		return snapshotID, 0, nil
	}
	// The snapshot controller may publish zero when the size of a
	// pre-provisioned snapshot is unknown. Let the restore path fall back to
	// the destination PVC size in that case.
	if *content.Status.RestoreSize < 0 {
		return "", 0, fmt.Errorf("pre-provisioned snapshot content %s has invalid restore size %d", content.Name, *content.Status.RestoreSize)
	}
	return snapshotID, *content.Status.RestoreSize, nil
}
