package main

import (
	"fmt"

	"github.com/urfave/cli/v2"
	"k8s.io/klog/v2"

	"github.com/harvester/csi-driver-lvm/pkg/lvm"
)

func deleteLVCmd() *cli.Command {
	return &cli.Command{
		Name: "deletelv",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  flagLVName,
				Usage: "Required. Specify lv name.",
			},
			&cli.StringFlag{
				Name:  flagSrcVGName,
				Usage: "Required. Source VG name",
			},
			&cli.StringFlag{
				Name:  flagSrcType,
				Usage: "Required. Source type, can be either striped or dm-thin",
			},
		},
		Action: func(c *cli.Context) error {
			if err := deleteLV(c); err != nil {
				klog.Fatalf("Error deleting lv: %v", err)
				return err
			}
			return nil
		},
	}
}

func deleteLV(c *cli.Context) error {
	lvName := c.String(flagLVName)
	if lvName == "" {
		return fmt.Errorf("invalid empty flag %v", flagLVName)
	}
	vgName := c.String(flagSrcVGName)
	if vgName == "" {
		return fmt.Errorf("invalid empty flag %v", flagSrcVGName)
	}
	vgType := c.String(flagSrcType)
	if vgType == "" {
		return fmt.Errorf("invalid empty flag %v", flagSrcType)
	}

	klog.Infof("delete lv %s", lvName)

	output, err := lvm.RemoveLVS(lvName)
	if err != nil {
		return fmt.Errorf("unable to delete lv: %w output:%s", err, output)
	}
	if vgType == lvm.DmThinType {
		err := lvm.RemoveThinPool(vgName)
		if err != nil {
			return fmt.Errorf("unable to delete thinpool: %w output:%s", err, output)
		}
	}
	klog.Infof("lv %s is deleted", lvName)
	return nil
}
