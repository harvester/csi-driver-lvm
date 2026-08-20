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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
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
	ThinVolType          = "thin"
	ThinPoolType         = "thin-pool"
	LinearType           = "linear"
	StripedType          = "striped"
	DmThinType           = "dm-thin"
	thinPoolDataExtents  = "90%FREE"
	thinPoolChunkSize    = "512K"
	thinPoolMetadataSize = "16G"
	actionTypeCreate     = "create"
	actionTypeDelete     = "delete"
	actionTypeClone      = "clone"
	pullIfNotPresent     = "ifnotpresent"
	DefaultChunkSize     = 4 * 1024 * 1024

	// Keep these exit statuses synchronized with util-linux misc-utils/blkid.c.
	// https://github.com/util-linux/util-linux/blob/master/misc-utils/blkid.c
	blkidExitNotFound  = 2
	blkidExitOther     = 4
	blkidExitAmbiguous = 8
	unmountTimeout     = 30 * time.Second
)

type commandExecutor interface {
	Execute(command string, args []string) (string, error)
}

type timedCommandExecutor interface {
	commandExecutor
	SetTimeout(timeout time.Duration)
}

var (
	newCommandExecutor = func() commandExecutor {
		return cmd.NewExecutor()
	}
	newUnmountExecutor = func() timedCommandExecutor {
		return cmd.NewExecutor()
	}
)

type wipefsReport struct {
	Signatures *[]struct {
		Type string `json:"type"`
	} `json:"signatures"`
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

	if existingFSType != "" && existingFSType != fsType {
		return "", fmt.Errorf("target fsType is %q but %q found", fsType, existingFSType)
	}
	if existingFSType != "" {
		return "", nil
	}

	klog.Infof("formatting with mkfs.%s %s", fsType, lvPath)
	out, err := executor.Execute(fmt.Sprintf("mkfs.%s", fsType), []string{lvPath})
	if err != nil {
		return out, fmt.Errorf("unable to format lv:%q err:%w", lvPath, err)
	}
	return out, nil
}

func mountFilesystem(executor commandExecutor, lvPath, mountPath, fsType string, mountOptions []string, readOnly bool) (string, error) {
	if err := os.MkdirAll(mountPath, 0777|os.ModeSetgid); err != nil {
		return "", fmt.Errorf("unable to create mount directory for lv:%q err:%w", lvPath, err)
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
		return "", fmt.Errorf("unable to change permissions of volume mount %q err:%w", mountPath, err)
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
		return out, fmt.Errorf("unable to mount %q to %q err:%w output:%s", lvPath, mountPath, err, out)
	}
	if err := validateExistingMount(executor, mountPath, readOnly); err != nil {
		return out, fmt.Errorf("existing mount at %q is incompatible: %w", mountPath, err)
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
	// Probe the device directly. lsblk can return an empty FSTYPE when the
	// container's udev database is missing or stale, even for a mounted filesystem.
	out, err := executor.Execute("blkid", []string{"-p", "-s", "TYPE", "-o", "value", lvPath})
	if err == nil {
		fsType := strings.TrimSpace(out)
		if fsType == "" {
			return "", fmt.Errorf("blkid succeeded but returned no filesystem type for %q", lvPath)
		}
		return fsType, nil
	}

	exitCode, ok := commandExitCode(err)
	if !ok {
		return "", fmt.Errorf("unable to determine filesystem type for %q with blkid: %w", lvPath, err)
	}

	switch exitCode {
	case blkidExitNotFound:
		return getFilesystemTypeFromWipefs(executor, lvPath)
	case blkidExitAmbiguous:
		return "", fmt.Errorf("ambiguous filesystem signatures detected on %q by blkid: %w", lvPath, err)
	case blkidExitOther:
		return "", fmt.Errorf("blkid failed to inspect %q: %w", lvPath, err)
	default:
		return "", fmt.Errorf("blkid returned unexpected status %d for %q: %w", exitCode, lvPath, err)
	}
}

func commandExitCode(err error) (int, bool) {
	var exitError interface {
		ExitCode() int
	}
	if !errors.As(err, &exitError) {
		return 0, false
	}
	return exitError.ExitCode(), true
}

func getFilesystemTypeFromWipefs(executor commandExecutor, lvPath string) (string, error) {
	out, err := executor.Execute("wipefs", []string{"--no-act", "--json", "--output", "TYPE", lvPath})
	if err != nil {
		return "", fmt.Errorf("unable to confirm filesystem signatures for %q with wipefs: %w", lvPath, err)
	}

	report := wipefsReport{}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		return "", fmt.Errorf("unable to parse wipefs output for %q: %w", lvPath, err)
	}
	if report.Signatures == nil {
		return "", fmt.Errorf("wipefs output for %q does not contain a signatures array", lvPath)
	}
	if len(*report.Signatures) == 0 {
		return "", nil
	}
	if len(*report.Signatures) != 1 {
		return "", fmt.Errorf("ambiguous filesystem signatures detected on %q by wipefs: found %d signatures", lvPath, len(*report.Signatures))
	}

	fsType := strings.TrimSpace((*report.Signatures)[0].Type)
	if fsType == "" {
		return "", fmt.Errorf("wipefs reported a signature without a type for %q", lvPath)
	}
	return fsType, nil
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
		return fmt.Errorf("unable to create mount target for lv:%q err:%w", lvName, err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("unable to close mount target for lv:%q err:%w", lvName, err)
	}
	if err := os.Chmod(mountPath, 0777|os.ModeSetgid); err != nil {
		return fmt.Errorf("unable to change permissions of volume mount %q err:%w", mountPath, err)
	}
	return nil
}

