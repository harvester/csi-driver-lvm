package lvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	cmd "github.com/harvester/go-common/command"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeUnmountExecutor struct {
	t       *testing.T
	timeout time.Duration
	results []commandResult
	calls   []commandCall
}

func (f *fakeUnmountExecutor) SetTimeout(timeout time.Duration) {
	f.timeout = timeout
}

func (f *fakeUnmountExecutor) Execute(command string, args []string) (string, error) {
	f.t.Helper()
	f.calls = append(f.calls, commandCall{command: command, args: append([]string(nil), args...)})
	if len(f.results) == 0 {
		f.t.Fatalf("unexpected command: %s %v", command, args)
	}
	result := f.results[0]
	f.results = f.results[1:]
	if result.command != command {
		f.t.Fatalf("expected command %q, got %q with args %v", result.command, command, args)
	}
	return result.output, result.err
}

func useFakeUnmountExecutor(t *testing.T, fake *fakeUnmountExecutor) {
	t.Helper()
	original := newUnmountExecutor
	newUnmountExecutor = func() timedCommandExecutor {
		return fake
	}
	t.Cleanup(func() {
		newUnmountExecutor = original
	})
}

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
	fake := &fakeUnmountExecutor{
		t: t,
		results: []commandResult{{
			command: "umount",
			err:     errors.New("umount: target is not mounted"),
		}},
	}
	useFakeUnmountExecutor(t, fake)

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
	fake := &fakeUnmountExecutor{
		t: t,
		results: []commandResult{
			{command: "umount", err: errors.New("unmount failed")},
			{command: "umount", err: errors.New("forced unmount failed")},
		},
	}
	useFakeUnmountExecutor(t, fake)

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

func TestUnmountTargetFallsBackToForcedUnmount(t *testing.T) {
	fake := &fakeUnmountExecutor{
		t: t,
		results: []commandResult{
			{command: "umount", err: errors.New("unmount failed")},
			{command: "umount"},
		},
	}
	useFakeUnmountExecutor(t, fake)

	if err := unmountTarget("/target"); err != nil {
		t.Fatalf("unmountTarget failed: %v", err)
	}
	want := []commandCall{
		{command: "umount", args: []string{"/target"}},
		{command: "umount", args: []string{"--force", "/target"}},
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("unexpected unmount calls: want %#v, got %#v", want, fake.calls)
	}
	if fake.timeout != unmountTimeout {
		t.Fatalf("unexpected unmount timeout: want %s, got %s", unmountTimeout, fake.timeout)
	}
}

func TestUnmountTargetFallsBackToLazyAfterForceTimeout(t *testing.T) {
	fake := &fakeUnmountExecutor{
		t: t,
		results: []commandResult{
			{command: "umount", err: errors.New("unmount failed")},
			{command: "umount", err: cmd.ErrCmdTimeout},
			{command: "umount"},
		},
	}
	useFakeUnmountExecutor(t, fake)

	if err := unmountTarget("/target"); err != nil {
		t.Fatalf("unmountTarget failed: %v", err)
	}
	want := []commandCall{
		{command: "umount", args: []string{"/target"}},
		{command: "umount", args: []string{"--force", "/target"}},
		{command: "umount", args: []string{"--force", "--lazy", "/target"}},
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("unexpected unmount calls: want %#v, got %#v", want, fake.calls)
	}
}

func TestUnmountTargetReturnsLazyUnmountFailure(t *testing.T) {
	fake := &fakeUnmountExecutor{
		t: t,
		results: []commandResult{
			{command: "umount", err: errors.New("unmount failed")},
			{command: "umount", err: cmd.ErrCmdTimeout},
			{command: "umount", err: errors.New("lazy unmount failed")},
		},
	}
	useFakeUnmountExecutor(t, fake)

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
