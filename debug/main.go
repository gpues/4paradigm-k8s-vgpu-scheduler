package main

import (
	"fmt"
	"gitlab.com/nvidia/cloud-native/go-nvlib/pkg/nvlib/device"
	"gitlab.com/nvidia/cloud-native/go-nvlib/pkg/nvml"
	"k8s.io/klog/v2"
	"log"
)

func main() {
	nvmllib := nvml.New()

	ret := nvmllib.Init()
	if ret != nvml.SUCCESS {
		log.Fatalln(fmt.Sprintf("failed to initialize NVML: %v", ret))
	}
	defer func() {
		ret := nvmllib.Shutdown()
		if ret != nvml.SUCCESS {
			klog.Errorf("Error shutting down NVML: %v", ret)
		}
	}()

	devicelib := device.New(
		device.WithNvml(nvmllib),
	)
	devicelib.VisitMigProfiles(func(p device.MigProfile) error {
		info := p.GetInfo()
		if info.C != info.G {
			return nil
		}
		fmt.Println(p.String())
		return nil
	})

}
