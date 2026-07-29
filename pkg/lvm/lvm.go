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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	cmd "github.com/harvester/go-common/command"
	ioutil "github.com/harvester/go-common/io"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"
)

// Lvm contains the main parameters
type Lvm struct {
	name              string
	nodeID            string
	version           string
	endpoint          string
	hostWritePath     string
	maxVolumesPerNode int64
	provisionerImage  string
	pullPolicy        v1.PullPolicy
	namespace         string

	ids *identityServer
	ns  *nodeServer
	cs  *controllerServer
}

var (
	vendorVersion = "dev"
)

type actionType string

// source lv could be a generic volume or a snapshot
type srcInfo struct {
	srcLVName string
	srcVGName string
	srcType   string
}

type volumeAction struct {
	action           actionType
	name             string
	nodeName         string
	size             int64
	lvmType          string
	provisionerImage string
	pullPolicy       v1.PullPolicy
	kubeClient       kubernetes.Interface
	namespace        string
	vgName           string
	hostWritePath    string
	srcInfo          *srcInfo
	//srcDev           string
}

type snapshotAction struct {
	action           actionType
	srcVolName       string
	nodeName         string
	snapshotName     string
	snapSize         int64
	provisionerImage string
	pullPolicy       v1.PullPolicy
	kubeClient       kubernetes.Interface
	namespace        string
	vgName           string
	lvType           string
	hostWritePath    string
}

const (
	ThinVolType      = "thin"
	ThinPoolType     = "thin-pool"
	LinearType       = "linear"
	StripedType      = "striped"
	DmThinType       = "dm-thin"
	actionTypeCreate = "create"
	actionTypeDelete = "delete"
	actionTypeClone  = "clone"
	pullIfNotPresent = "ifnotpresent"
	DefaultChunkSize = 4 * 1024 * 1024
	// xfsExternalLogSignature may be reported for stale XFS log metadata on reused LV extents.
	// See https://github.com/metal-stack/csi-driver-lvm/pull/77.
	xfsExternalLogSignature = "xfs_external_log"
)

type commandExecutor interface {
	Execute(command string, args []string) (string, error)
}

var (
	newCommandExecutor = func() commandExecutor {
		return cmd.NewExecutor()
	}
	unmountPath = unix.Unmount
)

type lsblkReport struct {
	BlockDevices []struct {
		FSType *string `json:"fstype"`
	} `json:"blockdevices"`
}

type logicalVolume struct {
	Name    string `json:"lv_name"`
	VGName  string `json:"vg_name"`
	Size    string `json:"lv_size"`
	SegType string `json:"segtype"`
	Origin  string `json:"origin"`
}

type logicalVolumeReport struct {
	Reports []struct {
		Volumes []logicalVolume `json:"lv"`
	} `json:"report"`
}

// NewLvmDriver creates the driver
func NewLvmDriver(driverName, nodeID, endpoint string, hostWritePath string, maxVolumesPerNode int64, version string, namespace string, provisionerImage string, pullPolicy string) (*Lvm, error) {
	if driverName == "" {
		return nil, fmt.Errorf("no driver name provided")
	}

	if nodeID == "" {
		return nil, fmt.Errorf("no node id provided")
	}

	if endpoint == "" {
		return nil, fmt.Errorf("no driver endpoint provided")
	}
	if version != "" {
		vendorVersion = version
	}

	pp := v1.PullAlways
	if strings.ToLower(pullPolicy) == pullIfNotPresent {
		klog.Info("pullpolicy: IfNotPresent")
		pp = v1.PullIfNotPresent
	}

	klog.Infof("Driver: %v ", driverName)
	klog.Infof("Version: %s", vendorVersion)

	return &Lvm{
		name:              driverName,
		version:           vendorVersion,
		nodeID:            nodeID,
		endpoint:          endpoint,
		hostWritePath:     hostWritePath,
		maxVolumesPerNode: maxVolumesPerNode,
		namespace:         namespace,
		provisionerImage:  provisionerImage,
		pullPolicy:        pp,
	}, nil
}

// Run starts the lvm plugin
func (lvm *Lvm) Run() error {
	var err error
	// Create GRPC servers
	lvm.ids = newIdentityServer(lvm.name, lvm.version)
	lvm.ns, err = newNodeServer(lvm.nodeID, lvm.maxVolumesPerNode)
	if err != nil {
		return err
	}
	lvm.cs, err = newControllerServer(lvm.nodeID, lvm.hostWritePath, lvm.namespace, lvm.provisionerImage, lvm.pullPolicy)
	if err != nil {
		return err
	}
	s := newNonBlockingGRPCServer()
	s.start(lvm.endpoint, lvm.ids, lvm.cs, lvm.ns)
	s.wait()
	return nil
}

func mountLV(lvname, mountPath, vgName, fsType string, mountOptions []string, readOnly bool) (string, error) {
	executor := newCommandExecutor()
	lvPath := fmt.Sprintf("/dev/%s/%s", vgName, lvname)
	fsType = defaultFilesystemType(fsType)

	formatOutput, err := ensureFilesystem(executor, lvPath, fsType)
	if err != nil {
		return formatOutput, err
	}

	return mountFilesystem(executor, lvPath, mountPath, fsType, mountOptions, readOnly)
}

func defaultFilesystemType(fsType string) string {
	if fsType == "" {
		return "ext4"
	}
	return fsType
}

