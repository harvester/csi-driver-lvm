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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	cmd "github.com/harvester/go-common/command"
	ioutil "github.com/harvester/go-common/io"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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
	kubeClient       kubernetes.Clientset
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
	kubeClient       kubernetes.Clientset
	namespace        string
	vgName           string
	lvType           string
	hostWritePath    string
}

const (
	ThinVolType        = "thin"
	ThinPoolType       = "thin-pool"
	StripedType        = "striped"
	DmThinType         = "dm-thin"
	actionTypeCreate   = "create"
	actionTypeDelete   = "delete"
	actionTypeClone    = "clone"
	pullIfNotPresent   = "ifnotpresent"
	fsTypeRegexpString = `TYPE="(\w+)"`
	DefaultChunkSize   = 4 * 1024 * 1024
)

var (
	fsTypeRegexp = regexp.MustCompile(fsTypeRegexpString)
)

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
	lvm.ns = newNodeServer(lvm.nodeID, lvm.maxVolumesPerNode)
	lvm.cs, err = newControllerServer(lvm.nodeID, lvm.hostWritePath, lvm.namespace, lvm.provisionerImage, lvm.pullPolicy)
	if err != nil {
		return err
	}
	s := newNonBlockingGRPCServer()
	s.start(lvm.endpoint, lvm.ids, lvm.cs, lvm.ns)
	s.wait()
	return nil
}

func mountLV(lvname, mountPath string, vgName string, fsType string) (string, error) {
	executor := cmd.NewExecutor()
	lvPath := fmt.Sprintf("/dev/%s/%s", vgName, lvname)

	formatted := false
	forceFormat := false
	if fsType == "" {
		fsType = "ext4"
	}
	out, err := executor.Execute("blkid", []string{lvPath})
	if err != nil {
		klog.Infof("unable to check if %s is already formatted:%v", lvPath, err)
	}
	matches := fsTypeRegexp.FindStringSubmatch(out)
	if len(matches) > 1 {
		if matches[1] == "xfs_external_log" { // If old xfs signature was found
			forceFormat = true
		} else {
			if matches[1] != fsType {
				return out, fmt.Errorf("target fsType is %s but %s found", fsType, matches[1])
			}

			formatted = true
		}
	}

	if !formatted {
		formatArgs := []string{}
		if forceFormat {
			formatArgs = append(formatArgs, "-f")
		}
		formatArgs = append(formatArgs, lvPath)

		klog.Infof("formatting with mkfs.%s %s", fsType, strings.Join(formatArgs, " "))
		out, err = executor.Execute(fmt.Sprintf("mkfs.%s", fsType), formatArgs)
		if err != nil {
			return out, fmt.Errorf("unable to format lv:%s err:%w", lvname, err)
		}
	}

	err = os.MkdirAll(mountPath, 0777|os.ModeSetgid)
	if err != nil {
		return out, fmt.Errorf("unable to create mount directory for lv:%s err:%w", lvname, err)
	}

	// --make-shared is required that this mount is visible outside this container.
	mountArgs := []string{"--make-shared", "-t", fsType, lvPath, mountPath}
	klog.Infof("mountlv command: mount %s", mountArgs)
	out, err = executor.Execute("mount", mountArgs)
	if err != nil {
		mountOutput := out
		if !strings.Contains(mountOutput, "already mounted") {
			return out, fmt.Errorf("unable to mount %s to %s err:%w output:%s", lvPath, mountPath, err, out)
		}
	}
	err = os.Chmod(mountPath, 0777|os.ModeSetgid)
	if err != nil {
		return "", fmt.Errorf("unable to change permissions of volume mount %s err:%w", mountPath, err)
	}
	klog.Infof("mountlv output:%s", out)
	return "", nil
}

