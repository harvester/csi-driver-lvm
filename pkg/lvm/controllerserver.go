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
	"fmt"
	"strconv"
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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

type controllerServer struct {
	caps             []*csi.ControllerServiceCapability
	nodeID           string
	hostWritePath    string
	kubeClient       kubernetes.Clientset
	provisionerImage string
	pullPolicy       v1.PullPolicy
	namespace        string
	snapClient       *snapclient.Clientset
}

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
		kubeClient:       *kubeClient,
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
	caps := req.GetVolumeCapabilities()
	if caps == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities missing in request")
	}

	// Keep a record of the requested access types.
	var accessTypeMount, accessTypeBlock bool

	for _, cap := range caps {
		if cap.GetBlock() != nil {
			accessTypeBlock = true
		}
		if cap.GetMount() != nil {
			accessTypeMount = true
		}
	}

	if accessTypeBlock && accessTypeMount {
		return nil, status.Error(codes.InvalidArgument, "cannot have both block and mount access type")
	}

	lvmType := req.GetParameters()["type"]
	if !(lvmType == "striped" || lvmType == "dm-thin") {
		return nil, status.Errorf(codes.Internal, "lvmType is incorrect: %s", lvmType)
	}

	vgName := req.GetParameters()["vgName"]
	if vgName == "" {
		return nil, status.Error(codes.InvalidArgument, "vgName is missing, please check the storage class")
	}

	volumeContext := req.GetParameters()
	size := strconv.FormatInt(req.GetCapacityRange().GetRequiredBytes(), 10)

	volumeContext["RequiredBytes"] = size

	// schedulded node of the pod is the first entry in the preferred segment
	node := req.GetAccessibilityRequirements().GetPreferred()[0].GetSegments()[topologyKeyNode]
	topology := []*csi.Topology{{
		Segments: map[string]string{topologyKeyNode: node},
	}}
	klog.Infof("creating volume %s on node: %s", req.GetName(), node)

	if req.GetVolumeContentSource() != nil {
		klog.Infof("cloning volume with source: %v", req.GetVolumeContentSource())
		volumeSource := req.VolumeContentSource
		switch volumeSource.Type.(type) {
		case *csi.VolumeContentSource_Snapshot:
			srcSnapID := volumeSource.GetSnapshot().GetSnapshotId()
			snapContentName := convertSnapContentName(srcSnapID)
			snapContent, err := cs.snapClient.SnapshotV1().VolumeSnapshotContents().Get(ctx, snapContentName, metav1.GetOptions{})
			if err != nil {
				klog.Errorf("error getting snapshot content: %v", err)
				return nil, err
			}
			if err := cs.cloneFromSnapshot(ctx, snapContent, req.GetName(), node, lvmType, vgName, req.GetCapacityRange().GetRequiredBytes()); err != nil {
				return nil, err
			}
		case *csi.VolumeContentSource_Volume:
			srcVolID := volumeSource.GetVolume().GetVolumeId()
			srcVolume, err := cs.kubeClient.CoreV1().PersistentVolumes().Get(ctx, srcVolID, metav1.GetOptions{})
			if err != nil {
				return nil, status.Errorf(codes.Unavailable, "source volume %s not found", srcVolID)
			}
			if err := cs.cloneFromVolume(ctx, srcVolume, req.GetName(), node, lvmType, vgName, req.GetCapacityRange().GetRequiredBytes()); err != nil {
				return nil, err
			}
		default:
			return nil, status.Errorf(codes.InvalidArgument, "%v not a proper volume source", volumeSource)
		}
	} else {
		va := volumeAction{
			action:           actionTypeCreate,
			name:             req.GetName(),
			nodeName:         node,
			size:             req.GetCapacityRange().GetRequiredBytes(),
			lvmType:          lvmType,
			pullPolicy:       cs.pullPolicy,
			provisionerImage: cs.provisionerImage,
			kubeClient:       cs.kubeClient,
			namespace:        cs.namespace,
			vgName:           vgName,
			hostWritePath:    cs.hostWritePath,
		}
		if err := createProvisionerPod(ctx, va); err != nil {
			klog.Errorf("error creating provisioner pod :%v", err)
			return nil, err
		}
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:           req.GetName(),
			CapacityBytes:      req.GetCapacityRange().GetRequiredBytes(),
			VolumeContext:      volumeContext,
			ContentSource:      req.GetVolumeContentSource(),
			AccessibleTopology: topology,
		},
	}, nil
}

