/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path"

	"github.com/harvester/csi-driver-lvm/pkg/lvm"
)

func init() {
	err := flag.Set("logtostderr", "true")
	if err != nil {
		log.Printf("unable to configure logging to stdout:%v\n", err)
	}
}

var (
	endpoint          = flag.String("endpoint", "unix://tmp/csi.sock", "CSI endpoint")
	hostWritePath     = flag.String("hostwritepath", "/etc/lvm", "host path where config, cache & backups will be written to")
	driverName        = flag.String("drivername", "harvester-csi-driver-lvm", "name of the driver")
	nodeID            = flag.String("nodeid", "", "node id")
	maxVolumesPerNode = flag.Int64("maxvolumespernode", 0, "limit of volumes per node")
	showVersion       = flag.Bool("version", false, "Show version.")
	namespace         = flag.String("namespace", "csi-lvm", "name of namespace")
	provisionerImage  = flag.String("provisionerimage", "metalstack/csi-lvmplugin-provisioner", "name of provisioner image")
	pullPolicy        = flag.String("pullpolicy", "ifnotpresent", "pull policy for provisioner image")

	// Set by the build process
	version = ""
)

func main() {
	flag.Parse()

	if *showVersion {
		baseName := path.Base(os.Args[0])
		fmt.Println(baseName, version)
		return
	}

	handle()
	os.Exit(0)
}

func handle() {
	driver, err := lvm.NewLvmDriver(*driverName, *nodeID, *endpoint, *hostWritePath, *maxVolumesPerNode, version, *namespace, *provisionerImage, *pullPolicy)
	if err != nil {
		fmt.Printf("Failed to initialize driver: %s\n", err.Error())
		os.Exit(1)
	}
	err = driver.Run()
	if err != nil {
		fmt.Printf("Failed to start driver: %s\n", err.Error())
		os.Exit(1)
	}
}
