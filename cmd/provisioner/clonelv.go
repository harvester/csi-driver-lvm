package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"
	"k8s.io/klog/v2"

	lvm "github.com/harvester/csi-driver-lvm/pkg/lvm"
)

func cloneLVCmd() *cli.Command {
	return &cli.Command{
		Name: "clonelv",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  flagSrcLVName,
				Usage: "Required. Source LV name.",
			},
			&cli.StringFlag{
				Name:  flagSrcVGName,
				Usage: "Required. Source VG name",
			},
			&cli.StringFlag{
				Name:  flagSrcType,
				Usage: "Required. Source type, can be either striped or dm-thin",
			},
			&cli.StringFlag{
				Name:  flagLVName,
				Usage: "Required. Target LV",
			},
			&cli.StringFlag{
				Name:  flagVGName,
				Usage: "Required. the name of the volumegroup",
			},
			&cli.StringFlag{
				Name:  flagLVMType,
				Usage: "Required. type of lvs, can be either striped or dm-thin",
			},
			&cli.Uint64Flag{
				Name:  flagLVSize,
				Usage: "Required. The size of the lv in MiB",
			},
		},
		Action: func(c *cli.Context) error {
			if err := clonelv(c); err != nil {
				klog.Fatalf("Error creating lv: %v", err)
				return err
			}
			return nil
		},
	}
}

func clonelv(c *cli.Context) error {
	srcLvName := c.String(flagSrcLVName)
	if srcLvName == "" {
		return fmt.Errorf("invalid empty flag %v", flagSrcLVName)
	}
	srcVgName := c.String(flagSrcVGName)
	if srcVgName == "" {
		return fmt.Errorf("invalid empty flag %v", flagSrcVGName)
	}
	srcType := c.String(flagSrcType)
	if srcType == "" {
		return fmt.Errorf("invalid empty flag %v", flagSrcType)
	}
	dstLV := c.String(flagLVName)
	if dstLV == "" {
		return fmt.Errorf("invalid empty flag %v", flagLVName)
	}
	dstVGName := c.String(flagVGName)
	if dstVGName == "" {
		return fmt.Errorf("invalid empty flag %v", flagVGName)
	}
	dstLVType := c.String(flagLVMType)
	if dstLVType == "" {
		return fmt.Errorf("invalid empty flag %v", flagLVMType)
	}
	dstSize := c.Uint64(flagLVSize)
	if dstSize == 0 {
		return fmt.Errorf("invalid empty flag %v", flagLVSize)
	}

	klog.Infof("Clone from src: %s, to dst: %s/%s", srcLvName, dstVGName, dstLV)

	if !lvm.VgExists(dstVGName) {
		lvm.VgActivate()
		time.Sleep(1 * time.Second) // jitter
		if !lvm.VgExists(dstVGName) {
			return fmt.Errorf("vg %s does not exist, please check the corresponding VG is created", dstVGName)
		}
	}

	klog.Infof("clone lv %s, vg: %s, type: %s", srcLvName, srcVgName, srcType)
	if strings.HasPrefix(srcLvName, snapshotPrefix) && srcType == lvm.DmThinType && srcType == dstLVType && srcVgName == dstVGName {
		// special case for clone dm-thin, so activate the target
		_, err := lvm.CreateSnapshot(dstLV, srcLvName, srcVgName, int64(dstSize), dstLVType, createSnapshotForClone) //nolint:gosec
		if err != nil {
			return fmt.Errorf("unable to create snapshot: %w", err)
		}
		klog.Infof("lv: %s/%s cloned from %s", dstVGName, dstLV, srcLvName)
		return nil
	}

	// check source dev
	srcDev := fmt.Sprintf("/dev/%s/%s", srcVgName, srcLvName)
	src, err := os.OpenFile(srcDev, syscall.O_RDONLY|syscall.O_DIRECT, 0)
	if err != nil {
		return fmt.Errorf("unable to open source device: %w", err)
	}
	defer src.Close()

	output, err := lvm.CreateLVS(dstVGName, dstLV, dstSize, dstLVType)
	if err != nil {
		return fmt.Errorf("unable to create lv: %w output:%s", err, output)
	}
	klog.Infof("lv: %s created, vg:%s size:%d type:%s", dstLV, dstVGName, dstSize, dstLVType)

	dst, err := os.OpenFile(fmt.Sprintf("/dev/%s/%s", dstVGName, dstLV), syscall.O_WRONLY|syscall.O_DIRECT, 0)
	if err != nil {
		return fmt.Errorf("unable to open target device: %w", err)
	}
	defer dst.Close()

	// Clone the source device to the target device
	if err := lvm.CloneDevice(src, dst); err != nil {
		return fmt.Errorf("unable to clone device: %w", err)
	}
	if err := dst.Sync(); err != nil {
		return fmt.Errorf("unable to sync target device: %w", err)
	}

	klog.Infof("lv: %s/%s cloned from %s", dstVGName, dstLV, srcDev)

	return nil
}
