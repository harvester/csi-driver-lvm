package main

import (
	"fmt"
	"time"

	"github.com/urfave/cli/v2"
	"k8s.io/klog/v2"

	lvm "github.com/harvester/csi-driver-lvm/pkg/lvm"
)

func createSnapCmd() *cli.Command {
	return &cli.Command{
		Name: "createsnap",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  flagSnapName,
				Usage: "Required. Specify snapshot name.",
			},
			&cli.Int64Flag{
				Name:  flagLVSize,
				Usage: "Required. The size of the lv in MiB",
			},
			&cli.StringFlag{
				Name:  flagVGName,
				Usage: "Required. the name of the volumegroup",
			},
			&cli.StringFlag{
				Name:  flagLVName,
				Usage: "Required. Specify the logical volume name.",
			},
			&cli.StringFlag{
				Name:  flagLVMType,
				Usage: "Required. the type of the source lvm",
			},
		},
		Action: func(c *cli.Context) error {
			if err := createSnap(c); err != nil {
				klog.Fatalf("Error creating lv: %v", err)
				return err
			}
			return nil
		},
	}
}

func createSnap(c *cli.Context) error {
	lvName := c.String(flagLVName)
	if lvName == "" {
		return fmt.Errorf("invalid empty flag %v", flagLVName)
	}
	lvSize := c.Int64(flagLVSize)
	if lvSize == 0 {
		return fmt.Errorf("invalid empty flag %v", flagLVSize)
	}
	vgName := c.String(flagVGName)
	if vgName == "" {
		return fmt.Errorf("invalid empty flag %v", flagVGName)
	}
	snapName := c.String(flagSnapName)
	if snapName == "" {
		return fmt.Errorf("invalid empty flag %v", flagSnapName)
	}
	lvType := c.String(flagLVMType)
	if lvType == "" {
		return fmt.Errorf("invalid empty flag %v", flagLVMType)
	}

	klog.Infof("create snapshot: %s source size: %d source lv: %s/%s", snapName, lvSize, vgName, lvName)

	if !lvm.VgExists(vgName) {
		lvm.VgActivate()
		time.Sleep(1 * time.Second) // jitter
		if !lvm.VgExists(vgName) {
			return fmt.Errorf("vg %s does not exist, please check the corresponding VG is created", vgName)
		}
	}

	output, err := lvm.CreateSnapshot(snapName, lvName, vgName, lvSize, lvType, !createSnapshotForClone)
	if err != nil {
		return fmt.Errorf("unable to create Snapshot: %w output:%s", err, output)
	}
	klog.Infof("Snapshot: %s created, source lv: %s/%s, size: %v", snapName, vgName, lvName, lvSize)
	return nil
}