func (cs *controllerServer) generateVolumeActionForClone(srcVol *v1.PersistentVolume, srcLVName, dstName, dstNode, dstLVType, dstVGName string, srcSize, dstSize int64) (volumeAction, error) {
	ns := srcVol.Spec.NodeAffinity.Required.NodeSelectorTerms
	srcNode := ns[0].MatchExpressions[0].Values[0]
	srcVgName := srcVol.Spec.CSI.VolumeAttributes["vgName"]
	srcType := srcVol.Spec.CSI.VolumeAttributes["type"]

	srcInfo := &srcInfo{
		srcLVName: srcLVName,
		srcVGName: srcVgName,
		srcType:   srcType,
	}
	klog.V(4).Infof("cloning volume from %s/%s ", srcVgName, srcLVName)

	if srcSize > dstSize {
		return volumeAction{}, status.Errorf(codes.InvalidArgument, "source/snapshot volume size(%v) is larger than destination volume size(%v)", srcSize, dstSize)
	}
	if srcNode != dstNode {
		return volumeAction{}, status.Errorf(codes.InvalidArgument, "source (%s) and destination (%s) nodes are different (not supported)", srcNode, dstNode)
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
		srcInfo:          srcInfo,
	}, nil
}

func (cs *controllerServer) cloneFromSnapshot(ctx context.Context, snapContent *snapv1.VolumeSnapshotContent, dstName, dstNode, dstLVType, dstVGName string, dstSize int64) error {
	srcVolID := *snapContent.Spec.Source.VolumeHandle
	srcVol, err := cs.kubeClient.CoreV1().PersistentVolumes().Get(ctx, srcVolID, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("error getting volume: %v", err)
		return err
	}
	restoreSize := *snapContent.Status.RestoreSize
	snapshotLVName := fmt.Sprintf("lvm-%s", *snapContent.Status.SnapshotHandle)
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

func (cs *controllerServer) cloneFromVolume(ctx context.Context, srcVol *v1.PersistentVolume, dstName, dstNode, dstLVType, dstVGName string, dstSize int64) error {
	srcSizeStr := srcVol.Spec.CSI.VolumeAttributes["RequiredBytes"]
	srcSize, err := strconv.ParseInt(srcSizeStr, 10, 64)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "error parsing srcSize: %v", err)
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
	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if err := cs.validateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		klog.V(3).Infof("invalid delete volume req: %v", req)
		return nil, err
	}

	volID := req.GetVolumeId()

	volume, err := cs.kubeClient.CoreV1().PersistentVolumes().Get(ctx, volID, metav1.GetOptions{})
	if err != nil {
		panic(err.Error())
	}
	klog.V(4).Infof("volume %s to be deleted", volume)
	ns := volume.Spec.NodeAffinity.Required.NodeSelectorTerms
	node := ns[0].MatchExpressions[0].Values[0]
	srcVgName := volume.Spec.CSI.VolumeAttributes["vgName"]
	srcType := volume.Spec.CSI.VolumeAttributes["type"]
	srcInfo := &srcInfo{
		srcLVName: volID,
		srcVGName: srcVgName,
		srcType:   srcType,
	}

	klog.V(4).Infof("from node %s ", node)

	_, err = cs.kubeClient.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		if k8serror.IsNotFound(err) {
			klog.Infof("node %s not found anymore. Assuming volume %s is gone for good.", node, volID)
			return &csi.DeleteVolumeResponse{}, nil
		}
		klog.Errorf("error getting nodes: %v", err)
		return nil, err
	}

	va := volumeAction{
		action:           actionTypeDelete,
		name:             volID,
		nodeName:         node,
		pullPolicy:       cs.pullPolicy,
		provisionerImage: cs.provisionerImage,
		kubeClient:       cs.kubeClient,
		namespace:        cs.namespace,
		hostWritePath:    cs.hostWritePath,
		srcInfo:          srcInfo,
	}
	if err := createProvisionerPod(ctx, va); err != nil {
		klog.Errorf("error creating provisioner pod :%v", err)
		return nil, err
	}

	klog.V(4).Infof("volume %v successfully deleted", volID)

	return &csi.DeleteVolumeResponse{}, nil
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

	for _, cap := range req.GetVolumeCapabilities() {
		if cap.GetMount() == nil && cap.GetBlock() == nil {
			return nil, status.Error(codes.InvalidArgument, "cannot have both mount and block access type be undefined")
		}

		// A real driver would check the capabilities of the given volume with
		// the set of requested capabilities.
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
	var csc = make([]*csi.ControllerServiceCapability, len(cl))

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
	volID := req.GetSourceVolumeId()
	volume, err := cs.kubeClient.CoreV1().PersistentVolumes().Get(ctx, volID, metav1.GetOptions{})
	if err != nil {
		panic(err.Error())
	}
	klog.V(4).Infof("taking snapshot with volume %s ", volume)
	ns := volume.Spec.NodeAffinity.Required.NodeSelectorTerms
	node := ns[0].MatchExpressions[0].Values[0]
	vgName := volume.Spec.CSI.VolumeAttributes["vgName"]
	lvType := volume.Spec.CSI.VolumeAttributes["type"]
	snapSizeStr := volume.Spec.CSI.VolumeAttributes["RequiredBytes"]
	snapSize, err := strconv.ParseInt(snapSizeStr, 10, 64)
	if err != nil {
		klog.Errorf("error parsing snapSize: %v", err)
		return nil, err
	}
	snapshotName := req.GetName()
	snapTimestamp := &timestamppb.Timestamp{
		Seconds: time.Now().Unix(),
	}

	sa := snapshotAction{
		action:           actionTypeCreate,
		srcVolName:       volID,
		snapshotName:     snapshotName,
		nodeName:         node,
		snapSize:         snapSize,
		vgName:           vgName,
		lvType:           lvType,
		hostWritePath:    cs.hostWritePath,
		kubeClient:       cs.kubeClient,
		namespace:        cs.namespace,
		provisionerImage: cs.provisionerImage,
		pullPolicy:       cs.pullPolicy,
	}
	if err := createSnapshotterPod(ctx, sa); err != nil {
		klog.Errorf("error creating provisioner pod :%v", err)
		return nil, err
	}

	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SnapshotId:     snapshotName,
			SourceVolumeId: volID,
			SizeBytes:      snapSize,
			CreationTime:   snapTimestamp,
			ReadyToUse:     true,
		},
	}, nil
}

