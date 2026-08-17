/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package lvm

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	snapclient "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	v1 "k8s.io/api/core/v1"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/harvester/csi-driver-lvm/pkg/utils"
)

type controllerServer struct {
	caps             []*csi.ControllerServiceCapability
	nodeID           string
	hostWritePath    string
	kubeClient       kubernetes.Interface
	provisionerImage string
	pullPolicy       v1.PullPolicy
	namespace        string
	snapClient       snapclient.Interface
}

const (
	snapshotNodeAnnotation = "lvm.driver.harvesterhci.io/nodeName"
	snapshotVGAnnotation   = "lvm.driver.harvesterhci.io/vgName"

	// snapshotLocationConfigMap is a ConfigMap in the driver namespace that maps a
	// CSI snapshot handle to the "node/vg" that physically holds its LVM snapshot.
	// The driver records this at CreateSnapshot (when it already knows node/VG) so
	// that DeleteSnapshot can target the one correct node for a handle-only
	// (data-mover) content, instead of demanding annotations or guessing.
	snapshotLocationConfigMap = "csi-driver-lvm-snapshot-locations"
	// snapshotLocationSeparator joins node and VG in a ConfigMap value. Neither
	// Kubernetes node names (RFC 1123) nor LVM VG names permit "/", so it is an
	// unambiguous delimiter.
	snapshotLocationSeparator = "/"
)

// NewControllerServer
func newControllerServer(nodeID string, hostWritePath string, namespace string, provisionerImage string, pullPolicy v1.PullPolicy) (*controllerServer, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	// creates the clientset
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	snapClient, err := snapclient.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &controllerServer{
		caps: getControllerServiceCapabilities(
			[]csi.ControllerServiceCapability_RPC_Type{
				csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
				csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
				csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
				// TODO
				//				csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
				//				csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
				//				csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
			}),
		nodeID:           nodeID,
		hostWritePath:    hostWritePath,
		kubeClient:       kubeClient,
		namespace:        namespace,
		provisionerImage: provisionerImage,
		pullPolicy:       pullPolicy,
		snapClient:       snapClient,
	}, nil
}

func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if err := cs.validateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		klog.V(3).Infof("invalid create volume req: %v", req)
		return nil, err
	}

	// Check arguments
	if len(req.GetName()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Name missing in request")
	}
	if err := validateVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	lvmType, vgName, err := parseLVMParameters(req.GetParameters())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	requiredBytes, err := validateCapacityRange(req.GetCapacityRange())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	volumeContext := buildVolumeContext(req.GetParameters(), requiredBytes)

	node, topology, err := topologyFromAccessibility(req.GetAccessibilityRequirements())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	klog.Infof("creating volume %s on node: %s", req.GetName(), node)

	if err := cs.provisionVolume(ctx, req, node, lvmType, vgName, requiredBytes); err != nil {
		return nil, err
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:           req.GetName(),
			CapacityBytes:      requiredBytes,
			VolumeContext:      volumeContext,
			ContentSource:      req.GetVolumeContentSource(),
			AccessibleTopology: topology,
		},
	}, nil
}

func (cs *controllerServer) provisionVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest,
	node, lvmType, vgName string,
	requiredBytes int64,
) error {
	if source := req.GetVolumeContentSource(); source != nil {
		klog.Infof("cloning volume with source: %v", source)
		return cs.cloneFromContentSource(ctx, source, req.GetName(), node, lvmType, vgName, requiredBytes)
	}

	action := cs.newCreateVolumeAction(req.GetName(), node, lvmType, vgName, requiredBytes)
	if err := createProvisionerPod(ctx, action); err != nil {
		klog.Errorf("error creating provisioner pod: %v", err)
		return err
	}
	return nil
}