func ensureFilesystem(executor commandExecutor, lvPath, fsType string) (string, error) {
	existingFSType, err := getFilesystemType(executor, lvPath)
	if err != nil {
		return "", err
	}

	forceFormat := existingFSType == xfsExternalLogSignature
	if existingFSType != "" && !forceFormat && existingFSType != fsType {
		return "", fmt.Errorf("target fsType is %s but %s found", fsType, existingFSType)
	}
	if existingFSType != "" && !forceFormat {
		return "", nil
	}

	formatArgs := []string{}
	if forceFormat {
		formatArgs = append(formatArgs, "-f")
	}
	formatArgs = append(formatArgs, lvPath)

	klog.Infof("formatting with mkfs.%s %s", fsType, strings.Join(formatArgs, " "))
	out, err := executor.Execute(fmt.Sprintf("mkfs.%s", fsType), formatArgs)
	if err != nil {
		return out, fmt.Errorf("unable to format lv:%s err:%w", lvPath, err)
	}
	return out, nil
}

func mountFilesystem(executor commandExecutor, lvPath, mountPath, fsType string, mountOptions []string, readOnly bool) (string, error) {
	if err := os.MkdirAll(mountPath, 0777|os.ModeSetgid); err != nil {
		return "", fmt.Errorf("unable to create mount directory for lv:%s err:%w", lvPath, err)
	}

	mountArgs := buildFilesystemMountArgs(lvPath, mountPath, fsType, mountOptions, readOnly)
	out, err := performFilesystemMount(executor, mountArgs, lvPath, mountPath, readOnly)
	if err != nil {
		return out, err
	}
	if readOnly {
		return "", nil
	}
	if err := os.Chmod(mountPath, 0777|os.ModeSetgid); err != nil {
		return "", fmt.Errorf("unable to change permissions of volume mount %s err:%w", mountPath, err)
	}
	return "", nil
}

func buildFilesystemMountArgs(lvPath, mountPath, fsType string, mountOptions []string, readOnly bool) []string {
	// --make-shared is required that this mount is visible outside this container.
	mountArgs := []string{"--make-shared", "-t", fsType}
	options := normalizeMountOptions(mountOptions, readOnly)
	if len(options) > 0 {
		mountArgs = append(mountArgs, "-o", strings.Join(options, ","))
	}
	return append(mountArgs, lvPath, mountPath)
}

func performFilesystemMount(executor commandExecutor, mountArgs []string, lvPath, mountPath string, readOnly bool) (string, error) {
	klog.Infof("mountlv command: mount %s", mountArgs)
	out, err := executor.Execute("mount", mountArgs)
	if err == nil {
		klog.Infof("mountlv output:%s", out)
		return out, nil
	}

	mountOutput := out + " " + err.Error()
	if !strings.Contains(strings.ToLower(mountOutput), "already mounted") {
		return out, fmt.Errorf("unable to mount %s to %s err:%w output:%s", lvPath, mountPath, err, out)
	}
	if err := validateExistingMount(executor, mountPath, readOnly); err != nil {
		return out, fmt.Errorf("existing mount at %s is incompatible: %w", mountPath, err)
	}
	return out, nil
}

func validateExistingMount(executor commandExecutor, mountPath string, readOnly bool) error {
	out, err := executor.Execute(
		"findmnt",
		[]string{"--noheadings", "--output", "OPTIONS", "--mountpoint", mountPath},
	)
	if err != nil {
		return fmt.Errorf("unable to verify mount: %w output:%s", err, out)
	}
	if !readOnly {
		return nil
	}
	for _, option := range strings.Split(strings.TrimSpace(out), ",") {
		if option == "ro" {
			return nil
		}
	}
	return fmt.Errorf("readonly was requested but existing mount options are %q", strings.TrimSpace(out))
}

func getFilesystemType(executor commandExecutor, lvPath string) (string, error) {
	out, err := executor.Execute("lsblk", []string{"--json", "--output", "FSTYPE", lvPath})
	if err != nil {
		return "", fmt.Errorf("unable to determine filesystem type for %s: %w", lvPath, err)
	}

	report := lsblkReport{}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		return "", fmt.Errorf("unable to parse lsblk output for %s: %w", lvPath, err)
	}
	if len(report.BlockDevices) != 1 {
		return "", fmt.Errorf("expected one block device for %s, got %d", lvPath, len(report.BlockDevices))
	}
	if report.BlockDevices[0].FSType == nil {
		return "", nil
	}
	return strings.TrimSpace(*report.BlockDevices[0].FSType), nil
}

func normalizeMountOptions(values []string, readOnly bool) []string {
	options := make([]string, 0, len(values)+1)
	hasReadOnly := false
	for _, option := range strings.Split(strings.Join(values, ","), ",") {
		option = strings.TrimSpace(option)
		if option == "" || readOnly && option == "rw" {
			continue
		}
		if option == "ro" {
			hasReadOnly = true
		}
		options = append(options, option)
	}
	if !readOnly || hasReadOnly {
		return options
	}
	return append(options, "ro")
}

