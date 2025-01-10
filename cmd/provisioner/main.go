package main

import (
	"fmt"
	"os"

	"github.com/urfave/cli/v2"
	"k8s.io/klog/v2"
)

const (
	flagLVName             = "lvname"
	flagLVSize             = "lvsize"
	flagVGName             = "vgname"
	flagDevicesPattern     = "devices"
	flagLVMType            = "lvmtype"
	flagSnapName           = "snapname"
	flagSrcLVName          = "srclvname"
	flagSrcVGName          = "srcvgname"
	flagSrcType            = "srctype"
	createSnapshotForClone = true
	snapshotPrefix         = "lvm-snapshot-"
)

func cmdNotFound(_ *cli.Context, command string) {
	panic(fmt.Errorf("unrecognized command: %s", command))
}

func onUsageError(_ *cli.Context, _ error, _ bool) error {
	panic(fmt.Errorf("usage error, please check your command"))
}

func main() {
	p := cli.NewApp()
	p.Usage = "LVM Provisioner Pod"
	p.Commands = []*cli.Command{
		createLVCmd(),
		deleteLVCmd(),
		createSnapCmd(),
		deleteSnapCmd(),
		cloneLVCmd(),
	}
	p.CommandNotFound = cmdNotFound
	p.OnUsageError = onUsageError

	klog.Infof("starting csi-lvmplugin-provisioner")

	if err := p.Run(os.Args); err != nil {
		klog.Fatalf("Critical error: %v", err)
	}
}