func (cs *controllerServer) cloneFromContentSource(
	ctx context.Context,
	source *csi.VolumeContentSource,
	dstName, dstNode, dstLVMType, dstVGName string,
	dstSize int64,
) error {
	if source == nil {
		return status.Error(codes.InvalidArgument, "volume content source is nil")
	}
	switch source.Type.(type) {
	case *csi.VolumeContentSource_Snapshot:
		return cs.cloneFromSnapshotSource(ctx, source.GetSnapshot(), dstName, dstNode, dstLVMType, dstVGName, dstSize)
	case *csi.VolumeContentSource_Volume:
		return cs.cloneFromVolumeSource(ctx, source.GetVolume(), dstName, dstNode, dstLVMType, dstVGName, dstSize)
	default:
		return status.Errorf(codes.InvalidArgument, "%v not a proper volume source", source)
	}
}

func (cs *controllerServer) cloneFromSnapshotSource(
	ctx context.Context,
	source *csi.VolumeContentSource_SnapshotSource,
	dstName, dstNode, dstLVMType, dstVGName string,
	dstSize int64,
) error {
	snapshotID := source.GetSnapshotId()
	if snapshotID == "" {
		return status.Error(codes.InvalidArgument, "source snapshot ID is empty")
	}

	// Snapshot handles are not VolumeSnapshotContent object names. In particular,
	// data movers create pre-provisioned contents with their own object names.
	content, err := cs.getSnapshotContent(ctx, snapshotID)
	if err != nil {
		return err
	}
	if content == nil {
		return status.Errorf(codes.NotFound, "source snapshot %s not found", snapshotID)
	}
	return cs.cloneFromSnapshot(ctx, content, dstName, dstNode, dstLVMType, dstVGName, dstSize)
}

func (cs *controllerServer) cloneFromVolumeSource(
	ctx context.Context,
	source *csi.VolumeContentSource_VolumeSource,
	dstName, dstNode, dstLVMType, dstVGName string,
	dstSize int64,
) error {
	volumeID := source.GetVolumeId()
	if volumeID == "" {
		return status.Error(codes.InvalidArgument, "source volume ID is empty")
	}

	volume, err := cs.kubeClient.CoreV1().PersistentVolumes().Get(ctx, volumeID, metav1.GetOptions{})
	if k8serror.IsNotFound(err) {
		return status.Errorf(codes.NotFound, "source volume %s not found", volumeID)
	}
	if err != nil {
		return status.Errorf(codes.Unavailable, "failed to get source volume %s: %v", volumeID, err)
	}
	return cs.cloneFromVolume(ctx, volume, dstName, dstNode, dstLVMType, dstVGName, dstSize)
}

func (cs *controllerServer) newCreateVolumeAction(name, node, lvmType, vgName string, size int64) volumeAction {
	return volumeAction{
		action:           actionTypeCreate,
		name:             name,
		nodeName:         node,
		size:             size,
		lvmType:          lvmType,
		pullPolicy:       cs.pullPolicy,
		provisionerImage: cs.provisionerImage,
		kubeClient:       cs.kubeClient,
		namespace:        cs.namespace,
		vgName:           vgName,
		hostWritePath:    cs.hostWritePath,
	}
}

func (cs *controllerServer) generateVolumeActionForClone(
	srcVol *v1.PersistentVolume,
	srcLVName, dstName, dstNode, dstLVType, dstVGName string,
	srcSize, dstSize int64,
) (volumeAction, error) {
	srcNode, srcVGName, srcLVMType, err := metadataFromPV(srcVol)
	if err != nil {
		return volumeAction{}, status.Error(codes.FailedPrecondition, err.Error())
	}

	srcInfo := &srcInfo{
		srcLVName: srcLVName,
		srcVGName: srcVGName,
		srcType:   srcLVMType,
	}
	return cs.newCloneVolumeAction(srcInfo, srcNode, dstName, dstNode, dstLVType, dstVGName, srcSize, dstSize)
}

