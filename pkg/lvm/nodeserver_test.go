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

// NodeStageVolume is where an encrypted volume's dm-crypt mapping is opened, so
// it needs the vgName to resolve the backing LV and the node-stage secret to
// unlock it.
func TestNodeStageVolumeOpensEncryptedMapping(t *testing.T) {
	const volID = "unit-stage-encrypted"
	crypt := &fakeCryptExecutor{
		t: t,
		results: []cryptResult{
			{err: commandExitError{code: cryptExitNotLuks}}, // isLuks
			{}, // luksFormat
			{}, // luksOpen
		},
	}
	useFakeCryptExecutor(t, crypt)

	ns := newNodeServerForTest()
	if _, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volID,
		StagingTargetPath: filepath.Join(t.TempDir(), "staging"),
		VolumeCapability:  mountCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     map[string]string{"vgName": "vg", encryptedParam: "true"},
		Secrets:           map[string]string{cryptoKeyValue: "pw"},
	}); err != nil {
		t.Fatalf("NodeStageVolume failed: %v", err)
	}

	subcommands := make([]string, 0, len(crypt.calls))
	for _, call := range crypt.calls {
		subcommands = append(subcommands, call.args[0])
	}
	if want := []string{"isLuks", "luksFormat", "luksOpen"}; !reflect.DeepEqual(subcommands, want) {
		t.Fatalf("unexpected cryptsetup calls: want %#v, got %#v", want, subcommands)
	}
}

// An encrypted volume whose secret never reached the node must fail the stage
// rather than silently defer the problem to publish.
func TestNodeStageVolumeRequiresEncryptionSecret(t *testing.T) {
	useFakeCryptExecutor(t, &fakeCryptExecutor{t: t})

	ns := newNodeServerForTest()
	_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "unit-stage-no-secret",
		StagingTargetPath: filepath.Join(t.TempDir(), "staging"),
		VolumeCapability:  mountCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     map[string]string{"vgName": "vg", encryptedParam: "true"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

// A malformed "encrypted" attribute must stop the stage, not resolve to a
// plaintext mount.
func TestNodeStageVolumeRejectsMalformedEncryptedAttribute(t *testing.T) {
	useFakeCryptExecutor(t, &fakeCryptExecutor{t: t})

	ns := newNodeServerForTest()
	_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "unit-stage-bad-flag",
		StagingTargetPath: filepath.Join(t.TempDir(), "staging"),
		VolumeCapability:  mountCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     map[string]string{"vgName": "vg", encryptedParam: "ture"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

// A plain volume stages without touching cryptsetup beyond the stray-header
// probe that keeps a LUKS container from being mounted as a filesystem.
func TestNodeStageVolumeProbesPlainFilesystemVolume(t *testing.T) {
	crypt := &fakeCryptExecutor{
		t:       t,
		results: []cryptResult{{err: commandExitError{code: cryptExitNotLuks}}}, // isLuks
	}
	useFakeCryptExecutor(t, crypt)

	ns := newNodeServerForTest()
	if _, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "unit-stage-plain",
		StagingTargetPath: filepath.Join(t.TempDir(), "staging"),
		VolumeCapability:  mountCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     map[string]string{"vgName": "vg"},
	}); err != nil {
		t.Fatalf("NodeStageVolume failed: %v", err)
	}
	if len(crypt.calls) != 1 {
		t.Fatalf("expected only the isLuks probe, got %#v", crypt.calls)
	}
	assertCryptSubcommand(t, crypt.calls[0], "isLuks", "")
}

func TestNodeStageVolumeRejectsIncompleteRequests(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "staging")
	capability := mountCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	volumeContext := map[string]string{"vgName": "vg"}

	tests := []struct {
		name string
		req  *csi.NodeStageVolumeRequest
	}{
		{"no volume id", &csi.NodeStageVolumeRequest{StagingTargetPath: staging, VolumeCapability: capability, VolumeContext: volumeContext}},
		{"no staging path", &csi.NodeStageVolumeRequest{VolumeId: "v", VolumeCapability: capability, VolumeContext: volumeContext}},
		{"relative staging path", &csi.NodeStageVolumeRequest{VolumeId: "v", StagingTargetPath: "staging", VolumeCapability: capability, VolumeContext: volumeContext}},
		{"no capability", &csi.NodeStageVolumeRequest{VolumeId: "v", StagingTargetPath: staging, VolumeContext: volumeContext}},
		{"no vgName", &csi.NodeStageVolumeRequest{VolumeId: "v", StagingTargetPath: staging, VolumeCapability: capability}},
	}

	ns := newNodeServerForTest()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useFakeCryptExecutor(t, &fakeCryptExecutor{t: t})
			if _, err := ns.NodeStageVolume(context.Background(), tt.req); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
		})
	}
}

// The mapper belongs to the staging lifecycle: unpublishing one of several
// targets on a ReadWriteOnce volume must not tear it down, or luksClose would
// find it busy and fail an unpublish that has already removed its target.
func TestNodeUnpublishLeavesEncryptedMappingOpen(t *testing.T) {
	useFakeUnmountExecutor(t, &fakeUnmountExecutor{
		t:       t,
		results: []commandResult{{command: "umount", err: errors.New("umount: target is not mounted")}},
	})
	crypt := &fakeCryptExecutor{t: t} // no results: any cryptsetup call fails the test
	useFakeCryptExecutor(t, crypt)

	ns := newNodeServerForTest()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "unit-unpublish-encrypted",
		TargetPath: target,
	}); err != nil {
		t.Fatalf("NodeUnpublishVolume failed: %v", err)
	}
	if len(crypt.calls) != 0 {
		t.Fatalf("unpublish must not touch the dm-crypt mapping, got %#v", crypt.calls)
	}
}

// Unstage is the reference-count boundary, so that is where the mapping is
// closed. For a plain volume there is nothing to close and cryptsetup is never
// invoked.
func TestNodeUnstageVolumeClosesMapping(t *testing.T) {
	crypt := &fakeCryptExecutor{t: t} // no mapper node exists for this volume ID
	useFakeCryptExecutor(t, crypt)

	ns := newNodeServerForTest()
	if _, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "unit-unstage-plain",
		StagingTargetPath: filepath.Join(t.TempDir(), "staging"),
	}); err != nil {
		t.Fatalf("NodeUnstageVolume failed: %v", err)
	}
	if len(crypt.calls) != 0 {
		t.Fatalf("expected no cryptsetup calls for a plain volume, got %#v", crypt.calls)
	}

	if _, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		StagingTargetPath: filepath.Join(t.TempDir(), "staging"),
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for a missing volume ID, got %v", err)
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
	if _, err := bindMountLV("/dev/vg/volume", target, true); err != nil {
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