func validateExistingBindMountTarget(lvName, mountPath string) error {
	info, err := os.Lstat(mountPath)
	if err != nil {
		return fmt.Errorf("unable to inspect mount target for lv:%q err:%w", lvName, err)
	}
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("mount target for lv:%q must be a file", lvName)
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
	return out, fmt.Errorf("unable to mount %q to %q err:%w output:%s", source, target, err, out)
}

func remountBindReadOnly(executor commandExecutor, target string) (string, error) {
	args := []string{"-o", "remount,bind,ro", target}
	klog.Infof("remounting bind mount read-only: mount %s", args)
	out, err := executor.Execute("mount", args)
	if err == nil {
		return out, nil
	}
	_ = unmountTarget(target)
	return out, fmt.Errorf("unable to remount %q read-only: %w output:%s", target, err, out)
}

func unmountTarget(targetPath string) error {
	executor := newUnmountExecutor()
	executor.SetTimeout(unmountTimeout)

	out, err := executor.Execute("umount", []string{targetPath})
	if isUnmountComplete(out, err) {
		return nil
	}
	reason := "failed"
	if errors.Is(err, cmd.ErrCmdTimeout) {
		reason = "timed out"
	}
	klog.Warningf("unmount of %s %s; retrying with force: %v output:%s", targetPath, reason, err, out)
	return forceUnmount(executor, targetPath)
}

func forceUnmount(executor commandExecutor, targetPath string) error {
	out, err := executor.Execute("umount", []string{"--force", targetPath})
	if isUnmountComplete(out, err) {
		return nil
	}
	if !errors.Is(err, cmd.ErrCmdTimeout) {
		return fmt.Errorf("unable to force unmount %q: %w output:%s", targetPath, err, out)
	}

	klog.Infof("forced unmount of %s timed out; retrying with lazy unmount", targetPath)
	return lazyUnmount(executor, targetPath)
}

func lazyUnmount(executor commandExecutor, targetPath string) error {
	out, err := executor.Execute("umount", []string{"--force", "--lazy", targetPath})
	if isUnmountComplete(out, err) {
		return nil
	}
	return fmt.Errorf("unable to force lazy unmount %q: %w output:%s", targetPath, err, out)
}

func isUnmountComplete(output string, err error) bool {
	if err == nil {
		return true
	}
	message := strings.ToLower(output + " " + err.Error())
	return strings.Contains(message, "not mounted") || strings.Contains(message, "no mount point specified")
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
		return nil, fmt.Errorf("invalid action %q", sa.action)
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
		return nil, fmt.Errorf("invalid action %q", va.action)
	}
	return append(args, "--lvname", va.name), nil
}