func bindMountLV(lvname, mountPath, vgName string, readOnly bool) (string, error) {
	executor := newCommandExecutor()
	lvPath := fmt.Sprintf("/dev/%s/%s", vgName, lvname)
	if err := prepareBindMountTarget(lvname, mountPath); err != nil {
		return "", err
	}

	out, err := performBindMount(executor, lvPath, mountPath)
	if err != nil {
		return out, err
	}
	if readOnly {
		out, err = remountBindReadOnly(executor, mountPath)
		if err != nil {
			return out, err
		}
	}
	klog.Infof("bindmountlv output:%s", out)
	return "", nil
}

func prepareBindMountTarget(lvName, mountPath string) error {
	target, err := os.OpenFile(mountPath, os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return validateExistingBindMountTarget(lvName, mountPath)
	}
	if err != nil {
		return fmt.Errorf("unable to create mount target for lv:%s err:%w", lvName, err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("unable to close mount target for lv:%s err:%w", lvName, err)
	}
	if err := os.Chmod(mountPath, 0777|os.ModeSetgid); err != nil {
		return fmt.Errorf("unable to change permissions of volume mount %s err:%w", mountPath, err)
	}
	return nil
}

func validateExistingBindMountTarget(lvName, mountPath string) error {
	info, err := os.Lstat(mountPath)
	if err != nil {
		return fmt.Errorf("unable to inspect mount target for lv:%s err:%w", lvName, err)
	}
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("mount target for lv:%s must be a file", lvName)
	}
	return nil
}

func performBindMount(executor commandExecutor, source, target string) (string, error) {
	// --make-shared is required that this mount is visible outside this container.
	// --bind is required for raw block volumes to make them visible inside the pod.
	args := []string{"--make-shared", "--bind", source, target}
	klog.Infof("bindmountlv command: mount %s", args)
	out, err := executor.Execute("mount", args)
	if err == nil || strings.Contains(strings.ToLower(out+" "+err.Error()), "already mounted") {
		return out, nil
	}
	return out, fmt.Errorf("unable to mount %s to %s err:%w output:%s", source, target, err, out)
}

func remountBindReadOnly(executor commandExecutor, target string) (string, error) {
	args := []string{"-o", "remount,bind,ro", target}
	klog.Infof("remounting bind mount read-only: mount %s", args)
	out, err := executor.Execute("mount", args)
	if err == nil {
		return out, nil
	}
	_ = unmountTarget(target)
	return out, fmt.Errorf("unable to remount %s read-only: %w output:%s", target, err, out)
}

func unmountTarget(targetPath string) error {
	err := unmountPath(targetPath, 0)
	if isUnmountComplete(err) {
		return nil
	}
	if errors.Is(err, unix.EBUSY) {
		return lazyUnmountTarget(targetPath)
	}
	return fmt.Errorf("unable to unmount %s: %w", targetPath, err)
}

func lazyUnmountTarget(targetPath string) error {
	err := unmountPath(targetPath, unix.MNT_DETACH)
	if isUnmountComplete(err) {
		return nil
	}
	return fmt.Errorf("unable to lazily unmount %s: %w", targetPath, err)
}

func isUnmountComplete(err error) bool {
	return err == nil || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT)
}

func createSnapshotterPod(ctx context.Context, sa snapshotAction) error {
	args, err := snapshotProvisionerArgs(sa)
	if err != nil {
		return err
	}

	klog.Infof("start snapshotterPod with args:%s", args)
	action := fmt.Sprintf("snap-%s", sa.action)
	pod := genProvisionerPodContent(action, sa.snapshotName, sa.nodeName, sa.hostWritePath, sa.provisionerImage, sa.pullPolicy, args)
	if err := runProvisionerPod(ctx, sa.kubeClient.CoreV1().Pods(sa.namespace), pod, "snapshot", sa.action); err != nil {
		return err
	}

	klog.Infof("Snapshot %v has been %vd on %v", sa.snapshotName, sa.action, sa.nodeName)
	return nil
}

func snapshotProvisionerArgs(sa snapshotAction) ([]string, error) {
	if sa.snapshotName == "" || sa.nodeName == "" {
		klog.Errorf("invalid snapshotAction %v", sa)
		return nil, fmt.Errorf("invalid empty name or path or node")
	}
	if sa.action == actionTypeCreate && sa.srcVolName == "" {
		klog.Errorf("invalid snapshotAction %v", sa)
		return nil, fmt.Errorf("createlv without srcVolName")
	}

	switch sa.action {
	case actionTypeCreate:
		return []string{"createsnap", "--snapname", sa.snapshotName, "--lvname", sa.srcVolName, "--vgname", sa.vgName, "--lvsize", fmt.Sprintf("%d", sa.snapSize), "--lvmtype", sa.lvType}, nil
	case actionTypeDelete:
		return []string{"deletesnap", "--snapname", sa.snapshotName, "--vgname", sa.vgName}, nil
	default:
		return nil, fmt.Errorf("invalid action %v", sa.action)
	}
}

func createProvisionerPod(ctx context.Context, va volumeAction) error {
	args, err := volumeProvisionerArgs(va)
	if err != nil {
		return err
	}

	klog.Infof("start provisionerPod with args:%s", args)
	action := fmt.Sprintf("lvm-%s", va.action)
	pod := genProvisionerPodContent(action, va.name, va.nodeName, va.hostWritePath, va.provisionerImage, va.pullPolicy, args)
	if err := runProvisionerPod(ctx, va.kubeClient.CoreV1().Pods(va.namespace), pod, "volume", va.action); err != nil {
		return err
	}

	klog.Infof("Volume %v has been %vd on %v", va.name, va.action, va.nodeName)
	return nil
}

