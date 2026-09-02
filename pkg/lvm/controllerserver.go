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
	"crypto/sha256"
	"fmt"
	"strconv"
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
	kubeClient       kubernetes.Interface
	provisionerImage string
	pullPolicy       v1.PullPolicy
	namespace        string
	snapClient       snapclient.Interface
}

const (
	snapshotNodeAnnotation = "lvm.driver.harvesterhci.io/nodeName"
	snapshotVGAnnotation   = "lvm.driver.harvesterhci.io/vgName"
	// snapshotEncryptedAnnotation lets an administrator declare the encryption
	// state of a pre-provisioned snapshot whose location record is gone, the
	// same way the node/VG annotations declare its physical location.
	snapshotEncryptedAnnotation = "lvm.driver.harvesterhci.io/encrypted"

	snapshotLocationConfigMapPrefix = "csi-lvm-snapshot-location-"
	snapshotLocationLabel           = "lvm.driver.harvesterhci.io/snapshot-location"
	snapshotLocationHandleKey       = "snapshotHandle"
	snapshotLocationNodeKey         = "nodeName"
	snapshotLocationVGKey           = "vgName"
	// snapshotLocationEncryptedKey persists the (non-secret) encryption state of
	// the source volume so a restore can tell whether the snapshot's blocks
	// carry a LUKS header. Records written before encryption support omit it.
	snapshotLocationEncryptedKey = "encrypted"
)

// sourceEncryption is the encryption state of a restore source. It has to be a
// tri-state: snapshot location records written before encryption support exist
// in the wild and carry no encryption state at all, which is different from
// knowing the source was plain.
type sourceEncryption int

const (
	sourceEncryptionUnknown sourceEncryption = iota
	sourceEncryptionPlain
	sourceEncryptionEncrypted
)

func (s sourceEncryption) String() string {
	switch s {
	case sourceEncryptionPlain:
		return "unencrypted"
	case sourceEncryptionEncrypted:
		return "encrypted"
	default:
		return "unknown"
	}
}

func encryptionStateOf(encrypted bool) sourceEncryption {
	if encrypted {
		return sourceEncryptionEncrypted
	}
	return sourceEncryptionPlain
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

	source := req.GetVolumeContentSource()
	volumeContext := buildVolumeContext(req.GetParameters(), requiredBytes, source != nil)

	node, topology, err := topologyFromAccessibility(req.GetAccessibilityRequirements())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	klog.Infof("creating volume %s on node: %s", req.GetName(), node)

	dstEncrypted := isEncrypted(req.GetParameters())

	// Restores are block-level clones of the source LV, so the destination
	// inherits the source's on-disk encryption state. Reject the combinations
	// that would corrupt or expose data before any LV is created.
	if source != nil {
		if err := cs.validateRestoreEncryption(ctx, source, dstEncrypted, req.GetSecrets()); err != nil {
			return nil, err
		}
	}

	// Encrypted volumes lose the LUKS2 header (16 MiB) to overhead, so grow the
	// backing LV by that much to still expose the requested capacity. The volume
	// still reports its requested (usable) size below.
	lvBytes := backingLVBytes(requiredBytes, dstEncrypted)

	if err := cs.provisionVolume(ctx, req, node, lvmType, vgName, lvBytes); err != nil {
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
		return status.Errorf(codes.NotFound, "source snapshot %q not found", snapshotID)
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
		return status.Errorf(codes.NotFound, "source volume %q not found", volumeID)
	}
	if err != nil {
		return status.Errorf(codes.Unavailable, "failed to get source volume %q: %v", volumeID, err)
	}
	return cs.cloneFromVolume(ctx, volume, dstName, dstNode, dstLVMType, dstVGName, dstSize)
}