func bindMountLV(lvname, mountPath string, vgName string) (string, error) {
	executor := cmd.NewExecutor()
	lvPath := fmt.Sprintf("/dev/%s/%s", vgName, lvname)
	_, err := os.Create(mountPath)
	if err != nil {
		return "", fmt.Errorf("unable to create mount directory for lv:%s err:%w", lvname, err)
	}

	// --make-shared is required that this mount is visible outside this container.
	// --bind is required for raw block volumes to make them visible inside the pod.
	mountArgs := []string{"--make-shared", "--bind", lvPath, mountPath}
	klog.Infof("bindmountlv command: mount %s", mountArgs)
	out, err := executor.Execute("mount", mountArgs)
	if err != nil {
		mountOutput := out
		if !strings.Contains(mountOutput, "already mounted") {
			return out, fmt.Errorf("unable to mount %s to %s err:%w output:%s", lvPath, mountPath, err, out)
		}
	}
	err = os.Chmod(mountPath, 0777|os.ModeSetgid)
	if err != nil {
		return "", fmt.Errorf("unable to change permissions of volume mount %s err:%w", mountPath, err)
	}
	klog.Infof("bindmountlv output:%s", out)
	return "", nil
}

func umountLV(targetPath string) {
	executor := cmd.NewExecutor()
	executor.SetTimeout(30 * time.Second)
	generalUmountArgs := []string{"--force", targetPath}
	out, err := executor.Execute("umount", generalUmountArgs)
	if err == cmd.ErrCmdTimeout {
		klog.Infof("umount %s timeout, use lazy", targetPath)
		lazyUmountArgs := []string{"--lazy", "--force", targetPath}
		out, err := executor.Execute("umount", lazyUmountArgs)
		if err != nil {
			klog.Errorf("unable to umount %s output:%s err:%v", targetPath, out, err)
		}
	} else if err != nil {
		klog.Errorf("unable to umount %s output:%s err:%v", targetPath, out, err)
	}
}

func createSnapshotterPod(ctx context.Context, sa snapshotAction) (err error) {
	if sa.snapshotName == "" || sa.nodeName == "" {
		klog.Errorf("invalid snapshotAction %v", sa)
		return fmt.Errorf("invalid empty name or path or node")
	}
	if sa.action == actionTypeCreate && sa.srcVolName == "" {
		klog.Errorf("invalid snapshotAction %v", sa)
		return fmt.Errorf("createlv without srcVolName")
	}
	args := []string{}
	switch sa.action {
	case actionTypeCreate:
		args = append(args, "createsnap", "--snapname", sa.snapshotName, "--lvname", sa.srcVolName, "--vgname", sa.vgName, "--lvsize", fmt.Sprintf("%d", sa.snapSize), "--lvmtype", sa.lvType)
	case actionTypeDelete:
		args = append(args, "deletesnap", "--snapname", sa.snapshotName, "--vgname", sa.vgName)
	default:
		return fmt.Errorf("invalid action %v", sa.action)
	}

	klog.Infof("start snapshotterPod with args:%s", args)
	action := fmt.Sprintf("snap-%s", sa.action)
	snapshotterPod := genProvisionerPodContent(action, sa.snapshotName, sa.nodeName, sa.hostWritePath, sa.provisionerImage, sa.pullPolicy, args)

	_, err = sa.kubeClient.CoreV1().Pods(sa.namespace).Create(ctx, snapshotterPod, metav1.CreateOptions{})
	if err != nil && !k8serror.IsAlreadyExists(err) {
		return err
	}

	defer func() {
		e := sa.kubeClient.CoreV1().Pods(sa.namespace).Delete(ctx, snapshotterPod.Name, metav1.DeleteOptions{})
		if e != nil {
			klog.Errorf("unable to delete the snapshotter pod: %v", e)
		}
	}()

	completed := false
	retrySeconds := 60
	for i := 0; i < retrySeconds; i++ {
		pod, err := sa.kubeClient.CoreV1().Pods(sa.namespace).Get(ctx, snapshotterPod.Name, metav1.GetOptions{})
		if pod.Status.Phase == v1.PodFailed {
			// pod terminated in time, but with failure
			// return ResourceExhausted so the requesting pod can be rescheduled to anonther node
			// see https://github.com/kubernetes-csi/external-provisioner/pull/405
			klog.Info("provisioner pod terminated with failure")
			return status.Error(codes.ResourceExhausted, "Snapshot creation failed")
		}
		if err != nil {
			klog.Errorf("error reading provisioner pod:%v", err)
		} else if pod.Status.Phase == v1.PodSucceeded {
			klog.Info("provisioner pod terminated successfully")
			completed = true
			break
		}
		klog.Infof("provisioner pod status:%s", pod.Status.Phase)
		time.Sleep(1 * time.Second)
	}
	if !completed {
		return fmt.Errorf("create process timeout after %v seconds", retrySeconds)
	}

	klog.Infof("Snapshot %v has been %vd on %v", sa.snapshotName, sa.action, sa.nodeName)
	return nil
}