func volumeProvisionerArgs(va volumeAction) ([]string, error) {
	if va.name == "" || va.nodeName == "" {
		return nil, fmt.Errorf("invalid empty name or path or node")
	}
	if va.action == actionTypeCreate && va.lvmType == "" {
		return nil, fmt.Errorf("createlv without lvm type")
	}

	var args []string
	switch va.action {
	case actionTypeCreate:
		args = append(args, "createlv", "--lvsize", fmt.Sprintf("%d", va.size), "--lvmtype", va.lvmType, "--vgname", va.vgName)
	case actionTypeDelete:
		if va.srcInfo == nil {
			return nil, fmt.Errorf("deletelv without source volume information")
		}
		args = append(args, "deletelv", "--srcvgname", va.srcInfo.srcVGName, "--srctype", va.srcInfo.srcType)
	case actionTypeClone:
		if va.srcInfo == nil {
			return nil, fmt.Errorf("clonelv without source volume information")
		}
		args = append(args, "clonelv", "--srclvname", va.srcInfo.srcLVName, "--srcvgname", va.srcInfo.srcVGName, "--srctype", va.srcInfo.srcType, "--lvsize", fmt.Sprintf("%d", va.size), "--vgname", va.vgName, "--lvmtype", va.lvmType)
	default:
		return nil, fmt.Errorf("invalid action %v", va.action)
	}
	return append(args, "--lvname", va.name), nil
}

func runProvisionerPod(ctx context.Context, pods corev1.PodInterface, pod *v1.Pod, resource string, action actionType) error {
	if err := createOrReuseProvisionerPod(ctx, pods, pod); err != nil {
		return err
	}

	terminal, err := waitForProvisionerPod(ctx, pods, pod.Name, resource, action)
	if !terminal {
		klog.Infof("retaining nonterminal provisioner pod %s for a later retry: %v", pod.Name, err)
		return err
	}

	deleteProvisionerPod(pods, pod.Name)
	return err
}

func createOrReuseProvisionerPod(ctx context.Context, pods corev1.PodInterface, pod *v1.Pod) error {
	_, err := pods.Create(ctx, pod, metav1.CreateOptions{})
	if k8serror.IsAlreadyExists(err) {
		klog.Infof("reusing existing provisioner pod %s", pod.Name)
		return nil
	}
	return err
}

func waitForProvisionerPod(ctx context.Context, pods corev1.PodInterface, podName, resource string, action actionType) (bool, error) {
	const provisionerPodPollAttempts = 60
	for range provisionerPodPollAttempts {
		pod, readErr := pods.Get(ctx, podName, metav1.GetOptions{})
		terminal, resultErr := provisionerPodResult(ctx, pod, readErr, resource, action)
		if terminal || resultErr != nil {
			return terminal, resultErr
		}
		if err := waitForRetry(ctx); err != nil {
			return false, err
		}
	}
	return false, fmt.Errorf("%s %s process timeout after %d polling attempts", resource, action, provisionerPodPollAttempts)
}

func provisionerPodResult(ctx context.Context, pod *v1.Pod, readErr error, resource string, action actionType) (bool, error) {
	if readErr != nil {
		if ctx.Err() != nil {
			return false, status.FromContextError(ctx.Err()).Err()
		}
		klog.Errorf("error reading provisioner pod: %v", readErr)
		return false, nil
	}
	if pod == nil {
		return false, status.Error(codes.Internal, "Kubernetes API returned an empty provisioner pod")
	}

	switch pod.Status.Phase {
	case v1.PodFailed:
		klog.Infof("provisioner pod %s terminated with failure", pod.Name)
		return true, provisionerPodFailure(pod, resource, action)
	case v1.PodSucceeded:
		klog.Infof("provisioner pod %s terminated successfully", pod.Name)
		return true, nil
	default:
		klog.Infof("provisioner pod %s status:%s", pod.Name, pod.Status.Phase)
		return false, nil
	}
}

func provisionerPodFailure(pod *v1.Pod, resource string, action actionType) error {
	details := make([]string, 0, len(pod.Status.ContainerStatuses)+1)
	if pod.Status.Reason != "" || pod.Status.Message != "" {
		details = append(details, fmt.Sprintf(
			"pod reason=%s message=%s",
			valueOrUnknown(pod.Status.Reason),
			valueOrUnknown(compactErrorMessage(pod.Status.Message)),
		))
	}
	for _, containerStatus := range pod.Status.ContainerStatuses {
		terminated := containerStatus.State.Terminated
		if terminated == nil {
			continue
		}
		detail := fmt.Sprintf(
			"container %s exited with code %d reason=%s",
			containerStatus.Name,
			terminated.ExitCode,
			valueOrUnknown(terminated.Reason),
		)
		if message := compactErrorMessage(terminated.Message); message != "" {
			detail += " message=" + message
		}
		details = append(details, detail)
	}

	message := fmt.Sprintf("%s %s helper pod %s failed", resource, action, pod.Name)
	if len(details) > 0 {
		message += ": " + strings.Join(details, "; ")
	}
	return status.Error(codes.Internal, message)
}