// validateRestoreEncryption rejects restores whose source and destination
// disagree about encryption, and makes a missing restore credential fail here
// rather than later on the node.
//
// Both restore paths (snapshot and volume clone) copy the source LV block for
// block, so the destination LV comes up carrying exactly the source's on-disk
// layout. Restoring an unencrypted source into an encrypted StorageClass would
// therefore hand the node a plain filesystem that NodePublishVolume must not
// LUKS-format, and restoring an encrypted source into a plain StorageClass
// would expose the raw LUKS container to the workload. Converting between the
// two needs a copy that writes through a new dm-crypt mapper, which this driver
// does not implement, so both directions are refused.
func (cs *controllerServer) validateRestoreEncryption(
	ctx context.Context,
	source *csi.VolumeContentSource,
	dstEncrypted bool,
	secrets map[string]string,
) error {
	srcState, err := cs.sourceEncryptionState(ctx, source)
	if err != nil {
		return err
	}

	switch {
	case srcState == encryptionStateOf(dstEncrypted):
		// Matching states: nothing to convert.
	case srcState == sourceEncryptionUnknown && !dstEncrypted:
		// The destination never formats, and a LUKS container reaching a plain
		// destination is still caught on the node before it is published.
		klog.Warningf("restore source encryption state is unknown; continuing into an unencrypted StorageClass")
	default:
		return status.Errorf(
			codes.InvalidArgument,
			"cannot restore an %s source into an %s StorageClass: restores are block-level clones and "+
				"cannot convert encryption state; restore into a StorageClass whose %q parameter matches the source",
			srcState,
			encryptionStateOf(dstEncrypted),
			encryptedParam,
		)
	}

	if !dstEncrypted {
		return nil
	}

	// The restored LV keeps the source's LUKS header, so only the source's
	// passphrase can open it. We cannot verify the passphrase from the
	// controller (the header lives on the node), but we can fail early and
	// explicitly when no usable credential reached us at all.
	if _, err := extractCryptoParams(secrets); err != nil {
		return status.Errorf(
			codes.InvalidArgument,
			"restoring into an encrypted StorageClass requires the source volume's passphrase: %v; "+
				"the StorageClass must set csi.storage.k8s.io/provisioner-secret-name and the referenced "+
				"secret must hold the passphrase the source was encrypted with (note that templated names "+
				"such as ${pvc.name}-luks resolve to a different secret for the restored PVC)",
			err,
		)
	}
	return nil
}

// sourceEncryptionState resolves the encryption state of a restore source.
func (cs *controllerServer) sourceEncryptionState(
	ctx context.Context,
	source *csi.VolumeContentSource,
) (sourceEncryption, error) {
	switch source.Type.(type) {
	case *csi.VolumeContentSource_Volume:
		volumeID := source.GetVolume().GetVolumeId()
		if volumeID == "" {
			return sourceEncryptionUnknown, status.Error(codes.InvalidArgument, "source volume ID is empty")
		}
		return cs.persistentVolumeEncryption(ctx, volumeID)
	case *csi.VolumeContentSource_Snapshot:
		snapshotID := source.GetSnapshot().GetSnapshotId()
		if snapshotID == "" {
			return sourceEncryptionUnknown, status.Error(codes.InvalidArgument, "source snapshot ID is empty")
		}
		content, err := cs.getSnapshotContent(ctx, snapshotID)
		if err != nil {
			return sourceEncryptionUnknown, err
		}
		if content == nil {
			return sourceEncryptionUnknown, status.Errorf(codes.NotFound, "source snapshot %q not found", snapshotID)
		}
		return cs.snapshotEncryptionState(ctx, snapshotID, content)
	default:
		return sourceEncryptionUnknown, status.Errorf(codes.InvalidArgument, "%v not a proper volume source", source)
	}
}

// snapshotEncryptionState prefers the still-present source volume, then the
// content's annotation, then the location record written at CreateSnapshot.
func (cs *controllerServer) snapshotEncryptionState(
	ctx context.Context,
	snapshotID string,
	content *snapv1.VolumeSnapshotContent,
) (sourceEncryption, error) {
	if handle := content.Spec.Source.VolumeHandle; handle != nil && *handle != "" {
		state, err := cs.persistentVolumeEncryption(ctx, *handle)
		if err == nil {
			return state, nil
		}
		if status.Code(err) != codes.NotFound {
			return sourceEncryptionUnknown, err
		}
		klog.Warningf(
			"source volume %s for snapshot %s is absent; falling back to recorded encryption state",
			*handle,
			snapshotID,
		)
	}

	if annotation, ok := content.Annotations[snapshotEncryptedAnnotation]; ok {
		encrypted, err := strconv.ParseBool(annotation)
		if err != nil {
			return sourceEncryptionUnknown, status.Errorf(
				codes.FailedPrecondition,
				"snapshot content %q has an invalid %q annotation %q",
				content.Name,
				snapshotEncryptedAnnotation,
				annotation,
			)
		}
		return encryptionStateOf(encrypted), nil
	}

	location, found, err := cs.recordedLocation(ctx, snapshotID)
	if err != nil {
		return sourceEncryptionUnknown, err
	}
	if !found || location.encrypted == "" {
		return sourceEncryptionUnknown, nil
	}
	encrypted, err := strconv.ParseBool(location.encrypted)
	if err != nil {
		return sourceEncryptionUnknown, status.Errorf(
			codes.FailedPrecondition,
			"snapshot %q has an invalid recorded encryption state %q",
			snapshotID,
			location.encrypted,
		)
	}
	return encryptionStateOf(encrypted), nil
}

