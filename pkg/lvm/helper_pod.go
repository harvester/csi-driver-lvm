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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"
)

const (
	helperPodLabel       = "lvm.driver.harvesterhci.io/helper"
	helperResourceLabel  = "lvm.driver.harvesterhci.io/resource"
	helperOperationLabel = "lvm.driver.harvesterhci.io/operation"
	helperVGAnnotation   = "lvm.driver.harvesterhci.io/volume-group"
)

var (
	helperAdmissionMu sync.Mutex
	podPollInterval   = time.Second
)

type helperParams struct {
	name             string
	resource         string
	action           actionType
	nodeName         string
	vgName           string
	lvmType          string
	hostWritePath    string
	provisionerImage string
	pullPolicy       v1.PullPolicy
	config           HelperConfig
}

func (sa snapshotAction) helperParams() helperParams {
	return helperParams{
		name:             sa.snapshotName,
		resource:         "snapshot",
		action:           sa.action,
		nodeName:         sa.nodeName,
		vgName:           sa.vgName,
		hostWritePath:    sa.hostWritePath,
		provisionerImage: sa.provisionerImage,
		pullPolicy:       sa.pullPolicy,
		config:           sa.helperConfig,
	}
}

func (va volumeAction) helperParams() helperParams {
	return helperParams{
		name:             va.name,
		resource:         "volume",
		action:           va.action,
		nodeName:         va.nodeName,
		vgName:           va.vgName,
		lvmType:          va.lvmType,
		hostWritePath:    va.hostWritePath,
		provisionerImage: va.provisionerImage,
		pullPolicy:       va.pullPolicy,
		config:           va.helperConfig,
	}
}

func createSnapshotHelper(ctx context.Context, action snapshotAction) error {
	args, err := snapshotHelperArgs(action)
	if err != nil {
		return err
	}

	klog.Infof("starting snapshot helper with args: %s", args)
	params := action.helperParams()
	pods := action.kubeClient.CoreV1().Pods(action.namespace)
	if err := runHelper(ctx, pods, newHelperPod(params, args), params); err != nil {
		return err
	}

	klog.Infof(
		"snapshot %v has been %vd on %v",
		action.snapshotName,
		action.action,
		action.nodeName,
	)
	return nil
}

func snapshotHelperArgs(action snapshotAction) ([]string, error) {
	if action.snapshotName == "" || action.nodeName == "" {
		return nil, fmt.Errorf("invalid empty name or path or node")
	}
	if action.action == actionTypeCreate && action.srcVolName == "" {
		return nil, fmt.Errorf("createlv without srcVolName")
	}

	switch action.action {
	case actionTypeCreate:
		return []string{
			"createsnap",
			"--snapname", action.snapshotName,
			"--lvname", action.srcVolName,
			"--vgname", action.vgName,
			"--lvsize", fmt.Sprintf("%d", action.snapSize),
			"--lvmtype", action.lvType,
		}, nil
	case actionTypeDelete:
		return []string{
			"deletesnap",
			"--snapname", action.snapshotName,
			"--vgname", action.vgName,
		}, nil
	default:
		return nil, fmt.Errorf("invalid action %q", action.action)
	}
}

func createVolumeHelper(ctx context.Context, action volumeAction) error {
	args, err := volumeHelperArgs(action)
	if err != nil {
		return err
	}

	klog.Infof("starting volume helper with args: %s", args)
	params := action.helperParams()
	pods := action.kubeClient.CoreV1().Pods(action.namespace)
	if err := runHelper(ctx, pods, newHelperPod(params, args), params); err != nil {
		return err
	}

	klog.Infof(
		"volume %v has been %vd on %v",
		action.name,
		action.action,
		action.nodeName,
	)
	return nil
}