func compactErrorMessage(message string) string {
	const maxLength = 1024
	message = strings.Join(strings.Fields(message), " ")
	runes := []rune(message)
	if len(runes) <= maxLength {
		return message
	}
	return string(runes[:maxLength]) + "..."
}

func valueOrUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func deleteProvisionerPod(pods corev1.PodInterface, podName string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pods.Delete(cleanupCtx, podName, metav1.DeleteOptions{}); err != nil && !k8serror.IsNotFound(err) {
		klog.Errorf("unable to delete provisioner pod %s: %v", podName, err)
	}
}

func waitForRetry(ctx context.Context) error {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-timer.C:
		return nil
	}
}

// VgExists checks if the given volume group exists
func VgExists(vgname string) bool {
	executor := newCommandExecutor()
	out, err := executor.Execute("vgs", []string{vgname, "--noheadings", "-o", "vg_name"})
	if err != nil {
		klog.Infof("unable to list existing volumegroups:%v", err)
		return false
	}
	return vgname == strings.TrimSpace(out)
}

// EnsureVG verifies that a volume group is discoverable and activates its LVs.
func EnsureVG(vgName string) error {
	if err := ensureVGDiscovered(vgName); err != nil {
		return err
	}
	if err := activateVolumeGroups(vgName); err != nil {
		return fmt.Errorf("unable to activate volume group %s: %w", vgName, err)
	}
	return nil
}

func ensureVGDiscovered(vgName string) error {
	if VgExists(vgName) {
		return nil
	}
	if err := scanVolumeGroups(); err != nil {
		return err
	}
	if VgExists(vgName) {
		return nil
	}
	return fmt.Errorf("volume group %s does not exist; ensure it is created on the target node", vgName)
}

// VgActivate executes vgchange -ay to activate all volumes of all discovered
// volume groups.
func VgActivate() error {
	if err := scanVolumeGroups(); err != nil {
		return err
	}
	return activateVolumeGroups()
}

func scanVolumeGroups() error {
	executor := newCommandExecutor()
	out, err := executor.Execute("vgscan", []string{})
	if err != nil {
		return fmt.Errorf("unable to scan for volume groups: %w output:%s", err, out)
	}
	return nil
}

func activateVolumeGroups(vgNames ...string) error {
	executor := newCommandExecutor()
	args := append([]string{"-ay"}, vgNames...)
	out, err := executor.Execute("vgchange", args)
	if err != nil {
		return fmt.Errorf("unable to activate volume groups: %w output:%s", err, out)
	}
	return nil
}

// CreateLVS creates the new volume, used by lvcreate provisioner pod
func CreateLVS(vg string, name string, size uint64, lvmType string) (string, error) {
	if size == 0 {
		return "", fmt.Errorf("size must be greater than 0")
	}
	if lvmType != StripedType && lvmType != DmThinType {
		return "", fmt.Errorf("unsupported lvmtype: %s", lvmType)
	}

	existing, found, err := getLogicalVolume(vg, name)
	if err != nil {
		return "", fmt.Errorf("unable to check existing logical volume %s/%s: %w", vg, name, err)
	}
	if found {
		if err := validateExistingVolume(existing, size, lvmType); err != nil {
			return "", err
		}
		klog.Infof("logical volume %s/%s already exists and is compatible", vg, name)
		return name, nil
	}

	thinPoolName, err := prepareThinPool(vg, lvmType)
	if err != nil {
		return "", err
	}

	executor := newCommandExecutor()
	args := []string{"-v", "--yes", "-n", name, "-W", "y"}

	pvs, err := pvCount(vg)
	if err != nil {
		return "", fmt.Errorf("unable to determine pv count of vg: %w", err)
	}

	if pvs < 2 && lvmType == StripedType {
		klog.Warning("pvcount is <2, the striped does not meaningful.")
	}

	switch lvmType {
	case StripedType:
		args = append(args, "-L", fmt.Sprintf("%db", size), "--type", "striped", "--stripes", fmt.Sprintf("%d", pvs), vg)
	case DmThinType:
		args = append(args, "-V", fmt.Sprintf("%db", size), "--thin-pool", thinPoolName, vg)
	}

	tags := []string{"harvester-csi-lvm"}
	for _, tag := range tags {
		args = append(args, "--addtag", tag)
	}
	klog.Infof("lvcreate %s", args)
	out, err := executor.Execute("lvcreate", args)
	return out, err
}

func prepareThinPool(vgName, lvmType string) (string, error) {
	if lvmType != DmThinType {
		return "", nil
	}

	thinPoolName := fmt.Sprintf("%s-thinpool", vgName)
	found, err := getThinPool(vgName, thinPoolName)
	if err != nil {
		return "", fmt.Errorf("unable to determine if thin pool %s/%s exists: %w", vgName, thinPoolName, err)
	}
	if found {
		return thinPoolName, validateThinPool(vgName, thinPoolName)
	}

	args := []string{"-l90%FREE", "--thinpool", thinPoolName, vgName}
	klog.Infof("lvcreate %s", args)
	if _, err := newCommandExecutor().Execute("lvcreate", args); err != nil {
		return "", fmt.Errorf("unable to create thin pool %s/%s: %w", vgName, thinPoolName, err)
	}
	return thinPoolName, nil
}