func (cs *controllerServer) persistentVolumeEncryption(ctx context.Context, volumeID string) (sourceEncryption, error) {
	volume, err := cs.kubeClient.CoreV1().PersistentVolumes().Get(ctx, volumeID, metav1.GetOptions{})
	if k8serror.IsNotFound(err) {
		return sourceEncryptionUnknown, status.Errorf(codes.NotFound, "source volume %q not found", volumeID)
	}
	if err != nil {
		return sourceEncryptionUnknown, status.Errorf(codes.Unavailable, "failed to get source volume %q: %v", volumeID, err)
	}
	encrypted, err := encryptedFromPV(volume)
	if err != nil {
		return sourceEncryptionUnknown, status.Error(codes.FailedPrecondition, err.Error())
	}
	return encryptionStateOf(encrypted), nil
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
			"source node %q and destination node %q are different (not supported)", srcNode, dstNode)
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
		return status.Errorf(codes.NotFound, "source volume %q not found", sourceVolumeID)
	}
	if err != nil {
		return status.Errorf(codes.Unavailable, "failed to get source volume %q: %v", sourceVolumeID, err)
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
		ctx,
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
	ctx context.Context,
	snapContent *snapv1.VolumeSnapshotContent,
	dstName, dstNode, dstLVType, dstVGName string,
	dstSize int64,
) (volumeAction, error) {
	snapshotID, restoreSize, err := preProvisionedSnapshotMetadata(snapContent)
	if err != nil {
		return volumeAction{}, status.Error(codes.FailedPrecondition, err.Error())
	}

	location, err := cs.resolvePreExistingSnapshotLocation(ctx, snapshotID, snapContent)
	if err != nil {
		return volumeAction{}, err
	}
	if restoreSize == 0 {
		restoreSize = dstSize
	}

	source := &srcInfo{
		srcLVName: fmt.Sprintf("lvm-%s", snapshotID),
		srcVGName: location.vgName,
		// Pre-provisioned contents do not carry the source LVM type. Restores
		// use the destination StorageClass type, which is also what determines
		// whether the optimized same-VG dm-thin clone path can be used.
		srcType: dstLVType,
	}
	return cs.newCloneVolumeAction(source, location.nodeName, dstName, dstNode, dstLVType, dstVGName, restoreSize, dstSize)
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
		return nil, status.Errorf(codes.Unavailable, "failed to get volume %q: %v", volumeID, err)
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
		return false, status.Errorf(codes.Unavailable, "failed to get node %q: %v", nodeName, err)
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
		return nil, status.Errorf(codes.NotFound, "source volume %q not found", volumeID)
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "failed to get source volume %q: %v", volumeID, err)
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

	// The snapshot is a block-level copy of the source LV, so it inherits the
	// source's LUKS header (or lack of one). Record that state alongside the
	// location so a restore can refuse mismatched destinations even when the
	// source volume is long gone. This is non-secret metadata: it says whether
	// the blocks are encrypted, never anything about the key.
	encrypted, err := encryptedFromPV(volume)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	// Keep the physical location independently of the VolumeSnapshotContent. A
	// Retain-policy content can be deleted and later re-created as a
	// pre-provisioned content that only carries the opaque snapshot handle.
	if err := cs.recordSnapshotLocation(ctx, snapshotName, nodeName, vgName, encrypted); err != nil {
		return nil, err
	}

	action := cs.newCreateSnapshotAction(snapshotName, volumeID, nodeName, vgName, lvmType, snapSize)
	if err := createSnapshotterPod(ctx, action); err != nil {
		klog.Errorf("error creating provisioner pod: %v", err)
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

	action, err := cs.deleteSnapshotAction(ctx, snapName, snapContent)
	if err != nil {
		return nil, err
	}
	if action == nil {
		return &csi.DeleteSnapshotResponse{}, nil
	}

	if err := createSnapshotterPod(ctx, *action); err != nil {
		klog.Errorf("error creating provisioner pod: %v", err)
		return nil, err
	}
	if err := cs.forgetSnapshotLocation(ctx, snapName); err != nil {
		// The backend deletion succeeded, so a stale location record must not
		// make this idempotent CSI operation fail. A later create with the same
		// handle will detect and report conflicting metadata.
		klog.Warningf("failed to remove location record for snapshot %s: %v", snapName, err)
	}

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
) (*snapshotAction, error) {
	if content == nil {
		return nil, status.Error(codes.FailedPrecondition, "snapshot content is nil")
	}
	if content.Spec.Source.VolumeHandle == nil {
		return cs.deletePreExistingSnapshotAction(ctx, snapshotID, content)
	}
	return cs.deleteDynamicSnapshotAction(ctx, snapshotID, *content.Spec.Source.VolumeHandle)
}