func (cs *controllerServer) newCloneVolumeAction(
	source *srcInfo,
	srcNode, dstName, dstNode, dstLVType, dstVGName string,
	srcSize, dstSize int64,
) (volumeAction, error) {
	if source == nil {
		return volumeAction{}, status.Error(codes.FailedPrecondition, "clone source is nil")
	}
	klog.V(4).Infof("cloning volume from %s/%s ", source.srcVGName, source.srcLVName)

	if srcSize > dstSize {
		return volumeAction{}, status.Errorf(codes.InvalidArgument,
			"source/snapshot volume size(%v) is larger than destination volume size(%v)", srcSize, dstSize)
	}
	if srcNode != dstNode {
		return volumeAction{}, status.Errorf(codes.InvalidArgument,
			"source (%s) and destination (%s) nodes are different (not supported)", srcNode, dstNode)
	}

	return volumeAction{
		action:           actionTypeClone,
		name:             dstName,
		nodeName:         dstNode,
		size:             dstSize,
		lvmType:          dstLVType,
		pullPolicy:       cs.pullPolicy,
		provisionerImage: cs.provisionerImage,
		kubeClient:       cs.kubeClient,
		namespace:        cs.namespace,
		vgName:           dstVGName,
		hostWritePath:    cs.hostWritePath,
		srcInfo:          source,
	}, nil
}

func (cs *controllerServer) cloneFromSnapshot(
	ctx context.Context,
	snapContent *snapv1.VolumeSnapshotContent,
	dstName, dstNode, dstLVType, dstVGName string,
	dstSize int64,
) error {
	if snapContent == nil {
		return status.Error(codes.FailedPrecondition, "snapshot content is nil")
	}
	if snapContent.Spec.Source.VolumeHandle == nil {
		return cs.cloneFromPreProvisionedSnapshot(ctx, snapContent, dstName, dstNode, dstLVType, dstVGName, dstSize)
	}
	return cs.cloneFromDynamicSnapshot(ctx, snapContent, dstName, dstNode, dstLVType, dstVGName, dstSize)
}

func (cs *controllerServer) cloneFromDynamicSnapshot(
	ctx context.Context,
	snapContent *snapv1.VolumeSnapshotContent,
	dstName, dstNode, dstLVType, dstVGName string,
	dstSize int64,
) error {
	sourceVolumeID, snapshotID, restoreSize, err := metadataFromSnapshotContent(snapContent)
	if err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}

	srcVol, err := cs.kubeClient.CoreV1().PersistentVolumes().Get(ctx, sourceVolumeID, metav1.GetOptions{})
	if k8serror.IsNotFound(err) {
		return status.Errorf(codes.NotFound, "source volume %s not found", sourceVolumeID)
	}
	if err != nil {
		return status.Errorf(codes.Unavailable, "failed to get source volume %s: %v", sourceVolumeID, err)
	}

	snapshotLVName := fmt.Sprintf("lvm-%s", snapshotID)
	va, err := cs.generateVolumeActionForClone(srcVol, snapshotLVName, dstName, dstNode, dstLVType, dstVGName, restoreSize, dstSize)
	if err != nil {
		return err
	}

	if err := createProvisionerPod(ctx, va); err != nil {
		klog.Errorf("error creating provisioner pod :%v", err)
		return err
	}

	return nil
}

func (cs *controllerServer) cloneFromPreProvisionedSnapshot(
	ctx context.Context,
	snapContent *snapv1.VolumeSnapshotContent,
	dstName, dstNode, dstLVType, dstVGName string,
	dstSize int64,
) error {
	action, err := cs.preProvisionedSnapshotCloneAction(
		snapContent,
		dstName,
		dstNode,
		dstLVType,
		dstVGName,
		dstSize,
	)
	if err != nil {
		return err
	}
	if err := createProvisionerPod(ctx, action); err != nil {
		klog.Errorf("error creating provisioner pod: %v", err)
		return err
	}
	return nil
}

