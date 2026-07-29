package lvm

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNodePublishRejectsUnsupportedAccessMode(t *testing.T) {
	ns := newNodeServerForTest()
	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:   "volume",
		TargetPath: filepath.Join(t.TempDir(), "target"),
		VolumeContext: map[string]string{
			"vgName": "vg",
		},
		VolumeCapability: mountCapability(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestNodeUnpublishRemovesTarget(t *testing.T) {
	original := unmountPath
	unmountPath = func(_ string, _ int) error {
		return unix.EINVAL
	}
	t.Cleanup(func() {
		unmountPath = original
	})

	ns := newNodeServerForTest()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "volume",
		TargetPath: target,
	}); err != nil {
		t.Fatalf("NodeUnpublishVolume failed: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("expected target to be removed, stat error: %v", err)
	}
}

func TestNodeUnpublishReturnsUnmountFailure(t *testing.T) {
	original := unmountPath
	unmountPath = func(_ string, _ int) error {
		return unix.EPERM
	}
	t.Cleanup(func() {
		unmountPath = original
	})

	ns := newNodeServerForTest()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}

	_, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "volume",
		TargetPath: target,
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatalf("target should remain after failed unmount: %v", statErr)
	}
}

func TestUnmountTargetFallsBackToLazyUnmountWhenBusy(t *testing.T) {
	original := unmountPath
	var flags []int
	unmountPath = func(_ string, unmountFlags int) error {
		flags = append(flags, unmountFlags)
		if unmountFlags == 0 {
			return unix.EBUSY
		}
		return nil
	}
	t.Cleanup(func() {
		unmountPath = original
	})

	if err := unmountTarget("/target"); err != nil {
		t.Fatalf("unmountTarget failed: %v", err)
	}
	want := []int{0, unix.MNT_DETACH}
	if !reflect.DeepEqual(flags, want) {
		t.Fatalf("unexpected unmount flags: want %#v, got %#v", want, flags)
	}
}

func TestUnmountTargetReturnsLazyUnmountFailure(t *testing.T) {
	original := unmountPath
	unmountPath = func(_ string, unmountFlags int) error {
		if unmountFlags == 0 {
			return unix.EBUSY
		}
		return unix.EPERM
	}
	t.Cleanup(func() {
		unmountPath = original
	})

	if err := unmountTarget("/target"); err == nil {
		t.Fatal("expected lazy unmount failure")
	}
}

func TestIsBlockVolumePath(t *testing.T) {
	t.Run("directory is filesystem volume", func(t *testing.T) {
		isBlock, err := isBlockVolumePath(t.TempDir())
		if err != nil || isBlock {
			t.Fatalf("expected filesystem volume, got isBlock=%t err=%v", isBlock, err)
		}
	})

	t.Run("file is block volume", func(t *testing.T) {
		volumePath := filepath.Join(t.TempDir(), "volume")
		if err := os.WriteFile(volumePath, nil, 0600); err != nil {
			t.Fatal(err)
		}

		isBlock, err := isBlockVolumePath(volumePath)
		if err != nil || !isBlock {
			t.Fatalf("expected block volume, got isBlock=%t err=%v", isBlock, err)
		}
	})

	t.Run("missing path fails", func(t *testing.T) {
		if _, err := isBlockVolumePath(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatal("expected missing path to fail")
		}
	})
}

func TestBindMountReadOnlyUsesRemount(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{
			{command: "mount"},
			{command: "mount"},
		},
	}
	useFakeCommandExecutor(t, fake)

	target := filepath.Join(t.TempDir(), "target")
	if _, err := bindMountLV("volume", target, "vg", true); err != nil {
		t.Fatalf("bindMountLV failed: %v", err)
	}

	want := []string{"-o", "remount,bind,ro", target}
	if got := fake.calls[1].args; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected readonly remount arguments: want %#v, got %#v", want, got)
	}
}

func TestPrepareBindMountTargetDoesNotChangeExistingTargetPermissions(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}

	if err := prepareBindMountTarget("volume", target); err != nil {
		t.Fatalf("prepareBindMountTarget failed: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("existing target permissions changed: got %o", got)
	}
}

func TestPrepareBindMountTargetRejectsDirectory(t *testing.T) {
	if err := prepareBindMountTarget("volume", t.TempDir()); err == nil {
		t.Fatal("expected directory target to fail")
	}
}

func newNodeServerForTest() *nodeServer {
	return &nodeServer{nodeID: "node-a"}
}