func (cs *controllerServer) deletePreExistingSnapshotAction(
	ctx context.Context,
	snapshotID string,
	content *snapv1.VolumeSnapshotContent,
) (*snapshotAction, error) {
	if content.Spec.Source.SnapshotHandle == nil || *content.Spec.Source.SnapshotHandle == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "snapshot content %q has no source handle", content.Name)
	}
	location, err := cs.resolvePreExistingSnapshotLocation(ctx, snapshotID, content)
	if err != nil {
		return nil, err
	}
	action := cs.newDeleteSnapshotAction(snapshotID, location.nodeName, location.vgName)
	return &action, nil
}

func (cs *controllerServer) resolvePreExistingSnapshotLocation(
	ctx context.Context,
	snapshotID string,
	content *snapv1.VolumeSnapshotContent,
) (snapshotLocation, error) {
	nodeName := content.Annotations[snapshotNodeAnnotation]
	vgName := content.Annotations[snapshotVGAnnotation]
	// Boolean inequality is XOR: reject when exactly one location annotation is missing.
	if (nodeName == "") != (vgName == "") {
		return snapshotLocation{}, status.Errorf(
			codes.FailedPrecondition,
			"pre-existing snapshot %q has incomplete location annotations; both %q and %q are required",
			snapshotID,
			snapshotNodeAnnotation,
			snapshotVGAnnotation,
		)
	}
	if nodeName != "" {
		if err := validateVGName(vgName); err != nil {
			return snapshotLocation{}, status.Errorf(
				codes.FailedPrecondition,
				"pre-existing snapshot %q has an invalid volume group: %v",
				snapshotID,
				err,
			)
		}
		return snapshotLocation{handle: snapshotID, nodeName: nodeName, vgName: vgName}, nil
	}

	recorded, found, err := cs.lookupSnapshotLocation(ctx, snapshotID)
	if err != nil {
		return snapshotLocation{}, status.Errorf(codes.Unavailable, "failed to look up location for snapshot %q: %v", snapshotID, err)
	}
	if !found {
		return snapshotLocation{}, status.Errorf(
			codes.FailedPrecondition,
			"pre-existing snapshot %q has no recorded location; add annotations %q and %q",
			snapshotID,
			snapshotNodeAnnotation,
			snapshotVGAnnotation,
		)
	}
	return recorded, nil
}

func (cs *controllerServer) deleteDynamicSnapshotAction(
	ctx context.Context,
	snapshotID, volumeID string,
) (*snapshotAction, error) {
	if volumeID == "" {
		return nil, status.Errorf(
			codes.FailedPrecondition,
			"snapshot %q has an empty source volume handle",
			snapshotID,
		)
	}

	location, found, err := cs.dynamicSnapshotLocation(ctx, snapshotID, volumeID)
	if err != nil {
		return nil, err
	}
	if !found {
		klog.Warningf(
			"source volume %s for snapshot %s is absent and no recorded location exists; "+
				"returning success to preserve delete idempotency",
			volumeID,
			snapshotID,
		)
		return nil, nil
	}

	action := cs.newDeleteSnapshotAction(snapshotID, location.nodeName, location.vgName)
	return &action, nil
}