func (cs *controllerServer) preProvisionedSnapshotCloneAction(
	snapContent *snapv1.VolumeSnapshotContent,
	dstName, dstNode, dstLVType, dstVGName string,
	dstSize int64,
) (volumeAction, error) {
	snapshotID, restoreSize, err := preProvisionedSnapshotMetadata(snapContent)
	if err != nil {
		return volumeAction{}, status.Error(codes.FailedPrecondition, err.Error())
	}

	srcNode := snapContent.Annotations[snapshotNodeAnnotation]
	if srcNode == "" {
		srcNode = dstNode
	}
	srcVGName := snapContent.Annotations[snapshotVGAnnotation]
	if srcVGName == "" {
		srcVGName = dstVGName
	}
	if restoreSize == 0 {
		restoreSize = dstSize
	}

	source := &srcInfo{
		srcLVName: fmt.Sprintf("lvm-%s", snapshotID),
		srcVGName: srcVGName,
		// Pre-provisioned contents do not carry the source LVM type. Restores
		// use the destination StorageClass type, which is also what determines
		// whether the optimized same-VG dm-thin clone path can be used.
		srcType: dstLVType,
	}
	return cs.newCloneVolumeAction(source, srcNode, dstName, dstNode, dstLVType, dstVGName, restoreSize, dstSize)
}

func (cs *controllerServer) cloneFromVolume(
	ctx context.Context,
	srcVol *v1.PersistentVolume,
	dstName, dstNode, dstLVType, dstVGName string,
	dstSize int64,
) error {
	srcSize, err := requiredBytesFromPersistentVolume(srcVol)
	if err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	srcLVName := srcVol.GetName()
	va, err := cs.generateVolumeActionForClone(srcVol, srcLVName, dstName, dstNode, dstLVType, dstVGName, srcSize, dstSize)
	if err != nil {
		return err
	}

	if err := createProvisionerPod(ctx, va); err != nil {
		klog.Errorf("error creating provisioner pod :%v", err)
		return err
	}

	return nil
}

func (cs *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if err := validateDeleteVolumeRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := cs.validateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		klog.V(3).Infof("invalid delete volume req: %v", req)
		return nil, err
	}

	volID := req.GetVolumeId()

	volume, err := cs.persistentVolumeForDeletion(ctx, volID)
	if err != nil {
		return nil, err
	}
	if volume == nil {
		return &csi.DeleteVolumeResponse{}, nil
	}

	klog.V(4).Infof("volume %s to be deleted", volume)
	nodeName, vgName, lvmType, err := metadataFromPV(volume)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	klog.V(4).Infof("from node %s ", nodeName)
	nodeAvailable, err := cs.nodeAvailableForDeletion(ctx, nodeName, volID)
	if err != nil {
		return nil, err
	}
	if !nodeAvailable {
		return &csi.DeleteVolumeResponse{}, nil
	}

	va := cs.newDeleteVolumeAction(volID, nodeName, vgName, lvmType)
	if err := createProvisionerPod(ctx, va); err != nil {
		klog.Errorf("error creating provisioner pod :%v", err)
		return nil, err
	}

	klog.V(4).Infof("volume %v successfully deleted", volID)
	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) persistentVolumeForDeletion(ctx context.Context, volumeID string) (*v1.PersistentVolume, error) {
	volume, err := cs.kubeClient.CoreV1().PersistentVolumes().Get(ctx, volumeID, metav1.GetOptions{})
	if k8serror.IsNotFound(err) {
		klog.Infof("volume %s is already absent", volumeID)
		return nil, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "failed to get volume %s: %v", volumeID, err)
	}
	return volume, nil
}

func (cs *controllerServer) nodeAvailableForDeletion(ctx context.Context, nodeName, volumeID string) (bool, error) {
	_, err := cs.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if k8serror.IsNotFound(err) {
		klog.Infof("node %s not found anymore. Assuming volume %s is gone for good.", nodeName, volumeID)
		return false, nil
	}
	if err != nil {
		klog.Errorf("error getting nodes: %v", err)
		return false, status.Errorf(codes.Unavailable, "failed to get node %s: %v", nodeName, err)
	}
	return true, nil
}