func getLogicalVolume(vgName, lvName string) (logicalVolume, bool, error) {
	volumes, err := listLogicalVolumes()
	if err != nil {
		return logicalVolume{}, false, err
	}
	for _, volume := range volumes {
		if volume.VGName == vgName && volume.Name == lvName {
			return volume, true, nil
		}
	}
	return logicalVolume{}, false, nil
}

func getLogicalVolumeByName(lvName string) (logicalVolume, error) {
	volumes, err := listLogicalVolumes()
	if err != nil {
		return logicalVolume{}, err
	}

	var match *logicalVolume
	for i := range volumes {
		if volumes[i].Name != lvName {
			continue
		}
		if match != nil {
			return logicalVolume{}, fmt.Errorf(
				"logical volume name %s is ambiguous across volume groups %s and %s",
				lvName,
				match.VGName,
				volumes[i].VGName,
			)
		}
		match = &volumes[i]
	}
	if match == nil {
		return logicalVolume{}, fmt.Errorf("logical volume %s does not exist", lvName)
	}
	return *match, nil
}

func listLogicalVolumes() ([]logicalVolume, error) {
	executor := newCommandExecutor()
	args := []string{
		"--reportformat", "json",
		"--units", "b",
		"--nosuffix",
		"--options", "lv_name,vg_name,lv_size,segtype,origin",
	}
	out, err := executor.Execute("lvs", args)
	if err != nil {
		return nil, err
	}

	report := logicalVolumeReport{}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		return nil, fmt.Errorf("unable to parse lvs output: %w", err)
	}

	volumes := []logicalVolume{}
	for _, item := range report.Reports {
		for _, volume := range item.Volumes {
			volumes = append(volumes, normalizeLogicalVolume(volume))
		}
	}
	return volumes, nil
}

func normalizeLogicalVolume(volume logicalVolume) logicalVolume {
	volume.Name = strings.TrimSpace(volume.Name)
	volume.VGName = strings.TrimSpace(volume.VGName)
	volume.Size = strings.TrimSpace(volume.Size)
	volume.SegType = strings.TrimSpace(volume.SegType)
	volume.Origin = strings.TrimSpace(volume.Origin)
	return volume
}

func validateExistingVolume(volume logicalVolume, requestedSize uint64, requestedType string) error {
	actualSize, err := parseLogicalVolumeSize(volume)
	if err != nil {
		return err
	}
	if actualSize < requestedSize {
		return fmt.Errorf(
			"existing logical volume %s/%s has size %d, smaller than requested size %d",
			volume.VGName,
			volume.Name,
			actualSize,
			requestedSize,
		)
	}

	typeMatches := requestedType == DmThinType && volume.SegType == ThinVolType ||
		requestedType == StripedType && (volume.SegType == StripedType || volume.SegType == LinearType)
	if !typeMatches {
		return fmt.Errorf(
			"existing logical volume %s/%s has type %s, incompatible with requested type %s",
			volume.VGName,
			volume.Name,
			volume.SegType,
			requestedType,
		)
	}
	return nil
}

func parseLogicalVolumeSize(volume logicalVolume) (uint64, error) {
	size, err := strconv.ParseUint(volume.Size, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unable to parse size %q of logical volume %s/%s: %w", volume.Size, volume.VGName, volume.Name, err)
	}
	return size, nil
}

func extendLVS(name string, size uint64, isBlock bool) (string, error) {
	volume, err := getLogicalVolumeByName(name)
	if err != nil {
		return "", fmt.Errorf("unable to get logical volume %s: %w", name, err)
	}

	lvSize, err := parseLogicalVolumeSize(volume)
	if err != nil {
		return "", err
	}
	if lvSize >= size {
		klog.Infof("logical volume %s already has size %d, satisfying requested size %d", name, lvSize, size)
		return "", nil
	}

	args := buildLVExtendArgs(volume.VGName, name, size, isBlock)
	klog.Infof("lvextend %s", args)
	return newCommandExecutor().Execute("lvextend", args)
}

func buildLVExtendArgs(vgName, lvName string, size uint64, isBlock bool) []string {
	resizeOption := "-r"
	if isBlock {
		resizeOption = "-n"
	}
	return []string{
		"-L", fmt.Sprintf("%db", size),
		resizeOption,
		fmt.Sprintf("%s/%s", vgName, lvName),
	}
}

// RemoveLVS executes lvremove
func RemoveLVS(name string) (string, error) {
	volume, err := getLogicalVolumeByName(name)
	if err != nil {
		return "", fmt.Errorf("unable to get logical volume %s: %w", name, err)
	}
	return RemoveLVSInVG(volume.VGName, name)
}

// RemoveLVSInVG removes a logical volume from the specified VG. It is
// idempotent: an already absent LV is treated as success.
func RemoveLVSInVG(vgName, name string) (string, error) {
	_, found, err := getLogicalVolume(vgName, name)
	if err != nil {
		return "", fmt.Errorf("unable to check logical volume %s/%s: %w", vgName, name, err)
	}
	if !found {
		return fmt.Sprintf("logical volume %s does not exist. Assuming it has already been deleted.", name), nil
	}

	executor := newCommandExecutor()
	args := make([]string, 0, 3)
	args = append(args, "-q", "-y")
	args = append(args, fmt.Sprintf("%s/%s", vgName, name))
	klog.Infof("lvremove %s", args)
	out, err := executor.Execute("lvremove", args)
	return out, err
}