func (cs *controllerServer) dynamicSnapshotLocation(
	ctx context.Context,
	snapshotID, volumeID string,
) (snapshotLocation, bool, error) {
	location, pvErr := cs.pvLocation(ctx, snapshotID, volumeID)
	if pvErr == nil {
		return location, true, nil
	}
	if status.Code(pvErr) == codes.Unavailable {
		return snapshotLocation{}, false, pvErr
	}

	klog.Warningf(
		"source volume %s cannot provide the location for snapshot %s; checking recorded location: %v",
		volumeID,
		snapshotID,
		pvErr,
	)
	location, found, err := cs.recordedLocation(ctx, snapshotID)
	if err != nil || found {
		return location, found, err
	}
	if k8serror.IsNotFound(pvErr) {
		return snapshotLocation{}, false, nil
	}
	return snapshotLocation{}, false, status.Errorf(
		codes.FailedPrecondition,
		"%v; snapshot %q has no recorded location",
		pvErr,
		snapshotID,
	)
}

func (cs *controllerServer) pvLocation(
	ctx context.Context,
	snapshotID, volumeID string,
) (snapshotLocation, error) {
	volume, err := cs.kubeClient.CoreV1().PersistentVolumes().Get(ctx, volumeID, metav1.GetOptions{})
	if k8serror.IsNotFound(err) {
		return snapshotLocation{}, err
	}
	if err != nil {
		return snapshotLocation{}, status.Errorf(
			codes.Unavailable,
			"failed to get source volume %q: %v",
			volumeID,
			err,
		)
	}
	nodeName, vgName, _, err := metadataFromPV(volume)
	if err != nil {
		return snapshotLocation{}, err
	}
	return snapshotLocation{handle: snapshotID, nodeName: nodeName, vgName: vgName}, nil
}

func (cs *controllerServer) recordedLocation(
	ctx context.Context,
	snapshotID string,
) (snapshotLocation, bool, error) {
	recorded, found, err := cs.lookupSnapshotLocation(ctx, snapshotID)
	if err != nil {
		return snapshotLocation{}, false, status.Errorf(
			codes.Unavailable,
			"failed to look up recorded location for snapshot %q: %v",
			snapshotID,
			err,
		)
	}
	if !found {
		return snapshotLocation{}, false, nil
	}
	return recorded, true, nil
}

func snapshotLocationConfigMapName(snapshotID string) string {
	digest := sha256.Sum256([]byte(snapshotID))
	return fmt.Sprintf("%s%x", snapshotLocationConfigMapPrefix, digest)
}

type snapshotLocation struct {
	handle   string
	nodeName string
	vgName   string
	// encrypted is the source volume's encryption state as "true"/"false", or
	// "" for records written before encryption support. It is deliberately
	// non-secret metadata: it says whether the snapshot's blocks carry a LUKS
	// header, never anything about the key.
	encrypted string
}

func (location snapshotLocation) validate() error {
	if location.handle == "" || location.nodeName == "" || location.vgName == "" {
		return fmt.Errorf("snapshot handle, node name, and volume group are required")
	}
	if err := validateVGName(location.vgName); err != nil {
		return err
	}
	if location.encrypted != "" {
		if _, err := strconv.ParseBool(location.encrypted); err != nil {
			return fmt.Errorf("encryption state %q is not a boolean", location.encrypted)
		}
	}
	return nil
}

func (location snapshotLocation) configMap(namespace string) *v1.ConfigMap {
	immutable := true
	data := map[string]string{
		snapshotLocationHandleKey: location.handle,
		snapshotLocationNodeKey:   location.nodeName,
		snapshotLocationVGKey:     location.vgName,
	}
	if location.encrypted != "" {
		data[snapshotLocationEncryptedKey] = location.encrypted
	}
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      snapshotLocationConfigMapName(location.handle),
			Namespace: namespace,
			Labels:    map[string]string{snapshotLocationLabel: "true"},
		},
		Immutable: &immutable,
		Data:      data,
	}
}