func createProvisionerPod(ctx context.Context, va volumeAction) (err error) {
	if va.name == "" || va.nodeName == "" {
		return fmt.Errorf("invalid empty name or path or node")
	}
	if va.action == actionTypeCreate && va.lvmType == "" {
		return fmt.Errorf("createlv without lvm type")
	}

	args := []string{}
	switch va.action {
	case actionTypeCreate:
		args = append(args, "createlv", "--lvsize", fmt.Sprintf("%d", va.size), "--lvmtype", va.lvmType, "--vgname", va.vgName)
	case actionTypeDelete:
		args = append(args, "deletelv", "--srcvgname", va.srcInfo.srcVGName, "--srctype", va.srcInfo.srcType)
	case actionTypeClone:
		args = append(args, "clonelv", "--srclvname", va.srcInfo.srcLVName, "--srcvgname", va.srcInfo.srcVGName, "--srctype", va.srcInfo.srcType, "--lvsize", fmt.Sprintf("%d", va.size), "--vgname", va.vgName, "--lvmtype", va.lvmType)
	default:
		return fmt.Errorf("invalid action %v", va.action)
	}
	args = append(args, "--lvname", va.name)

	klog.Infof("start provisionerPod with args:%s", args)
	action := fmt.Sprintf("lvm-%s", va.action)
	provisionerPod := genProvisionerPodContent(action, va.name, va.nodeName, va.hostWritePath, va.provisionerImage, va.pullPolicy, args)

	// If it already exists due to some previous errors, the pod will be cleaned up later automatically
	// https://github.com/rancher/local-path-provisioner/issues/27
	_, err = va.kubeClient.CoreV1().Pods(va.namespace).Create(ctx, provisionerPod, metav1.CreateOptions{})
	if err != nil && !k8serror.IsAlreadyExists(err) {
		return err
	}

	defer func() {
		e := va.kubeClient.CoreV1().Pods(va.namespace).Delete(ctx, provisionerPod.Name, metav1.DeleteOptions{})
		if e != nil {
			klog.Errorf("unable to delete the provisioner pod: %v", e)
		}
	}()

	completed := false
	retrySeconds := 60
	for i := 0; i < retrySeconds; i++ {
		pod, err := va.kubeClient.CoreV1().Pods(va.namespace).Get(ctx, provisionerPod.Name, metav1.GetOptions{})
		if pod.Status.Phase == v1.PodFailed {
			// pod terminated in time, but with failure
			// return ResourceExhausted so the requesting pod can be rescheduled to anonther node
			// see https://github.com/kubernetes-csi/external-provisioner/pull/405
			klog.Info("provisioner pod terminated with failure")
			return status.Error(codes.ResourceExhausted, "volume creation failed")
		}
		if err != nil {
			klog.Errorf("error reading provisioner pod:%v", err)
		} else if pod.Status.Phase == v1.PodSucceeded {
			klog.Info("provisioner pod terminated successfully")
			completed = true
			break
		}
		klog.Infof("provisioner pod status:%s", pod.Status.Phase)
		time.Sleep(1 * time.Second)
	}
	if !completed {
		return fmt.Errorf("create process timeout after %v seconds", retrySeconds)
	}

	klog.Infof("Volume %v has been %vd on %v", va.name, va.action, va.nodeName)
	return nil
}

// VgExists checks if the given volume group exists
func VgExists(vgname string) bool {
	executor := cmd.NewExecutor()
	out, err := executor.Execute("vgs", []string{vgname, "--noheadings", "-o", "vg_name"})
	if err != nil {
		klog.Infof("unable to list existing volumegroups:%v", err)
		return false
	}
	return vgname == strings.TrimSpace(out)
}