func CreateSnapshot(snapshotName, srcVolName, vgName string, volSize int64, lvType string, forClone bool) (string, error) {
	if snapshotName == "" || srcVolName == "" {
		return "", fmt.Errorf("invalid empty name or path")
	}

	if volSize == 0 {
		return "", fmt.Errorf("size must be greater than 0")
	}

	if _, found, err := getLogicalVolume(vgName, srcVolName); err != nil {
		return "", fmt.Errorf("unable to check source logical volume %s/%s: %w", vgName, srcVolName, err)
	} else if !found {
		return "", fmt.Errorf("logical volume %s does not exist", srcVolName)
	}

	// Names starting "snapshot" are reserved for internal use by LVM
	// we patch new snapName as "lvm-<snapshotName>"
	// parameters: -s, -y, -a n, -n, snapshotName, (-L, volSize), vgName/srcVolName
	backendSnapshotName := snapshotName
	if !forClone {
		backendSnapshotName = fmt.Sprintf("lvm-%s", snapshotName)
	}
	existing, found, err := getLogicalVolume(vgName, backendSnapshotName)
	if err != nil {
		return "", fmt.Errorf("unable to check existing snapshot %s/%s: %w", vgName, backendSnapshotName, err)
	}
	if found {
		if existing.Origin != srcVolName {
			return "", fmt.Errorf(
				"existing snapshot %s/%s has origin %s, expected %s",
				vgName,
				backendSnapshotName,
				existing.Origin,
				srcVolName,
			)
		}
		klog.Infof("snapshot %s/%s already exists and is compatible", vgName, backendSnapshotName)
		return backendSnapshotName, nil
	}

	executor := newCommandExecutor()
	args := []string{"-s", "-y"}
	if !forClone {
		args = append(args, "-a", "n")
	}
	args = append(args, "-n", backendSnapshotName)
	switch lvType {
	case StripedType:
		args = append(args, "-L", fmt.Sprintf("%db", volSize))
	case DmThinType:
		// no-size option for the dm-thin
		break
	default:
		return "", fmt.Errorf("unsupported lvmtype: %s", lvType)
	}
	args = append(args, fmt.Sprintf("%s/%s", vgName, srcVolName))
	klog.Infof("lvcreate %s", args)
	out, err := executor.Execute("lvcreate", args)
	return out, err
}

func DeleteSnapshot(snapshotName, vgName string) (string, error) {
	if snapshotName == "" {
		return "", fmt.Errorf("invalid empty name")
	}

	// Names starting "snapshot" are reserved for internal use by LVM
	// we patch new snapName as "lvm-<snapshotName>"
	snapshotName = fmt.Sprintf("lvm-%s", snapshotName)
	if _, found, err := getLogicalVolume(vgName, snapshotName); err != nil {
		return "", fmt.Errorf("unable to check snapshot %s/%s: %w", vgName, snapshotName, err)
	} else if !found {
		return fmt.Sprintf("snapshot %s/%s does not exist. Assuming it has already been deleted.", vgName, snapshotName), nil
	}

	executor := newCommandExecutor()
	args := make([]string, 0, 3)
	args = append(args, "-q", "-y")
	args = append(args, fmt.Sprintf("/dev/%s/%s", vgName, snapshotName))
	klog.Infof("lvremove %s", args)
	out, err := executor.Execute("lvremove", args)
	return out, err
}

func CloneDevice(src, dst *os.File) error {
	return ioutil.Copy(src, dst, DefaultChunkSize)
}

func RemoveThinPool(vgName string) error {
	// if the vg is not empty, we should skip this steps
	targetThinPool := fmt.Sprintf("%s-thinpool", vgName)
	thinPoolInfo, err := getThinPoolAndCounts(vgName)
	if err != nil {
		return fmt.Errorf("unable to get thinpool and count: %w", err)
	}
	if _, ok := thinPoolInfo[targetThinPool]; !ok {
		klog.Infof("thinpool %s does not exist, skip remove!", targetThinPool)
		return nil
	}
	if thinPoolInfo[targetThinPool] > 0 {
		klog.Infof("thinpool %s is not empty, skip remove!", targetThinPool)
		return nil
	}
	_, err = RemoveLVSInVG(vgName, targetThinPool)
	if err != nil {
		return fmt.Errorf("unable to remove thinpool: %w", err)
	}
	return nil
}

func pvCount(vgname string) (int, error) {
	executor := newCommandExecutor()
	out, err := executor.Execute("vgs", []string{vgname, "--noheadings", "-o", "pv_count"})
	if err != nil {
		return 0, err
	}
	outStr := strings.TrimSpace(out)
	count, err := strconv.Atoi(outStr)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func getThinPoolAndCounts(vgName string) (map[string]int, error) {
	executor := newCommandExecutor()
	// we would like to get the segtype, name as below:
	// vg02 thin thinvol01    <-- this is volume
	// vg02 thin-pool vg02-thinpool 1  <-- this is thin-pool
	// Query all VGs so an already removed VG produces an empty result rather
	// than turning an idempotent delete into an error.
	args := []string{"--noheadings", "-o", "vg_name,segtype,name,thin_count"}
	out, err := executor.Execute("lvs", args)
	if err != nil {
		klog.Infof("execute lvs %s, err: %v", args, err)
		return nil, err
	}
	lines := strings.Split(out, "\n")
	// type[Name]
	// type: thin -> vol name
	//       thin-pool -> pool name -> thin_count
	thinInfo := make(map[string]int)
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 4 {
			klog.Infof("Skip thin info: %s", line)
			continue
		}
		// confirm again, we only care about thin-pool
		// thinInfo: map[<thin pool>]:<thin count>
		if parts[0] == vgName && parts[1] == ThinPoolType {
			count, err := strconv.Atoi(parts[3])
			if err != nil {
				return nil, err
			}
			thinInfo[parts[2]] = count
		}
	}
	return thinInfo, nil
}

