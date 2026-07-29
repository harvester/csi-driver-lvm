package main

import (
	"fmt"

	"github.com/urfave/cli/v2"
	"k8s.io/klog/v2"

	lvm "github.com/harvester/csi-driver-lvm/pkg/lvm"
)

func deleteSnapCmd() *cli.Command {
	return &cli.Command{
		Name: "deletesnap",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  flagSnapName,
				Usage: "Required. Specify snapshot name.",
			},
			&cli.StringFlag{
				Name:  flagVGName,
				Usage: "Required. the name of the volumegroup",
			},
		},
		Action: func(c *cli.Context) error {
			if err := deleteSnap(c); err != nil {
				klog.Fatalf("Error creating lv: %v", err)
				return err
			}
			return nil
		},
	}
}

func deleteSnap(c *cli.Context) error {
	vgName := c.String(flagVGName)
	if vgName == "" {
		return fmt.Errorf("invalid empty flag %v", flagVGName)
	}
	snapName := c.String(flagSnapName)
	if snapName == "" {
		return fmt.Errorf("invalid empty flag %v", flagSnapName)
	}

	klog.Infof("delete snapshot: %s/%s", vgName, snapName)

	output, err := lvm.DeleteSnapshot(snapName, vgName)
	if err != nil {
		return fmt.Errorf("unable to delete snapshot: %w output:%s", err, output)
	}
	klog.Infof("Snapshot: %s/%s deleted", vgName, snapName)
	return nil
}