// VgActivate execute vgchange -ay to activate all volumes of the volume group
func VgActivate() {
	executor := cmd.NewExecutor()
	// scan for vgs and activate if any
	out, err := executor.Execute("vgscan", []string{})
	if err != nil {
		klog.Infof("unable to scan for volumegroups:%s %v", out, err)
	}
	_, err = executor.Execute("vgchange", []string{"-ay"})
	if err != nil {
		klog.Infof("unable to activate volumegroups:%s %v", out, err)
	}
}

// CreateLVS creates the new volume, used by lvcreate provisioner pod
func CreateLVS(vg string, name string, size uint64, lvmType string) (string, error) {

	if lvExists(vg, name) {
		klog.Infof("logicalvolume: %s already exists\n", name)
		return name, nil
	}

	if size == 0 {
		return "", fmt.Errorf("size must be greater than 0")
	}

	// TODO: check available capacity, fail if request doesn't fit

	executor := cmd.NewExecutor()
	thinPoolName := ""
	// we need to create thin pool first if the lvmType is dm-thin
	if lvmType == DmThinType {
		thinPoolName = fmt.Sprintf("%s-thinpool", vg)
		found, err := getThinPool(vg, thinPoolName)
		if err != nil {
			return "", fmt.Errorf("unable to determine if thinpool exists: %w", err)
		}
		if !found {
			args := []string{"-l90%FREE", "--thinpool", thinPoolName, vg}
			klog.Infof("lvcreate %s", args)
			_, err := executor.Execute("lvcreate", args)
			if err != nil {
				return "", fmt.Errorf("unable to create thinpool: %w", err)
			}
		}
	}

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
	default:
		return "", fmt.Errorf("unsupported lvmtype: %s", lvmType)
	}

	tags := []string{"harvester-csi-lvm"}
	for _, tag := range tags {
		args = append(args, "--addtag", tag)
	}
	klog.Infof("lvcreate %s", args)
	out, err := executor.Execute("lvcreate", args)
	return out, err
}

func lvExists(vg string, name string) bool {
	executor := cmd.NewExecutor()
	vgname := vg + "/" + name
	out, err := executor.Execute("lvs", []string{vgname, "--noheadings", "-o", "lv_name"})
	if err != nil {
		klog.Infof("unable to list existing volumes:%v", err)
		return false
	}
	return name == strings.TrimSpace(out)
}

func extendLVS(name string, size uint64, isBlock bool) (string, error) {
	vgName, err := getRelatedVG(name)
	if err != nil {
		return "", fmt.Errorf("unable to get related vg for lv %s: %w", name, err)
	}
	if !lvExists(vgName, name) {
		return "", fmt.Errorf("logical volume %s does not exist", name)
	}

	lvSize, err := getLVSize(name, vgName)
	if err != nil {
		return "", fmt.Errorf("unable to get size of lv %s: %w", name, err)
	}
	if lvSize == size {
		klog.Infof("logical volume %s already has the requested size %d", name, size)
		return "", nil
	}

	executor := cmd.NewExecutor()
	args := []string{"-L", fmt.Sprintf("%db", size)}
	if isBlock {
		args = append(args, "-n")
	} else {
		args = append(args, "-r")
	}
	args = append(args, fmt.Sprintf("%s/%s", vgName, name))
	klog.Infof("lvextend %s", args)
	out, err := executor.Execute("lvextend", args)
	return out, err
}