func runProvisionerPod(ctx context.Context, pods corev1.PodInterface, pod *v1.Pod, resource string, action actionType) error {
	if err := ensureProvisionerPod(ctx, pods, pod); err != nil {
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

func ensureProvisionerPod(ctx context.Context, pods corev1.PodInterface, pod *v1.Pod) error {
	_, err := pods.Create(ctx, pod, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !k8serror.IsAlreadyExists(err) {
		return err
	}
	return reuseProvisionerPod(ctx, pods, pod)
}

func reuseProvisionerPod(ctx context.Context, pods corev1.PodInterface, desired *v1.Pod) error {
	existing, err := pods.Get(ctx, desired.Name, metav1.GetOptions{})
	if err != nil {
		return status.Errorf(codes.Unavailable, "failed to get existing provisioner pod %q: %v", desired.Name, err)
	}
	if existing == nil {
		return status.Errorf(
			codes.Unavailable,
			"Kubernetes API returned an empty provisioner pod %q",
			desired.Name,
		)
	}
	if existing.DeletionTimestamp != nil {
		return forceDeletePod(ctx, pods, existing.Name)
	}
	if apiequality.Semantic.DeepDerivative(desired.Spec, existing.Spec) {
		klog.Infof("reusing existing provisioner pod %s", desired.Name)
		return nil
	}
	return retireProvisionerPod(ctx, pods, existing)
}

func forceDeletePod(ctx context.Context, pods corev1.PodInterface, podName string) error {
	gracePeriod := int64(0)
	options := metav1.DeleteOptions{GracePeriodSeconds: &gracePeriod}
	if err := pods.Delete(ctx, podName, options); err != nil && !k8serror.IsNotFound(err) {
		return status.Errorf(codes.Unavailable, "failed to force-delete provisioner pod %q: %v", podName, err)
	}
	return status.Errorf(codes.Unavailable, "force-deleted provisioner pod %q; retry the request", podName)
}

func retireProvisionerPod(ctx context.Context, pods corev1.PodInterface, pod *v1.Pod) error {
	if !podTerminal(pod) {
		// Do not interrupt an LVM operation that may still be in progress.
		return status.Errorf(
			codes.Unavailable,
			"provisioner pod %q belongs to a different request and is still %q",
			pod.Name,
			valueOrUnknown(string(pod.Status.Phase)),
		)
	}
	if err := pods.Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !k8serror.IsNotFound(err) {
		return status.Errorf(codes.Unavailable, "failed to delete stale provisioner pod %q: %v", pod.Name, err)
	}
	return status.Errorf(codes.Unavailable, "removed stale provisioner pod %q; retry the request", pod.Name)
}

func podTerminal(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed
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
		return fmt.Errorf("unable to activate volume group %q: %w", vgName, err)
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
	return fmt.Errorf("volume group %q does not exist; ensure it is created on the target node", vgName)
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
		return "", fmt.Errorf("unsupported lvmtype: %q", lvmType)
	}

	existing, found, err := getLogicalVolume(vg, name)
	if err != nil {
		return "", fmt.Errorf("unable to check existing logical volume %q/%q: %w", vg, name, err)
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
		return "", fmt.Errorf("unable to determine if thin pool %q/%q exists: %w", vgName, thinPoolName, err)
	}
	if found {
		return thinPoolName, validateThinPool(vgName, thinPoolName)
	}

	args := thinPoolCreateArgs(vgName, thinPoolName)
	klog.Infof("lvcreate %s", args)
	if _, err := newCommandExecutor().Execute("lvcreate", args); err != nil {
		return "", fmt.Errorf("unable to create thin pool %q/%q: %w", vgName, thinPoolName, err)
	}
	return thinPoolName, nil
}

func thinPoolCreateArgs(vg, thinPoolName string) []string {
	return []string{
		"--type", ThinPoolType,
		"--name", thinPoolName,
		"--extents", thinPoolDataExtents,
		"--chunksize", thinPoolChunkSize,
		"--poolmetadatasize", thinPoolMetadataSize,
		"--poolmetadataspare", "y",
		vg,
	}
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
				"logical volume name %q is ambiguous across volume groups %q and %q",
				lvName,
				match.VGName,
				volumes[i].VGName,
			)
		}
		match = &volumes[i]
	}
	if match == nil {
		return logicalVolume{}, fmt.Errorf("logical volume %q does not exist", lvName)
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
			"existing logical volume %q/%q has size %d, smaller than requested size %d",
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
			"existing logical volume %q/%q has type %q, incompatible with requested type %q",
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
		return 0, fmt.Errorf("unable to parse size %q of logical volume %q/%q: %w", volume.Size, volume.VGName, volume.Name, err)
	}
	return size, nil
}