func volumeHelperArgs(action volumeAction) ([]string, error) {
	if action.name == "" || action.nodeName == "" {
		return nil, fmt.Errorf("invalid empty name or path or node")
	}
	if action.action == actionTypeCreate && action.lvmType == "" {
		return nil, fmt.Errorf("createlv without lvm type")
	}

	var args []string
	switch action.action {
	case actionTypeCreate:
		args = []string{
			"createlv",
			"--lvsize", fmt.Sprintf("%d", action.size),
			"--lvmtype", action.lvmType,
			"--vgname", action.vgName,
			"--lvname", action.name,
		}
	case actionTypeDelete:
		if action.srcInfo == nil {
			return nil, fmt.Errorf("deletelv without source volume information")
		}
		args = []string{
			"deletelv",
			"--srcvgname", action.srcInfo.srcVGName,
			"--srctype", action.srcInfo.srcType,
			"--lvname", action.name,
		}
	case actionTypeClone:
		if action.srcInfo == nil {
			return nil, fmt.Errorf("clonelv without source volume information")
		}
		args = []string{
			"clonelv",
			"--srclvname", action.srcInfo.srcLVName,
			"--srcvgname", action.srcInfo.srcVGName,
			"--srctype", action.srcInfo.srcType,
			"--lvsize", fmt.Sprintf("%d", action.size),
			"--vgname", action.vgName,
			"--lvmtype", action.lvmType,
			"--lvname", action.name,
		}
	default:
		return nil, fmt.Errorf("invalid action %q", action.action)
	}
	return args, nil
}

func runHelper(
	ctx context.Context,
	pods corev1.PodInterface,
	pod *v1.Pod,
	params helperParams,
) error {
	if err := ensureHelper(ctx, pods, pod, params.config.MaxActive); err != nil {
		return err
	}

	terminal, err := waitForHelper(
		ctx,
		pods,
		pod.Name,
		params.resource,
		params.action,
	)
	if !terminal {
		klog.Infof(
			"not deleting helper pod %s after wait ended without a terminal state: %v",
			pod.Name,
			err,
		)
		return err
	}

	deleteHelper(pods, pod.Name)
	return err
}

