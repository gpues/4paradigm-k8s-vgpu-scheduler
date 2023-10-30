package main

import (
	"fmt"
	v1 "github.com/NVIDIA/k8s-device-plugin/api/config/v1"
	"gopkg.in/yaml.v2"
)

func main() {
	a := v1.ReplicatedResource{
		Name:     "a",
		Rename:   "x",
		Devices:  v1.ReplicatedDevices{All: false},
		Replicas: 0,
	}
	out, err := yaml.Marshal(a)
	fmt.Println(string(out))
	fmt.Println(err)
}
