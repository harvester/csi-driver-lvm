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
	"errors"
	"fmt"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
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

	// Resolve the block device to publish. For encrypted volumes this is the
	// opened dm-crypt mapper; otherwise it is the bare logical volume.
	devicePath := fmt.Sprintf("/dev/%s/%s", vgName, req.GetVolumeId())
	encrypted := isEncrypted(req.GetVolumeContext())
	// Volumes restored from a snapshot or clone already hold their source's
	// blocks, so the encryption state on disk is the source's, not this
	// StorageClass's. Both mismatch directions are rejected at CreateVolume; the
	// checks below are the last line of defence for a volume that reached the
	// node anyway (a hand-written PV, or a PV created before this validation).
	restored := isRestoredFromSource(req.GetVolumeContext())
	if encrypted {
		params, perr := extractCryptoParams(req.GetSecrets())
		if perr != nil {
			return nil, status.Error(codes.InvalidArgument, perr.Error())
		}
		// allowFormat is false for restores: no LUKS header there means the
		// source was unencrypted, and formatting would destroy restored data.
		mapperPath, oerr := openEncryptedDevice(devicePath, req.GetVolumeId(), params, !restored)
		if oerr != nil {
			return nil, encryptedOpenError(req.GetVolumeId(), oerr)
		}
		devicePath = mapperPath
	} else if restored {
		if err := rejectRestoredLuksContainer(devicePath, req.GetVolumeId()); err != nil {
			return nil, err
		}
	}

	if req.GetVolumeCapability().GetBlock() != nil {
		err = ns.publishBlockVolume(req, devicePath)
	} else {
		err = ns.publishFilesystemVolume(req, devicePath)
	}
	if err != nil {
		// Avoid leaking an open dm-crypt mapping if the mount step failed.
		if encrypted {
			if cerr := closeEncryptedDevice(req.GetVolumeId()); cerr != nil {
				klog.Errorf("failed to close dm-crypt device for %s after publish error: %v", req.GetVolumeId(), cerr)
			}
		}
		return nil, err
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

// encryptedOpenError maps a failure to open the dm-crypt mapping onto a CSI
// code. A wrong or missing passphrase and an unencrypted restore source are
// configuration problems an operator has to fix, so they get FailedPrecondition
// (which the CO surfaces without hiding it behind endless retries) instead of
// the generic Internal used for transient cryptsetup failures.
func encryptedOpenError(volID string, err error) error {
	if errors.Is(err, errBadPassphrase) || errors.Is(err, errRestoredVolumeNotLuks) {
		return status.Errorf(codes.FailedPrecondition, "unable to open encrypted volume %s: %v", volID, err)
	}
	return status.Errorf(codes.Internal, "unable to open encrypted volume %s: %v", volID, err)
}

// rejectRestoredLuksContainer stops a volume restored from an encrypted source
// from being published through an unencrypted StorageClass. Without this the
// workload would be handed the raw, still-locked LUKS container: a raw block
// volume would surface as unreadable ciphertext, and a filesystem volume would
// be at the mercy of the mount path's signature handling.
func rejectRestoredLuksContainer(devicePath, volID string) error {
	hasLuks, err := luksHeaderPresent(devicePath)
	if err != nil {
		return status.Errorf(codes.Internal, "unable to probe %s for a LUKS header: %v", devicePath, err)
	}
	if hasLuks {
		return status.Errorf(
			codes.FailedPrecondition,
			"volume %s was restored from an encrypted source but its StorageClass does not set %q=true; "+
				"refusing to expose the raw LUKS container",
			volID,
			encryptedParam,
		)
	}
	return nil
}

func (ns *nodeServer) publishBlockVolume(req *csi.NodePublishVolumeRequest, devicePath string) error {
	output, err := bindMountLV(devicePath, req.GetTargetPath(), req.GetReadonly())
	if err != nil {
		return fmt.Errorf("unable to bind mount lv: %w output:%s", err, output)
	}
	klog.Infof(
		"block lv %s capability:%s device:%s devices:%s created at:%s",
		req.GetVolumeId(),
		req.GetVolumeCapability(),
		devicePath,
		ns.devicesPattern,
		req.GetTargetPath(),
	)
	return nil
}

func (ns *nodeServer) publishFilesystemVolume(req *csi.NodePublishVolumeRequest, devicePath string) error {
	mount := req.GetVolumeCapability().GetMount()
	output, err := mountLV(
		devicePath,
		req.GetTargetPath(),
		mount.GetFsType(),
		mount.GetMountFlags(),
		req.GetReadonly(),
	)
	if err != nil {
		return fmt.Errorf("unable to mount lv: %w output:%s", err, output)
	}
	klog.Infof(
		"mounted lv %s capability:%s device:%s devices:%s created at:%s",
		req.GetVolumeId(),
		req.GetVolumeCapability(),
		devicePath,
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
		return nil, status.Errorf(codes.Internal, "failed to unmount volume %q: %v", volID, err)
	}
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		return nil, status.Errorf(codes.Internal, "failed to remove target path %q: %v", targetPath, err)
	}

	// NodeUnpublishVolume carries no volume context or secrets, so we cannot
	// tell here whether the volume was encrypted. closeEncryptedDevice is a
	// no-op when no dm-crypt mapping exists, so it is safe to always attempt.
	if err := closeEncryptedDevice(volID); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to close encrypted volume %s: %v", volID, err)
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
	// StripSecrets keeps the node-expand-secret passphrase out of the logs for
	// encrypted volumes (external-resizer populates req.Secrets for those).
	klog.Infof("NodeExpandVolume: %s", protosanitizer.StripSecrets(req))
	volID, volPath, capacity, err := validateNodeExpandRequest(req)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	isBlock, err := isBlockVolumePath(volPath)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// Expand requests carry no volume context, so probe for an open dm-crypt
	// mapping to decide whether this is an encrypted volume.
	encrypted, err := encryptedVolumeActive(volID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to inspect encrypted volume %q: %v", volID, err)
	}

	if !encrypted {
		output, eerr := extendLVS(volID, uint64(capacity), isBlock, volPath) //nolint:gosec
		if eerr != nil {
			return nil, status.Errorf(codes.Internal, "unable to expand volume %s: %v output:%s", volID, eerr, output)
		}
		return &csi.NodeExpandVolumeResponse{CapacityBytes: capacity}, nil
	}

	// Encrypted: the LUKS resize needs the passphrase, which reaches us only if
	// the StorageClass wires csi.storage.k8s.io/node-expand-secret-name/-namespace
	// so the external-resizer populates NodeExpandVolumeRequest.Secrets.
	params, perr := extractCryptoParams(req.GetSecrets())
	if perr != nil {
		return nil, status.Errorf(codes.InvalidArgument, "encrypted volume %s expand is missing its passphrase secret (StorageClass needs a node-expand-secret): %v", volID, perr)
	}

	// Encrypted: grow the backing LV only (the filesystem, if any, lives on the
	// dm-crypt mapper, not the bare LV), then grow the crypt mapping, then the
	// filesystem on the mapper. Grow the LV by the LUKS2 header overhead so the
	// decrypted device reaches the full requested capacity (matching create).
	if output, eerr := extendLVS(volID, uint64(backingLVBytes(capacity, true)), true, volPath); eerr != nil { //nolint:gosec
		return nil, status.Errorf(codes.Internal, "unable to expand logical volume %s: %v output:%s", volID, eerr, output)
	}
	if _, output, rerr := resizeEncryptedDevice(volID, params.passphrase); rerr != nil {
		return nil, status.Errorf(codes.Internal, "unable to resize encrypted volume %s: %v output:%s", volID, rerr, output)
	}
	if !isBlock {
		if output, rerr := resizeFilesystem(newCommandExecutor(), cryptMapperPath(volID), volPath); rerr != nil {
			return nil, status.Errorf(codes.Internal, "unable to resize filesystem for %q: %v output: %s", volID, rerr, output)
		}
	}

	return &csi.NodeExpandVolumeResponse{
		CapacityBytes: capacity,
	}, nil
}

func isBlockVolumePath(volumePath string) (bool, error) {
	info, err := os.Stat(volumePath)
	if err != nil {
		return false, fmt.Errorf("could not get file information about block volume: %w", err)
	}
	if info.IsDir() {
		return false, nil
	}

	klog.Warning("volume expand request on block device: filesystem resize has to be done externally")
	return true, nil
}