func (cs *controllerServer) newDeleteVolumeAction(volumeID, nodeName, vgName, lvmType string) volumeAction {
	return volumeAction{
		action:           actionTypeDelete,
		name:             volumeID,
		nodeName:         nodeName,
		pullPolicy:       cs.pullPolicy,
		provisionerImage: cs.provisionerImage,
		kubeClient:       cs.kubeClient,
		namespace:        cs.namespace,
		hostWritePath:    cs.hostWritePath,
		srcInfo: &srcInfo{
			srcLVName: volumeID,
			srcVGName: vgName,
			srcType:   lvmType,
		},
	}
}

func (cs *controllerServer) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: cs.caps,
	}, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID cannot be empty")
	}
	if len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, req.VolumeId)
	}

	if err := validateVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{Message: err.Error()}, nil
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeContext:      req.GetVolumeContext(),
			VolumeCapabilities: req.GetVolumeCapabilities(),
			Parameters:         req.GetParameters(),
		},
	}, nil
}

func (cs *controllerServer) validateControllerServiceRequest(c csi.ControllerServiceCapability_RPC_Type) error {
	if c == csi.ControllerServiceCapability_RPC_UNKNOWN {
		return nil
	}

	for _, cap := range cs.caps {
		if c == cap.GetRpc().GetType() {
			return nil
		}
	}
	return status.Errorf(codes.InvalidArgument, "unsupported capability %s", c)
}

func getControllerServiceCapabilities(cl []csi.ControllerServiceCapability_RPC_Type) []*csi.ControllerServiceCapability {
	var csc = make([]*csi.ControllerServiceCapability, 0, len(cl))

	for _, cap := range cl {
		klog.Infof("Enabling controller service capability: %v", cap.String())
		csc = append(csc, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		})
	}

	return csc
}

// Following functions will never be implemented
// use the "NodeXXX" versions of the nodeserver instead

func (cs *controllerServer) ControllerPublishVolume(_ context.Context, _ *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *controllerServer) ControllerUnpublishVolume(_ context.Context, _ *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *controllerServer) GetCapacity(_ context.Context, _ *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *controllerServer) ListVolumes(_ context.Context, _ *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *controllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	klog.Infof("CreateSnapshot req: %v", req)
	snapshotName, volumeID, err := validateCreateSnapshotRequest(req)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	volume, err := cs.kubeClient.CoreV1().PersistentVolumes().Get(ctx, volumeID, metav1.GetOptions{})
	if k8serror.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "source volume %s not found", volumeID)
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "failed to get source volume %s: %v", volumeID, err)
	}

	klog.V(4).Infof("taking snapshot with volume %s ", volume)
	nodeName, vgName, lvmType, err := metadataFromPV(volume)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	snapSize, err := requiredBytesFromPersistentVolume(volume)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	// Record where this snapshot's LV lives before creating it. DeleteSnapshot
	// uses this to target the one correct node for a handle-only content (e.g. a
	// Velero data-mover Delete-policy content) without annotations or fan-out.
	// Recording first keeps the retry clean: if this fails no LV is created yet.
	if err := cs.recordSnapshotLocation(ctx, snapshotName, nodeName, vgName); err != nil {
		return nil, status.Errorf(codes.Unavailable, "failed to record location for snapshot %s: %v", snapshotName, err)
	}

	action := cs.newCreateSnapshotAction(snapshotName, volumeID, nodeName, vgName, lvmType, snapSize)
	if err := createSnapshotterPod(ctx, action); err != nil {
		klog.Errorf("error creating provisioner pod :%v", err)
		return nil, err
	}

	return newCreateSnapshotResponse(snapshotName, volumeID, snapSize), nil
}

