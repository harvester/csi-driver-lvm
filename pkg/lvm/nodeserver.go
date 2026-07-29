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
	"context"
	"fmt"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

const topologyKeyNode = "topology.lvm.csi/node"

type nodeServer struct {
	nodeID            string
	maxVolumesPerNode int64
	devicesPattern    string
}

func newNodeServer(nodeID string, maxVolumesPerNode int64) (*nodeServer, error) {
	if err := VgActivate(); err != nil {
		return nil, fmt.Errorf("unable to initialize LVM volume groups: %w", err)
	}

	return &nodeServer{
		nodeID:            nodeID,
		maxVolumesPerNode: maxVolumesPerNode,
	}, nil
}

func (ns *nodeServer) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	vgName, err := validateNodePublishRequest(req)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if req.GetVolumeCapability().GetBlock() != nil {
		err = ns.publishBlockVolume(req, vgName)
	} else {
		err = ns.publishFilesystemVolume(req, vgName)
	}
	if err != nil {
		return nil, err
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) publishBlockVolume(req *csi.NodePublishVolumeRequest, vgName string) error {
	output, err := bindMountLV(req.GetVolumeId(), req.GetTargetPath(), vgName, req.GetReadonly())
	if err != nil {
		return fmt.Errorf("unable to bind mount lv: %w output:%s", err, output)
	}
	klog.Infof(
		"block lv %s capability:%s vg:%s devices:%s created at:%s",
		req.GetVolumeId(),
		req.GetVolumeCapability(),
		vgName,
		ns.devicesPattern,
		req.GetTargetPath(),
	)
	return nil
}

func (ns *nodeServer) publishFilesystemVolume(req *csi.NodePublishVolumeRequest, vgName string) error {
	mount := req.GetVolumeCapability().GetMount()
	output, err := mountLV(
		req.GetVolumeId(),
		req.GetTargetPath(),
		vgName,
		mount.GetFsType(),
		mount.GetMountFlags(),
		req.GetReadonly(),
	)
	if err != nil {
		return fmt.Errorf("unable to mount lv: %w output:%s", err, output)
	}
	klog.Infof(
		"mounted lv %s capability:%s vg:%s devices:%s created at:%s",
		req.GetVolumeId(),
		req.GetVolumeCapability(),
		vgName,
		ns.devicesPattern,
		req.GetTargetPath(),
	)
	return nil
}

func (ns *nodeServer) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.Infof("NodeUnpublishRequest: %s", req)
	volID, targetPath, err := validateNodeUnpublishRequest(req)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if err := unmountTarget(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to unmount volume %s: %v", volID, err)
	}
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		return nil, status.Errorf(codes.Internal, "failed to remove target path %s: %v", targetPath, err)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeStageVolume(_ context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if len(req.GetStagingTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capability missing in request")
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnstageVolume(_ context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if len(req.GetStagingTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {

	topology := &csi.Topology{
		Segments: map[string]string{topologyKeyNode: ns.nodeID},
	}

	return &csi.NodeGetInfoResponse{
		NodeId:             ns.nodeID,
		MaxVolumesPerNode:  ns.maxVolumesPerNode,
		AccessibleTopology: topology,
	}, nil
}

func (ns *nodeServer) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {

	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
					},
				},
			},
		},
	}, nil
}

func (ns *nodeServer) NodeGetVolumeStats(_ context.Context, in *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {

	var fs unix.Statfs_t

	err := unix.Statfs(in.GetVolumePath(), &fs)
	if err != nil {
		return nil, err
	}

	diskFree := int64(fs.Bfree) * int64(fs.Bsize)   //nolint:gosec
	diskTotal := int64(fs.Blocks) * int64(fs.Bsize) //nolint:gosec

	inodesFree := int64(fs.Ffree)  //nolint:gosec
	inodesTotal := int64(fs.Files) //nolint:gosec

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Available: diskFree,
				Total:     diskTotal,
				Used:      diskTotal - diskFree,
				Unit:      csi.VolumeUsage_BYTES,
			},
			{
				Available: inodesFree,
				Total:     inodesTotal,
				Used:      inodesTotal - inodesFree,
				Unit:      csi.VolumeUsage_INODES,
			},
		},
	}, nil
}

func (ns *nodeServer) NodeExpandVolume(_ context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	klog.Infof("NodeExpandVolume: %s", req)
	volID, volPath, capacity, err := validateNodeExpandRequest(req)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	isBlock, err := isBlockVolumePath(volPath)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	output, err := extendLVS(volID, uint64(capacity), isBlock, volPath) //nolint:gosec
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to expand volume %s: %v output:%s", volID, err, output)
	}

	return &csi.NodeExpandVolumeResponse{
		CapacityBytes: capacity,
	}, nil
}

func isBlockVolumePath(volumePath string) (bool, error) {
	info, err := os.Stat(volumePath)
	if err != nil {
		return false, fmt.Errorf("could not get file information from %s: %w", volumePath, err)
	}
	if info.IsDir() {
		return false, nil
	}

	klog.Warning("volume expand request on block device: filesystem resize has to be done externally")
	return true, nil
}