// recordSnapshotLocation creates one immutable ConfigMap per snapshot. The
// object deliberately has no owner reference: it must outlive a
// Retain-policy VolumeSnapshotContent so a later pre-provisioned content can
// still resolve the backend location.
func (cs *controllerServer) recordSnapshotLocation(ctx context.Context, snapshotID, nodeName, vgName string, encrypted bool) error {
	configMapName := snapshotLocationConfigMapName(snapshotID)
	desired := snapshotLocation{
		handle:    snapshotID,
		nodeName:  nodeName,
		vgName:    vgName,
		encrypted: strconv.FormatBool(encrypted),
	}
	if err := desired.validate(); err != nil {
		return status.Errorf(codes.FailedPrecondition,
			"invalid location for snapshot %q (ConfigMap %q): %v", snapshotID, configMapName, err)
	}

	configMaps := cs.kubeClient.CoreV1().ConfigMaps(cs.namespace)
	_, err := configMaps.Create(ctx, desired.configMap(cs.namespace), metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !k8serror.IsAlreadyExists(err) {
		return status.Errorf(codes.Unavailable,
			"failed to create location ConfigMap %q for snapshot %q: %v", configMapName, snapshotID, err)
	}

	existing, err := configMaps.Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return status.Errorf(codes.Unavailable,
			"failed to get location ConfigMap %q for snapshot %q: %v", configMapName, snapshotID, err)
	}
	actual, err := snapshotLocationFromConfigMap(existing, snapshotID)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition,
			"invalid location ConfigMap %q for snapshot %q: %v", configMapName, snapshotID, err)
	}
	if actual.nodeName != desired.nodeName || actual.vgName != desired.vgName {
		return status.Errorf(
			codes.AlreadyExists,
			"snapshot %q location ConfigMap %q already records %q/%q, requested %q/%q",
			snapshotID,
			configMapName,
			actual.nodeName,
			actual.vgName,
			desired.nodeName,
			desired.vgName,
		)
	}
	// An empty recorded state means the record predates encryption support, not
	// that the source was plain; only a real disagreement is a conflict. The
	// ConfigMap is immutable, so the missing state cannot be backfilled here.
	if actual.encrypted != "" && actual.encrypted != desired.encrypted {
		return status.Errorf(
			codes.AlreadyExists,
			"snapshot %q location ConfigMap %q already records encryption state %q, requested %q",
			snapshotID,
			configMapName,
			actual.encrypted,
			desired.encrypted,
		)
	}
	return nil
}

func (cs *controllerServer) lookupSnapshotLocation(
	ctx context.Context,
	snapshotID string,
) (snapshotLocation, bool, error) {
	name := snapshotLocationConfigMapName(snapshotID)
	configMap, err := cs.kubeClient.CoreV1().ConfigMaps(cs.namespace).Get(ctx, name, metav1.GetOptions{})
	if k8serror.IsNotFound(err) {
		return snapshotLocation{}, false, nil
	}
	if err != nil {
		return snapshotLocation{}, false, err
	}
	recorded, err := snapshotLocationFromConfigMap(configMap, snapshotID)
	if err != nil {
		return snapshotLocation{}, false, err
	}
	return recorded, true, nil
}

func snapshotLocationFromConfigMap(configMap *v1.ConfigMap, snapshotID string) (snapshotLocation, error) {
	if configMap == nil {
		return snapshotLocation{}, fmt.Errorf("snapshot location ConfigMap is nil")
	}
	if configMap.Labels[snapshotLocationLabel] != "true" {
		return snapshotLocation{}, fmt.Errorf("ConfigMap %q is not a snapshot location record", configMap.Name)
	}
	recorded := snapshotLocation{
		handle:    configMap.Data[snapshotLocationHandleKey],
		nodeName:  configMap.Data[snapshotLocationNodeKey],
		vgName:    configMap.Data[snapshotLocationVGKey],
		encrypted: configMap.Data[snapshotLocationEncryptedKey],
	}
	if recorded.handle != snapshotID {
		return snapshotLocation{}, fmt.Errorf(
			"ConfigMap %q records snapshot handle %q, expected %q",
			configMap.Name,
			recorded.handle,
			snapshotID,
		)
	}
	if err := recorded.validate(); err != nil {
		return snapshotLocation{}, fmt.Errorf("ConfigMap %q has an invalid snapshot location: %w", configMap.Name, err)
	}
	return recorded, nil
}

func (cs *controllerServer) forgetSnapshotLocation(ctx context.Context, snapshotID string) error {
	err := cs.kubeClient.CoreV1().ConfigMaps(cs.namespace).Delete(
		ctx,
		snapshotLocationConfigMapName(snapshotID),
		metav1.DeleteOptions{},
	)
	if k8serror.IsNotFound(err) {
		return nil
	}
	return err
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