func extendLVS(name string, size uint64, isBlock bool, volumePath string) (string, error) {
	volume, err := getLogicalVolumeByName(name)
	if err != nil {
		return "", fmt.Errorf("unable to get logical volume %q: %w", name, err)
	}

	executor := newCommandExecutor()
	lvOutput, err := ensureLogicalVolumeSize(executor, volume, size)
	if err != nil {
		return lvOutput, err
	}
	if isBlock {
		return lvOutput, nil
	}

	fsOutput, err := resizeFilesystem(executor, volume, volumePath)
	return combineCommandOutput(lvOutput, fsOutput), err
}

func ensureLogicalVolumeSize(executor commandExecutor, volume logicalVolume, size uint64) (string, error) {
	lvSize, err := parseLogicalVolumeSize(volume)
	if err != nil {
		return "", err
	}
	if lvSize >= size {
		klog.Infof("logical volume %s already has size %d, satisfying requested size %d", volume.Name, lvSize, size)
		return "", nil
	}

	args := buildLVExtendArgs(volume.VGName, volume.Name, size)
	klog.Infof("lvextend %s", args)
	output, err := executor.Execute("lvextend", args)
	if err != nil {
		return output, fmt.Errorf("unable to extend logical volume %q/%q: %w", volume.VGName, volume.Name, err)
	}
	return output, nil
}

func resizeFilesystem(executor commandExecutor, volume logicalVolume, volumePath string) (string, error) {
	devicePath := fmt.Sprintf("/dev/%s/%s", volume.VGName, volume.Name)
	fsType, err := getFilesystemType(executor, devicePath)
	if err != nil {
		return "", fmt.Errorf("unable to detect filesystem on %q: %w", devicePath, err)
	}

	var command string
	var args []string
	switch fsType {
	case "ext2", "ext3", "ext4":
		command = "resize2fs"
		args = []string{devicePath}
	case "xfs":
		command = "xfs_growfs"
		args = []string{"-d", volumePath}
	default:
		return "", fmt.Errorf("filesystem type %q on %q does not support expansion", fsType, devicePath)
	}

	klog.Infof("%s %s", command, args)
	output, err := executor.Execute(command, args)
	if err != nil {
		return output, fmt.Errorf("unable to resize %q filesystem on %q: %w", fsType, devicePath, err)
	}
	return output, nil
}

func combineCommandOutput(outputs ...string) string {
	nonEmpty := make([]string, 0, len(outputs))
	for _, output := range outputs {
		if output = strings.TrimSpace(output); output != "" {
			nonEmpty = append(nonEmpty, output)
		}
	}
	return strings.Join(nonEmpty, "\n")
}

func buildLVExtendArgs(vgName, lvName string, size uint64) []string {
	return []string{
		"-L", fmt.Sprintf("%db", size),
		"-n",
		fmt.Sprintf("%s/%s", vgName, lvName),
	}
}

// RemoveLVSInVG removes a logical volume from the specified VG. It is
// idempotent: an already absent LV is treated as success.
func RemoveLVSInVG(vgName, name string) (string, error) {
	_, found, err := getLogicalVolume(vgName, name)
	if err != nil {
		return "", fmt.Errorf("unable to check logical volume %q/%q: %w", vgName, name, err)
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
		return "", fmt.Errorf("unable to check source logical volume %q/%q: %w", vgName, srcVolName, err)
	} else if !found {
		return "", fmt.Errorf("logical volume %q does not exist", srcVolName)
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
		return "", fmt.Errorf("unable to check existing snapshot %q/%q: %w", vgName, backendSnapshotName, err)
	}
	if found {
		if existing.Origin != srcVolName {
			return "", fmt.Errorf(
				"existing snapshot %q/%q has origin %q, expected %q",
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
		return "", fmt.Errorf("unsupported lvmtype: %q", lvType)
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
		return "", fmt.Errorf("unable to check snapshot %q/%q: %w", vgName, snapshotName, err)
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
		return fmt.Errorf("unable to inspect thin pool %q/%q: %w output:%s", vgName, thinpoolName, err, out)
	}

	fields := strings.Fields(out)
	if len(fields) == 0 || len(fields[0]) < 5 {
		return fmt.Errorf("thin pool %q/%q returned invalid attributes %q", vgName, thinpoolName, strings.TrimSpace(out))
	}
	if fields[0][4] != 'a' {
		return fmt.Errorf("thin pool %q/%q is inactive (attributes %q)", vgName, thinpoolName, fields[0])
	}
	if len(fields) > 1 {
		return fmt.Errorf("thin pool %q/%q is unhealthy: %q", vgName, thinpoolName, strings.Join(fields[1:], " "))
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