func getThinPool(vgName, thinpoolName string) (bool, error) {
	thinPoolInfo, err := getThinPoolAndCounts(vgName)
	if err != nil {
		return false, err
	}
	if _, ok := thinPoolInfo[thinpoolName]; ok {
		return true, nil
	}
	return false, nil
}

func validateThinPool(vgName, thinpoolName string) error {
	executor := newCommandExecutor()
	args := []string{
		"--noheadings",
		"-o", "lv_attr,lv_health_status",
		fmt.Sprintf("%s/%s", vgName, thinpoolName),
	}
	out, err := executor.Execute("lvs", args)
	if err != nil {
		return fmt.Errorf("unable to inspect thin pool %s/%s: %w output:%s", vgName, thinpoolName, err, out)
	}

	fields := strings.Fields(out)
	if len(fields) == 0 || len(fields[0]) < 5 {
		return fmt.Errorf("thin pool %s/%s returned invalid attributes %q", vgName, thinpoolName, strings.TrimSpace(out))
	}
	if fields[0][4] != 'a' {
		return fmt.Errorf("thin pool %s/%s is inactive (attributes %s)", vgName, thinpoolName, fields[0])
	}
	if len(fields) > 1 {
		return fmt.Errorf("thin pool %s/%s is unhealthy: %s", vgName, thinpoolName, strings.Join(fields[1:], " "))
	}
	return nil
}

func genProvisionerPodContent(
	action, name, targetNode, hostWritePath, provisionerImage string,
	pullPolicy v1.PullPolicy,
	args []string,
) *v1.Pod {
	hostPathTypeDirOrCreate := v1.HostPathDirectoryOrCreate
	hostPathTypeDirectory := v1.HostPathDirectory
	privileged := true
	mountPropagationBidirectional := v1.MountPropagationBidirectional
	targetPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: string(action) + "-" + name,
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyNever,
			NodeName:      targetNode,
			Tolerations: []v1.Toleration{
				{
					Operator: v1.TolerationOpExists,
				},
			},
			Containers: []v1.Container{
				{
					Name:    "csi-lvmplugin-" + string(action),
					Image:   provisionerImage,
					Command: []string{"csi-lvmplugin-provisioner"},
					Args:    args,
					VolumeMounts: []v1.VolumeMount{
						{
							Name:             "devices",
							ReadOnly:         false,
							MountPath:        "/dev",
							MountPropagation: &mountPropagationBidirectional,
						},
						{
							Name:      "modules",
							ReadOnly:  false,
							MountPath: "/lib/modules",
						},
						{
							Name:             "lvmbackup",
							ReadOnly:         false,
							MountPath:        "/etc/lvm/backup",
							MountPropagation: &mountPropagationBidirectional,
						},
						{
							Name:             "lvmcache",
							ReadOnly:         false,
							MountPath:        "/etc/lvm/cache",
							MountPropagation: &mountPropagationBidirectional,
						},
						{
							Name:             "lvmlock",
							ReadOnly:         false,
							MountPath:        "/run/lock/lvm",
							MountPropagation: &mountPropagationBidirectional,
						},
						{
							Name:      "host-lvm-conf",
							ReadOnly:  true,
							MountPath: "/etc/lvm/lvm.conf",
						},
						{
							Name:      "host-run-udev",
							ReadOnly:  true,
							MountPath: "/run/udev",
						},
					},
					TerminationMessagePath:   "/termination.log",
					TerminationMessagePolicy: v1.TerminationMessageFallbackToLogsOnError,
					ImagePullPolicy:          pullPolicy,
					SecurityContext: &v1.SecurityContext{
						Privileged: &privileged,
					},
				},
			},
			Volumes: []v1.Volume{
				{
					Name: "devices",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Path: "/dev",
							Type: &hostPathTypeDirOrCreate,
						},
					},
				},
				{
					Name: "modules",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Path: "/lib/modules",
							Type: &hostPathTypeDirOrCreate,
						},
					},
				},
				{
					Name: "lvmbackup",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Path: filepath.Join(hostWritePath, "backup"),
							Type: &hostPathTypeDirOrCreate,
						},
					},
				},
				{
					Name: "lvmcache",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Path: filepath.Join(hostWritePath, "cache"),
							Type: &hostPathTypeDirOrCreate,
						},
					},
				},
				{
					Name: "lvmlock",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Path: filepath.Join(hostWritePath, "lock"),
							Type: &hostPathTypeDirOrCreate,
						},
					},
				},
				{
					Name: "host-lvm-conf",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Path: "/etc/lvm/lvm.conf",
						},
					},
				},
				{
					Name: "host-run-udev",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Path: "/run/udev",
							Type: &hostPathTypeDirectory,
						},
					},
				},
			},
		},
	}
	return targetPod
}