func (cs *controllerServer) newCreateSnapshotAction(
	snapshotName, volumeID, nodeName, vgName, lvmType string,
	size int64,
) snapshotAction {
	return snapshotAction{
		action:           actionTypeCreate,
		srcVolName:       volumeID,
		snapshotName:     snapshotName,
		nodeName:         nodeName,
		snapSize:         size,
		vgName:           vgName,
		lvType:           lvmType,
		hostWritePath:    cs.hostWritePath,
		kubeClient:       cs.kubeClient,
		namespace:        cs.namespace,
		provisionerImage: cs.provisionerImage,
		pullPolicy:       cs.pullPolicy,
	}
}

func newCreateSnapshotResponse(snapshotName, volumeID string, size int64) *csi.CreateSnapshotResponse {
	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SnapshotId:     snapshotName,
			SourceVolumeId: volumeID,
			SizeBytes:      size,
			CreationTime: &timestamppb.Timestamp{
				Seconds: time.Now().Unix(),
			},
			ReadyToUse: true,
		},
	}
}

func (cs *controllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	klog.Infof("DeleteSnapshot req: %v", req)
	snapName := req.GetSnapshotId()
	if snapName == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot ID missing in request")
	}

	snapContent, err := cs.getSnapshotContent(ctx, snapName)
	if err != nil {
		return nil, err
	}
	if snapContent == nil {
		klog.Infof("snapshot %s is already absent", snapName)
		return &csi.DeleteSnapshotResponse{}, nil
	}

	actions, err := cs.deleteSnapshotAction(ctx, snapName, snapContent)
	if err != nil {
		return nil, err
	}

	for i := range actions {
		if err := createSnapshotterPod(ctx, actions[i]); err != nil {
			klog.Errorf("error creating provisioner pod :%v", err)
			return nil, err
		}
	}

	// The snapshot is gone (deleted here, or unresolvable and treated as absent);
	// drop its location record so the ConfigMap does not accumulate stale entries.
	cs.forgetSnapshotLocation(ctx, snapName)

	return &csi.DeleteSnapshotResponse{}, nil
}

func (cs *controllerServer) getSnapshotContent(ctx context.Context, snapshotID string) (*snapv1.VolumeSnapshotContent, error) {
	contents, err := cs.snapClient.SnapshotV1().VolumeSnapshotContents().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "failed to list snapshot contents: %v", err)
	}

	for i := range contents.Items {
		content := &contents.Items[i]
		if content.Status != nil &&
			content.Status.SnapshotHandle != nil &&
			*content.Status.SnapshotHandle == snapshotID {
			return content, nil
		}
		if content.Spec.Source.SnapshotHandle != nil &&
			*content.Spec.Source.SnapshotHandle == snapshotID {
			return content, nil
		}
	}
	return nil, nil
}

func (cs *controllerServer) deleteSnapshotAction(
	ctx context.Context,
	snapshotID string,
	content *snapv1.VolumeSnapshotContent,
) ([]snapshotAction, error) {
	if content == nil {
		return nil, status.Error(codes.FailedPrecondition, "snapshot content is nil")
	}
	if content.Spec.Source.VolumeHandle == nil {
		return cs.deletePreExistingSnapshotAction(ctx, snapshotID, content)
	}
	return cs.deleteDynamicSnapshotAction(ctx, snapshotID, *content.Spec.Source.VolumeHandle)
}

