package main

import (
	"fmt"
	"time"

	"github.com/urfave/cli/v2"
	"k8s.io/klog/v2"

	lvm "github.com/harvester/csi-driver-lvm/pkg/lvm"
)

func createLVCmd() *cli.Command {
	return &cli.Command{
		Name: "createlv",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  flagLVName,
				Usage: "Required. Specify lv name.",
			},
			&cli.Uint64Flag{
				Name:  flagLVSize,
				Usage: "Required. The size of the lv in MiB",
			},
			&cli.StringFlag{
				Name:  flagVGName,
				Usage: "Required. the name of the volumegroup",
			},
			&cli.StringFlag{
				Name:  flagLVMType,
				Usage: "Required. type of lvs, can be either striped or mirrored",
			},
		},
		Action: func(c *cli.Context) error {
			if err := createLV(c); err != nil {
				klog.Fatalf("Error creating lv: %v", err)
				return err
			}
			return nil
		},
	}
}

func createLV(c *cli.Context) error {
	lvName := c.String(flagLVName)
	if lvName == "" {
		return fmt.Errorf("invalid empty flag %v", flagLVName)
	}
	lvSize := c.Uint64(flagLVSize)
	if lvSize == 0 {
		return fmt.Errorf("invalid empty flag %v", flagLVSize)
	}
	vgName := c.String(flagVGName)
	if vgName == "" {
		return fmt.Errorf("invalid empty flag %v", flagVGName)
	}
	lvmType := c.String(flagLVMType)
	if lvmType == "" {
		return fmt.Errorf("invalid empty flag %v", flagLVMType)
	}

	klog.Infof("create lv %s size:%d vg:%s type:%s", lvName, lvSize, vgName, lvmType)

	if !lvm.VgExists(vgName) {
		lvm.VgActivate()
		time.Sleep(1 * time.Second) // jitter
		if !lvm.VgExists(vgName) {
			return fmt.Errorf("vg %s does not exist, please check the corresponding VG is created", vgName)
		}
	}

	output, err := lvm.CreateLVS(vgName, lvName, lvSize, lvmType)
	if err != nil {
		return fmt.Errorf("unable to create lv: %w output:%s", err, output)
	}
	klog.Infof("lv: %s created, vg:%s size:%d type:%s", lvName, vgName, lvSize, lvmType)
	return nil
}