func (cs *controllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	klog.Infof("DeleteSnapshot req: %v", req)
	snapName := req.GetSnapshotId()
	snapshotsList, err := cs.snapClient.SnapshotV1().VolumeSnapshotContents().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("error listing snapshots: %v", err)
		return nil, err
	}
	volID := ""
	for _, snap := range snapshotsList.Items {
		if *snap.Status.SnapshotHandle == snapName {
			volID = *snap.Spec.Source.VolumeHandle
		}
	}
	if volID == "" {
		klog.Errorf("snapshot %s not found", snapName)
		return nil, status.Error(codes.NotFound, "snapshot not found")
	}
	volume, err := cs.kubeClient.CoreV1().PersistentVolumes().Get(ctx, volID, metav1.GetOptions{})
	if err != nil {
		panic(err.Error())
	}
	klog.V(4).Infof("deleting snapshot with volume %s ", snapName)
	ns := volume.Spec.NodeAffinity.Required.NodeSelectorTerms
	node := ns[0].MatchExpressions[0].Values[0]
	vgName := volume.Spec.CSI.VolumeAttributes["vgName"]

	sa := snapshotAction{
		action:           actionTypeDelete,
		snapshotName:     snapName,
		nodeName:         node,
		vgName:           vgName,
		lvType:           "", // not used
		hostWritePath:    cs.hostWritePath,
		kubeClient:       cs.kubeClient,
		namespace:        cs.namespace,
		provisionerImage: cs.provisionerImage,
		pullPolicy:       cs.pullPolicy,
	}
	if err := createSnapshotterPod(ctx, sa); err != nil {
		klog.Errorf("error creating provisioner pod :%v", err)
		return nil, err
	}

	return &csi.DeleteSnapshotResponse{}, nil
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

func convertSnapContentName(snapID string) string {
	// snapshotID is in the form of "snapshot-<snapID>"
	// snapshotContentName is in the form of "snapshotcontent-<snapID>"
	return strings.Replace(snapID, "snapshot-", "snapcontent-", 1)
}