// deletePreExistingSnapshotAction builds the delete action for a
// statically-referenced VolumeSnapshotContent — one whose source is only a
// snapshotHandle (spec.source.volumeHandle is nil). This is not just the
// hand-authored Retain import case: backup data-movers (e.g. Velero's CSI
// data-mover) routinely produce Delete-policy contents of exactly this shape on
// every backup cycle, and they never carry the driver-internal node/VG
// annotations. Requiring those annotations here wedged their deletion forever
// (harvester/harvester#11128).
//
// Rather than guess, the driver resolves the one node/VG that actually holds the
// snapshot. Resolution order:
//  1. the nodeName/vgName annotations, when present, as an explicit override
//     (this also covers hand-authored pre-provisioned imports);
//  2. otherwise the location the driver recorded for this handle at
//     CreateSnapshot (see recordSnapshotLocation) — an exact, single target.
//
// If neither is available the snapshot cannot be located, so we return no action
// (success) rather than a retryable FailedPrecondition — a single un-annotated
// content must never permanently jam snapshot reconciliation for the cluster.
func (cs *controllerServer) deletePreExistingSnapshotAction(
	ctx context.Context,
	snapshotID string,
	content *snapv1.VolumeSnapshotContent,
) ([]snapshotAction, error) {
	if content.Spec.Source.SnapshotHandle == nil || *content.Spec.Source.SnapshotHandle == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "snapshot content %s has no source handle", content.Name)
	}

	// 1. Explicit override via annotations, if the client supplied them.
	nodeName := content.Annotations[snapshotNodeAnnotation]
	vgName := content.Annotations[snapshotVGAnnotation]
	if nodeName != "" && vgName != "" {
		action := cs.newDeleteSnapshotAction(snapshotID, nodeName, vgName)
		return []snapshotAction{action}, nil
	}

	// 2. Location recorded by the driver at CreateSnapshot.
	nodeName, vgName, found, err := cs.lookupSnapshotLocation(ctx, snapshotID)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "failed to look up recorded location for snapshot %s: %v", snapshotID, err)
	}
	if found {
		klog.Infof("pre-existing snapshot %s resolved to node %q / VG %q from recorded location", snapshotID, nodeName, vgName)
		action := cs.newDeleteSnapshotAction(snapshotID, nodeName, vgName)
		return []snapshotAction{action}, nil
	}

	// 3. Neither annotations nor a recorded location: cannot locate the snapshot.
	// This is either an orphan or something created outside this driver, so log
	// actionable cleanup guidance and treat the delete as already-satisfied.
	logUnmanagedSnapshot(snapshotID)
	return nil, nil
}

// logUnmanagedSnapshot emits an actionable warning when the driver is asked to
// delete a snapshot it has no record of and that carries no management
// annotations — i.e. an orphan, or something created outside this driver. It
// prints the deterministic on-node LV name and a guarded manual-cleanup command
// so an operator can reclaim the space after confirming the LV is truly unused.
func logUnmanagedSnapshot(snapshotID string) {
	lvName := fmt.Sprintf("lvm-%s", snapshotID)
	klog.Warningf(
		"unknown snapshot %s is not managed by %s: it has no %s/%s annotations and no location was recorded at "+
			"CreateSnapshot, so it is either an orphan or was created outside this driver. Treating DeleteSnapshot as "+
			"already-satisfied to avoid blocking snapshot reconciliation. If a stale LV named %q still exists on a node, "+
			"locate it with `lvs -a -o lv_name,vg_name,lv_size -S lv_name=%s` and, ONLY after confirming it is not in use "+
			"by any volume, clone, or backup, remove it with `lvremove -y /dev/<vgName>/%s`.",
		snapshotID, utils.LVMCSIDriver, snapshotNodeAnnotation, snapshotVGAnnotation, lvName, lvName, lvName,
	)
}

func (cs *controllerServer) deleteDynamicSnapshotAction(
	ctx context.Context,
	snapshotID, volumeID string,
) ([]snapshotAction, error) {
	if volumeID == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "snapshot %s has an empty source volume handle", snapshotID)
	}
	volume, err := cs.kubeClient.CoreV1().PersistentVolumes().Get(ctx, volumeID, metav1.GetOptions{})
	if k8serror.IsNotFound(err) {
		klog.Warningf(
			"source volume %s for snapshot %s is already absent; returning success to preserve delete idempotency",
			volumeID,
			snapshotID,
		)
		return nil, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "failed to get source volume %s: %v", volumeID, err)
	}
	nodeName, vgName, _, err := metadataFromPV(volume)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	action := cs.newDeleteSnapshotAction(snapshotID, nodeName, vgName)
	return []snapshotAction{action}, nil
}