func ensureHelper(
	ctx context.Context,
	pods corev1.PodInterface,
	desired *v1.Pod,
	maxActive int,
) error {
	helperAdmissionMu.Lock()
	defer helperAdmissionMu.Unlock()

	existing, err := pods.Get(ctx, desired.Name, metav1.GetOptions{})
	if err == nil {
		return useExistingHelper(ctx, pods, desired, existing)
	}
	if !k8serror.IsNotFound(err) {
		return status.Errorf(
			codes.Unavailable,
			"failed to check for helper pod %q: %v",
			desired.Name,
			err,
		)
	}
	if err := admitHelper(ctx, pods, desired, maxActive); err != nil {
		return err
	}

	_, err = pods.Create(ctx, desired, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !k8serror.IsAlreadyExists(err) {
		return err
	}
	return reuseHelper(ctx, pods, desired)
}

func reuseHelper(
	ctx context.Context,
	pods corev1.PodInterface,
	desired *v1.Pod,
) error {
	existing, err := pods.Get(ctx, desired.Name, metav1.GetOptions{})
	if err != nil {
		return status.Errorf(
			codes.Unavailable,
			"failed to get helper pod %q: %v",
			desired.Name,
			err,
		)
	}
	return useExistingHelper(ctx, pods, desired, existing)
}

func useExistingHelper(
	ctx context.Context,
	pods corev1.PodInterface,
	desired, existing *v1.Pod,
) error {
	if existing == nil {
		return status.Errorf(
			codes.Unavailable,
			"Kubernetes API returned an empty helper pod %q",
			desired.Name,
		)
	}
	if existing.DeletionTimestamp != nil {
		return forceDeleteHelper(ctx, pods, existing.Name)
	}
	if apiequality.Semantic.DeepDerivative(desired.Spec, existing.Spec) {
		klog.Infof("reusing existing helper pod %s", desired.Name)
		return nil
	}
	return retireHelper(ctx, pods, existing)
}

func admitHelper(
	ctx context.Context,
	pods corev1.PodInterface,
	desired *v1.Pod,
	maxActive int,
) error {
	if maxActive <= 0 {
		return status.Error(codes.Internal, "maximum active helper limit is not configured")
	}
	list, err := pods.List(ctx, metav1.ListOptions{})
	if err != nil {
		return status.Errorf(codes.Unavailable, "failed to list LVM helpers: %v", err)
	}

	groups := helperVGs(desired)
	if len(groups) == 0 {
		return status.Errorf(codes.Internal, "helper pod %q has no volume group", desired.Name)
	}
	for _, vgName := range groups {
		active := countActiveHelpers(list.Items, desired, vgName)
		if active < maxActive {
			continue
		}
		return status.Errorf(
			codes.Aborted,
			"node %q volume group %q has %d active helpers; limit is %d",
			desired.Spec.NodeName,
			vgName,
			active,
			maxActive,
		)
	}
	return nil
}

func countActiveHelpers(pods []v1.Pod, desired *v1.Pod, vgName string) int {
	active := 0
	for i := range pods {
		pod := &pods[i]
		if pod.Name == desired.Name || pod.Spec.NodeName != desired.Spec.NodeName {
			continue
		}
		if activeHelper(pod) && helperUsesVG(pod, vgName) {
			active++
		}
	}
	return active
}

func activeHelper(pod *v1.Pod) bool {
	return isHelper(pod) && !podTerminal(pod)
}

func isHelper(pod *v1.Pod) bool {
	if pod.Labels[helperPodLabel] == "true" {
		return true
	}
	for _, container := range pod.Spec.Containers {
		if len(container.Command) == 0 {
			continue
		}
		if filepath.Base(container.Command[0]) == "csi-lvmplugin-provisioner" {
			return true
		}
	}
	return false
}

func helperUsesVG(pod *v1.Pod, vgName string) bool {
	for _, podVG := range helperVGs(pod) {
		if podVG == vgName {
			return true
		}
	}
	return false
}

func helperVGs(pod *v1.Pod) []string {
	groups := map[string]struct{}{}
	if vgName := pod.Annotations[helperVGAnnotation]; vgName != "" {
		groups[vgName] = struct{}{}
	}
	for _, container := range pod.Spec.Containers {
		addVGArgs(groups, container.Args)
	}

	result := make([]string, 0, len(groups))
	for vgName := range groups {
		result = append(result, vgName)
	}
	sort.Strings(result)
	return result
}

func addVGArgs(groups map[string]struct{}, args []string) {
	for index, arg := range args {
		if arg == "--vgname" || arg == "--srcvgname" {
			if index+1 < len(args) && args[index+1] != "" {
				groups[args[index+1]] = struct{}{}
			}
			continue
		}
		for _, prefix := range []string{"--vgname=", "--srcvgname="} {
			if value := strings.TrimPrefix(arg, prefix); value != arg && value != "" {
				groups[value] = struct{}{}
			}
		}
	}
}

func forceDeleteHelper(
	ctx context.Context,
	pods corev1.PodInterface,
	podName string,
) error {
	options := metav1.DeleteOptions{GracePeriodSeconds: new(int64(0))}
	if err := pods.Delete(ctx, podName, options); err != nil && !k8serror.IsNotFound(err) {
		return status.Errorf(
			codes.Unavailable,
			"failed to force-delete helper pod %q: %v",
			podName,
			err,
		)
	}
	return status.Errorf(codes.Unavailable, "force-deleted helper pod %q; retry", podName)
}

func retireHelper(
	ctx context.Context,
	pods corev1.PodInterface,
	pod *v1.Pod,
) error {
	if !podTerminal(pod) {
		return status.Errorf(
			codes.Unavailable,
			"helper pod %q belongs to another request and is still %q",
			pod.Name,
			valueOrUnknown(string(pod.Status.Phase)),
		)
	}
	if err := pods.Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil &&
		!k8serror.IsNotFound(err) {
		return status.Errorf(
			codes.Unavailable,
			"failed to delete stale helper pod %q: %v",
			pod.Name,
			err,
		)
	}
	return status.Errorf(codes.Unavailable, "removed stale helper pod %q; retry", pod.Name)
}

func podTerminal(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed
}

func waitForHelper(
	ctx context.Context,
	pods corev1.PodInterface,
	podName, resource string,
	action actionType,
) (bool, error) {
	for {
		pod, readErr := pods.Get(ctx, podName, metav1.GetOptions{})
		terminal, resultErr := helperResult(
			ctx,
			pod,
			readErr,
			podName,
			resource,
			action,
		)
		if terminal || resultErr != nil {
			return terminal, resultErr
		}
		if err := waitForRetry(ctx); err != nil {
			return false, err
		}
	}
}

func helperResult(
	ctx context.Context,
	pod *v1.Pod,
	readErr error,
	podName, resource string,
	action actionType,
) (bool, error) {
	if readErr != nil {
		if ctx.Err() != nil {
			return false, status.FromContextError(ctx.Err()).Err()
		}
		if k8serror.IsNotFound(readErr) {
			return false, status.Errorf(
				codes.Unavailable,
				"%s %s helper pod %q disappeared; retry",
				resource,
				action,
				podName,
			)
		}
		klog.Errorf("error reading helper pod: %v", readErr)
		return false, nil
	}
	if pod == nil {
		return false, status.Error(codes.Internal, "Kubernetes API returned an empty helper pod")
	}

	switch pod.Status.Phase {
	case v1.PodFailed:
		klog.Infof("helper pod %s terminated with failure", pod.Name)
		return true, helperFailure(pod, resource, action)
	case v1.PodSucceeded:
		klog.Infof("helper pod %s terminated successfully", pod.Name)
		return true, nil
	default:
		klog.Infof("helper pod %s status: %s", pod.Name, pod.Status.Phase)
		return false, nil
	}
}

func helperFailure(pod *v1.Pod, resource string, action actionType) error {
	details := make([]string, 0, len(pod.Status.ContainerStatuses)+1)
	if pod.Status.Reason != "" || pod.Status.Message != "" {
		details = append(details, fmt.Sprintf(
			"pod reason=%s message=%s",
			valueOrUnknown(pod.Status.Reason),
			valueOrUnknown(compactErrorMessage(pod.Status.Message)),
		))
	}
	for _, status := range pod.Status.ContainerStatuses {
		terminated := status.State.Terminated
		if terminated == nil {
			continue
		}
		detail := fmt.Sprintf(
			"container %s exited with code %d reason=%s",
			status.Name,
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

func deleteHelper(pods corev1.PodInterface, podName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pods.Delete(ctx, podName, metav1.DeleteOptions{}); err != nil &&
		!k8serror.IsNotFound(err) {
		klog.Errorf("unable to delete helper pod %s: %v", podName, err)
	}
}

func waitForRetry(ctx context.Context) error {
	timer := time.NewTimer(podPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-timer.C:
		return nil
	}
}

func newHelperPod(params helperParams, args []string) *v1.Pod {
	operation := helperOperation(params)
	return &v1.Pod{
		ObjectMeta: helperMetadata(params, operation),
		Spec:       helperSpec(params, operation, args),
	}
}

func helperOperation(params helperParams) string {
	prefix := "lvm"
	if params.resource == "snapshot" {
		prefix = "snap"
	}
	return fmt.Sprintf("%s-%s", prefix, params.action)
}

func helperMetadata(params helperParams, operation string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: operation + "-" + params.name,
		Labels: map[string]string{
			helperPodLabel:       "true",
			helperResourceLabel:  params.resource,
			helperOperationLabel: string(params.action),
		},
		Annotations: map[string]string{
			helperVGAnnotation: params.vgName,
		},
	}
}

func helperSpec(
	params helperParams,
	operation string,
	args []string,
) v1.PodSpec {
	deadline := durationSeconds(helperPodTimeout(params))
	gracePeriod := int64(10)
	return v1.PodSpec{
		AutomountServiceAccountToken:  new(false),
		RestartPolicy:                 v1.RestartPolicyNever,
		NodeName:                      params.nodeName,
		ActiveDeadlineSeconds:         &deadline,
		TerminationGracePeriodSeconds: &gracePeriod,
		Tolerations: []v1.Toleration{{
			Operator: v1.TolerationOpExists,
		}},
		Containers: []v1.Container{
			newHelperContainer(params, operation, args),
		},
		Volumes: helperVolumes(params.hostWritePath),
	}
}

func helperPodTimeout(params helperParams) time.Duration {
	if params.action == actionTypeCreate && params.lvmType == DmThinType {
		return params.config.ThinPoolPodTimeout
	}
	return params.config.PodTimeout
}

func durationSeconds(duration time.Duration) int64 {
	seconds := int64(duration / time.Second)
	if duration%time.Second != 0 {
		seconds++
	}
	return seconds
}

func newHelperContainer(
	params helperParams,
	operation string,
	args []string,
) v1.Container {
	privileged := true
	commandArgs := append(
		[]string{
			"--command-timeout=" + params.config.CommandTimeout.String(),
			"--thin-pool-create-timeout=" + params.config.ThinPoolCreateTimeout.String(),
		},
		args...,
	)
	return v1.Container{
		Name:                     "csi-lvmplugin-" + operation,
		Image:                    params.provisionerImage,
		Command:                  []string{"csi-lvmplugin-provisioner"},
		Args:                     commandArgs,
		VolumeMounts:             helperMounts(),
		TerminationMessagePath:   "/termination.log",
		TerminationMessagePolicy: v1.TerminationMessageFallbackToLogsOnError,
		ImagePullPolicy:          params.pullPolicy,
		SecurityContext: &v1.SecurityContext{
			Privileged: &privileged,
		},
	}
}

func helperMounts() []v1.VolumeMount {
	propagation := v1.MountPropagationBidirectional
	return []v1.VolumeMount{
		{
			Name:             "devices",
			MountPath:        "/dev",
			MountPropagation: &propagation,
		},
		{Name: "modules", MountPath: "/lib/modules"},
		{
			Name:             "lvmbackup",
			MountPath:        "/etc/lvm/backup",
			MountPropagation: &propagation,
		},
		{
			Name:             "lvmcache",
			MountPath:        "/etc/lvm/cache",
			MountPropagation: &propagation,
		},
		{
			Name:             "lvmlock",
			MountPath:        "/run/lock/lvm",
			MountPropagation: &propagation,
		},
		{Name: "host-lvm-conf", MountPath: "/etc/lvm/lvm.conf", ReadOnly: true},
		{Name: "host-run-udev", MountPath: "/run/udev", ReadOnly: true},
	}
}

func helperVolumes(hostWritePath string) []v1.Volume {
	directory := v1.HostPathDirectory
	createDirectory := v1.HostPathDirectoryOrCreate
	return []v1.Volume{
		hostVolume("devices", "/dev", &createDirectory),
		hostVolume("modules", "/lib/modules", &createDirectory),
		hostVolume(
			"lvmbackup",
			filepath.Join(hostWritePath, "backup"),
			&createDirectory,
		),
		hostVolume(
			"lvmcache",
			filepath.Join(hostWritePath, "cache"),
			&createDirectory,
		),
		hostVolume(
			"lvmlock",
			filepath.Join(hostWritePath, "lock"),
			&createDirectory,
		),
		hostVolume("host-lvm-conf", "/etc/lvm/lvm.conf", nil),
		hostVolume("host-run-udev", "/run/udev", &directory),
	}
}

func hostVolume(name, path string, pathType *v1.HostPathType) v1.Volume {
	return v1.Volume{
		Name: name,
		VolumeSource: v1.VolumeSource{
			HostPath: &v1.HostPathVolumeSource{
				Path: path,
				Type: pathType,
			},
		},
	}
}
