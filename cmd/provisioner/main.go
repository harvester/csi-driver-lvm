package main

import (
	"fmt"
	"os"

	"github.com/harvester/csi-driver-lvm/pkg/lvm"
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
	flagCommandTimeout     = "command-timeout"
	flagThinPoolTimeout    = "thin-pool-create-timeout"
	createSnapshotForClone = true
	snapshotPrefix         = "lvm-snapshot-"
)

func onUsageError(_ *cli.Context, err error, _ bool) error {
	return fmt.Errorf("usage error: %w", err)
}

func main() {
	p := cli.NewApp()
	p.Usage = "LVM Provisioner Pod"
	p.Flags = []cli.Flag{
		&cli.DurationFlag{
			Name:  flagCommandTimeout,
			Value: lvm.DefaultHelperCommandTimeout,
			Usage: "Timeout for each LVM command",
		},
		&cli.DurationFlag{
			Name:  flagThinPoolTimeout,
			Value: lvm.DefaultThinPoolCreateTimeout,
			Usage: "Timeout for initial thin-pool creation",
		},
	}
	p.Before = func(c *cli.Context) error {
		if err := lvm.SetCommandTimeout(c.Duration(flagCommandTimeout)); err != nil {
			return err
		}
		return lvm.SetThinPoolCreateTimeout(c.Duration(flagThinPoolTimeout))
	}
	p.Commands = []*cli.Command{
		createLVCmd(),
		deleteLVCmd(),
		createSnapCmd(),
		deleteSnapCmd(),
		cloneLVCmd(),
	}
	p.OnUsageError = onUsageError

	klog.Infof("starting csi-lvmplugin-provisioner")

	if err := p.Run(os.Args); err != nil {
		klog.Fatalf("Critical error: %v", err)
	}
}