// recordSnapshotLocation stores snapshotID -> "node/vg" in the location
// ConfigMap so DeleteSnapshot can later target the exact node holding the LV.
// Entries for distinct snapshots are written with a JSON merge patch, so
// concurrent CreateSnapshot calls do not clobber each other's keys. The
// ConfigMap is created lazily on first use.
func (cs *controllerServer) recordSnapshotLocation(ctx context.Context, snapshotID, nodeName, vgName string) error {
	value := nodeName + snapshotLocationSeparator + vgName
	patch, err := json.Marshal(map[string]map[string]string{"data": {snapshotID: value}})
	if err != nil {
		return err
	}

	cms := cs.kubeClient.CoreV1().ConfigMaps(cs.namespace)
	_, err = cms.Patch(ctx, snapshotLocationConfigMap, types.MergePatchType, patch, metav1.PatchOptions{})
	if err == nil {
		return nil
	}
	if !k8serror.IsNotFound(err) {
		return err
	}

	// ConfigMap does not exist yet; create it seeded with this entry.
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: snapshotLocationConfigMap, Namespace: cs.namespace},
		Data:       map[string]string{snapshotID: value},
	}
	_, err = cms.Create(ctx, cm, metav1.CreateOptions{})
	if k8serror.IsAlreadyExists(err) {
		// Lost the create race with a concurrent CreateSnapshot; patch instead.
		_, err = cms.Patch(ctx, snapshotLocationConfigMap, types.MergePatchType, patch, metav1.PatchOptions{})
	}
	return err
}

// lookupSnapshotLocation returns the (node, VG) recorded for snapshotID at
// CreateSnapshot. found is false when there is no entry (including when the
// ConfigMap does not exist yet).
func (cs *controllerServer) lookupSnapshotLocation(ctx context.Context, snapshotID string) (nodeName, vgName string, found bool, err error) {
	cm, err := cs.kubeClient.CoreV1().ConfigMaps(cs.namespace).Get(ctx, snapshotLocationConfigMap, metav1.GetOptions{})
	if k8serror.IsNotFound(err) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	value, ok := cm.Data[snapshotID]
	if !ok {
		return "", "", false, nil
	}
	parts := strings.SplitN(value, snapshotLocationSeparator, 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false, fmt.Errorf("malformed location %q for snapshot %s", value, snapshotID)
	}
	return parts[0], parts[1], true, nil
}

// forgetSnapshotLocation removes snapshotID's entry from the location ConfigMap.
// It is best-effort: a leftover entry is harmless (it is overwritten if the
// handle is ever reused and ignored otherwise), so failures are only logged.
func (cs *controllerServer) forgetSnapshotLocation(ctx context.Context, snapshotID string) {
	patch, err := json.Marshal(map[string]map[string]interface{}{"data": {snapshotID: nil}})
	if err != nil {
		klog.Warningf("failed to build location cleanup patch for snapshot %s: %v", snapshotID, err)
		return
	}
	_, err = cs.kubeClient.CoreV1().ConfigMaps(cs.namespace).Patch(ctx, snapshotLocationConfigMap, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil && !k8serror.IsNotFound(err) {
		klog.Warningf("failed to remove location entry for snapshot %s: %v", snapshotID, err)
	}
}

func (cs *controllerServer) newDeleteSnapshotAction(snapshotID, nodeName, vgName string) snapshotAction {
	return snapshotAction{
		action:           actionTypeDelete,
		snapshotName:     snapshotID,
		nodeName:         nodeName,
		vgName:           vgName,
		hostWritePath:    cs.hostWritePath,
		kubeClient:       cs.kubeClient,
		namespace:        cs.namespace,
		provisionerImage: cs.provisionerImage,
		pullPolicy:       cs.pullPolicy,
	}
}

func (cs *controllerServer) ListSnapshots(_ context.Context, _ *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *controllerServer) ControllerExpandVolume(_ context.Context, _ *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *controllerServer) ControllerGetVolume(_ context.Context, _ *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *controllerServer) ControllerModifyVolume(_ context.Context, _ *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}