// RemoveLVS executes lvremove
func RemoveLVS(name string) (string, error) {
	vgName, err := getRelatedVG(name)
	if err != nil {
		return "", fmt.Errorf("unable to get related vg for lv %s: %w", name, err)
	}
	if !lvExists(vgName, name) {
		return fmt.Sprintf("logical volume %s does not exist. Assuming it has already been deleted.", name), nil
	}

	executor := cmd.NewExecutor()
	args := []string{"-q", "-y"}
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

	if !lvExists(vgName, srcVolName) {
		return "", fmt.Errorf("logical volume %s does not exist", srcVolName)
	}

	executor := cmd.NewExecutor()
	// Names starting "snapshot" are reserved for internal use by LVM
	// we patch new snapName as "lvm-<snapshotName>"
	// parameters: -s, -y, -a n, -n, snapshotName, (-L, volSize), vgName/srcVolName
	args := []string{"-s", "-y"}
	if !forClone {
		args = append(args, "-a", "n")
		snapshotName = fmt.Sprintf("lvm-%s", snapshotName)
	}
	args = append(args, "-n", snapshotName)
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

	executor := cmd.NewExecutor()
	// Names starting "snapshot" are reserved for internal use by LVM
	// we patch new snapName as "lvm-<snapshotName>"
	snapshotName = fmt.Sprintf("lvm-%s", snapshotName)
	args := []string{"-q", "-y"}
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
	_, err = RemoveLVS(targetThinPool)
	if err != nil {
		return fmt.Errorf("unable to remove thinpool: %w", err)
	}
	return nil
}

func pvCount(vgname string) (int, error) {
	executor := cmd.NewExecutor()
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
	executor := cmd.NewExecutor()
	// we would like to get the segtype, name as below:
	// thin thinvol01    <-- this is volume
	// thin-pool vg02-thinpool 1  <-- this is thin-pool
	args := []string{"--noheadings", "-o", "segtype,name,thin_count", vgName}
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
		if len(parts) != 3 {
			klog.Infof("Skip thin info: %s", line)
			continue
		}
		// confirm again, we only care about thin-pool
		// thinInfo: map[<thin pool>]:<thin count>
		if parts[0] == ThinPoolType {
			count, err := strconv.Atoi(parts[2])
			if err != nil {
				return nil, err
			}
			thinInfo[parts[1]] = count
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

func getRelatedVG(lvname string) (string, error) {
	executor := cmd.NewExecutor()
	// we would like to get the lvname, vgname as below:
	// pvc-2e08db0f-01d0-462a-9da7-7da06fefd206 vg01
	// pvc-b13348e4-3c0c-4757-b781-3e6485a16780 vg02
	out, err := executor.Execute("lvs", []string{"--noheadings", "-o", "lv_name,vg_name"})
	if err != nil {
		return "", fmt.Errorf("unable to list existing volumes:%v", err)
	}
	lines := strings.Split(out, "\n")
	lvVgPairs := make(map[string]string)
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			klog.Warningf("unexpected output from lvs: %s", line)
			continue
		}
		lvVgPairs[parts[0]] = parts[1]
	}

	if _, ok := lvVgPairs[lvname]; !ok {
		return "", fmt.Errorf("logical volume %s does not exist", lvname)
	}
	relatedVgName := lvVgPairs[lvname]

	return relatedVgName, nil
}

func getLVSize(lvName, vgName string) (uint64, error) {
	var err error
	if vgName == "" {
		vgName, err = getRelatedVG(lvName)
		if err != nil {
			return 0, fmt.Errorf("unable to get related vg for lv %s: %w", lvName, err)
		}
		if !lvExists(vgName, lvName) {
			return 0, fmt.Errorf("logical volume %s does not exist", lvName)
		}
	}
	executor := cmd.NewExecutor()
	targetLVName := fmt.Sprintf("%s/%s", vgName, lvName)
	// check current lv size
	args := []string{"--noheadings", "--unit", "b", "-o", "Size"}
	args = append(args, targetLVName)
	out, err := executor.Execute("lvs", args)
	if err != nil {
		return 0, fmt.Errorf("unable to get size of lv %s: %w", lvName, err)
	}
	lvSizeStr := strings.TrimSpace(out)
	lvSizeStr = strings.TrimSuffix(lvSizeStr, "B")
	klog.Infof("current size of lv %s is %s", lvName, lvSizeStr)
	lvSize, err := strconv.ParseUint(lvSizeStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unable to parse size of lv %s: %w", lvName, err)
	}
	return lvSize, nil
}

func genProvisionerPodContent(action, name, targetNode, hostWritePath, provisionerImage string, pullPolicy v1.PullPolicy, args []string) *v1.Pod {
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
					TerminationMessagePath: "/termination.log",
					ImagePullPolicy:        pullPolicy,
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
