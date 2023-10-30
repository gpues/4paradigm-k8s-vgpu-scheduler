/*
 * Copyright (c) 2019, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package plugin

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"4pd.io/k8s-vgpu/pkg/api"
	"4pd.io/k8s-vgpu/pkg/device"
	"4pd.io/k8s-vgpu/pkg/device-plugin/nvidiadevice/nvinternal/cdi"
	"4pd.io/k8s-vgpu/pkg/device-plugin/nvidiadevice/nvinternal/rm"
	"4pd.io/k8s-vgpu/pkg/device/nvidia"
	"4pd.io/k8s-vgpu/pkg/util"
	"4pd.io/k8s-vgpu/pkg/util/nodelock"
	spec "github.com/NVIDIA/k8s-device-plugin/api/config/v1"
	cdiapi "github.com/container-orchestrated-devices/container-device-interface/pkg/cdi"

	"github.com/google/uuid"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// Constants for use by the 'volume-mounts' device list strategy
const (
	deviceListAsVolumeMountsHostPath          = "/dev/null"
	deviceListAsVolumeMountsContainerPathRoot = "/var/run/nvidia-container-devices"
)

// NvidiaDevicePlugin implements the Kubernetes device plugin API
type NvidiaDevicePlugin struct {
	rm                   rm.ResourceManager
	config               *util.DeviceConfig
	deviceListEnvvar     string
	deviceListStrategies spec.DeviceListStrategies
	socket               string

	cdiHandler          cdi.Interface
	cdiEnabled          bool
	cdiAnnotationPrefix string

	server *grpc.Server
	health chan *rm.Device
	stop   chan interface{}
}

// Allocate which return list of devices.
func (plugin *NvidiaDevicePlugin) Allocate(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	klog.Infoln("Allocate", reqs.ContainerRequests)
	responses := pluginapi.AllocateResponse{}
	nodename := os.Getenv("NodeName")
	current, err := util.GetPendingPod(nodename)
	if err != nil {
		nodelock.ReleaseNodeLock(nodename)
		return &pluginapi.AllocateResponse{}, err
	}

	for idx, req := range reqs.ContainerRequests {
		// If the devices being allocated are replicas, then (conditionally)
		// error out if more than one resource is being allocated.

		if strings.Contains(req.DevicesIDs[0], "MIG") {

			if plugin.config.Sharing.TimeSlicing.FailRequestsGreaterThanOne && rm.AnnotatedIDs(req.DevicesIDs).AnyHasAnnotations() {
				if len(req.DevicesIDs) > 1 {
					return nil, fmt.Errorf("request for '%v: %v' too large: maximum request size for shared resources is 1", plugin.rm.Resource(), len(req.DevicesIDs))
				}
			}

			for _, id := range req.DevicesIDs {
				if !plugin.rm.Devices().Contains(id) {
					return nil, fmt.Errorf("invalid allocation request for '%s': unknown device: %s", plugin.rm.Resource(), id)
				}
			}

			response, err := plugin.getAllocateResponse(req.DevicesIDs)
			if err != nil {
				return nil, fmt.Errorf("failed to get allocate response: %v", err)
			}
			responses.ContainerResponses = append(responses.ContainerResponses, response)
		} else {
			currentCtr, devreq, err := util.GetNextDeviceRequest(nvidia.NvidiaGPUDevice, *current)
			// 哪个容器要分配gpu, 要分配的资源量
			klog.Infoln("deviceAllocateFromAnnotation=", devreq)
			if err != nil {
				device.PodAllocationFailed(nodename, current)
				return &pluginapi.AllocateResponse{}, err
			}
			if len(devreq) != len(reqs.ContainerRequests[idx].DevicesIDs) {
				device.PodAllocationFailed(nodename, current)
				return &pluginapi.AllocateResponse{}, errors.New("device number not matched")
			}
			response, err := plugin.getAllocateResponse(util.GetContainerDeviceStrArray(devreq)) // 返回声明、env、devices
			if err != nil {
				return nil, fmt.Errorf("failed to get allocate response: %v", err)
			}

			err = util.EraseNextDeviceTypeFromAnnotation(nvidia.NvidiaGPUDevice, *current) // 清除声明中第一个要匹配的资源
			if err != nil {
				device.PodAllocationFailed(nodename, current)
				return &pluginapi.AllocateResponse{}, err
			}

			for i, dev := range devreq {
				limitKey := fmt.Sprintf("CUDA_DEVICE_MEMORY_LIMIT_%v", i)
				response.Envs[limitKey] = fmt.Sprintf("%vm", dev.Usedmem)

				/*tmp := response.Envs["NVIDIA_VISIBLE_DEVICES"]
				if i > 0 {
					response.Envs["NVIDIA_VISIBLE_DEVICES"] = fmt.Sprintf("%v,%v", tmp, dev.UUID)
				} else {
					response.Envs["NVIDIA_VISIBLE_DEVICES"] = dev.UUID
				}*/
			}
			response.Envs["CUDA_DEVICE_SM_LIMIT"] = fmt.Sprint(devreq[0].Usedcores)
			response.Envs["CUDA_DEVICE_MEMORY_SHARED_CACHE"] = fmt.Sprintf("/usr/local/vgpu/%v.cache", uuid.New().String())
			if *util.DeviceMemoryScaling > 1 {
				response.Envs["CUDA_OVERSUBSCRIBE"] = "true"
			}
			if *util.DisableCoreLimit {
				response.Envs[api.CoreLimitSwitch] = "disable"
			}
			cacheFileHostDirectory := "/usr/local/vgpu/containers/" + string(current.UID) + "_" + currentCtr.Name
			os.MkdirAll(cacheFileHostDirectory, 0777)
			os.Chmod(cacheFileHostDirectory, 0777)
			os.MkdirAll("/tmp/vgpulock", 0777)
			os.Chmod("/tmp/vgpulock", 0777)
			hostHookPath := os.Getenv("HOOK_PATH")
			response.Mounts = append(response.Mounts,
				&pluginapi.Mount{ContainerPath: "/usr/local/vgpu/libvgpu.so",
					HostPath: hostHookPath + "/libvgpu.so",
					ReadOnly: true},
				&pluginapi.Mount{ContainerPath: "/usr/local/vgpu",
					HostPath: cacheFileHostDirectory,
					ReadOnly: false},
				&pluginapi.Mount{ContainerPath: "/tmp/vgpulock",
					HostPath: "/tmp/vgpulock",
					ReadOnly: false},
			)
			found := false
			for _, val := range currentCtr.Env {
				if strings.Compare(val.Name, "CUDA_DISABLE_CONTROL") == 0 {
					found = true
					break
				}
			}
			if !found {
				response.Mounts = append(response.Mounts, &pluginapi.Mount{ContainerPath: "/etc/ld.so.preload",
					HostPath: hostHookPath + "/ld.so.preload",
					ReadOnly: true},
				)
			}
			_, err = os.Stat("/usr/local/vgpu/license")
			if err == nil {
				response.Mounts = append(response.Mounts, &pluginapi.Mount{
					ContainerPath: "/vgpu/",
					HostPath:      "/usr/local/vgpu/license",
					ReadOnly:      true,
				})
				response.Mounts = append(response.Mounts, &pluginapi.Mount{
					ContainerPath: "/usr/bin/vgpuvalidator",
					HostPath:      hostHookPath + "/vgpuvalidator",
					ReadOnly:      true,
				})
			}
			responses.ContainerResponses = append(responses.ContainerResponses, response)
		}
	}
	klog.Infoln("Allocate Response", responses.ContainerResponses)
	device.PodAllocationTrySuccess(nodename, current)
	return &responses, nil
}

/*
func (plugin *NvidiaDevicePlugin) apiMounts(deviceIDs []string) []*pluginapi.Mount {
	var mounts []*pluginapi.Mount

	for _, id := range deviceIDs {
		mount := &pluginapi.Mount{
			HostPath:      deviceListAsVolumeMountsHostPath,
			ContainerPath: filepath.Join(deviceListAsVolumeMountsContainerPathRoot, id),
		}
		mounts = append(mounts, mount)
	}

	return mounts
}*/

func (plugin *NvidiaDevicePlugin) apiDeviceSpecs(driverRoot string, ids []string) []*pluginapi.DeviceSpec {
	optional := map[string]bool{
		"/dev/nvidiactl":        true,
		"/dev/nvidia-uvm":       true,
		"/dev/nvidia-uvm-tools": true,
		"/dev/nvidia-modeset":   true,
	}

	paths := plugin.rm.GetDevicePaths(ids)

	var specs []*pluginapi.DeviceSpec
	for _, p := range paths {
		if optional[p] {
			if _, err := os.Stat(p); err != nil {
				continue
			}
		}
		spec := &pluginapi.DeviceSpec{
			ContainerPath: p,
			HostPath:      filepath.Join(driverRoot, p),
			Permissions:   "rw",
		}
		specs = append(specs, spec)
	}

	return specs
}

// NewNvidiaDevicePlugin returns an initialized NvidiaDevicePlugin
func NewNvidiaDevicePlugin(config *util.DeviceConfig, resourceManager rm.ResourceManager, cdiHandler cdi.Interface, cdiEnabled bool) *NvidiaDevicePlugin {
	_, name := resourceManager.Resource().Split()

	deviceListStrategies, _ := spec.NewDeviceListStrategies(*config.Flags.Plugin.DeviceListStrategy)

	return &NvidiaDevicePlugin{
		rm:                   resourceManager,
		config:               config,
		deviceListEnvvar:     "NVIDIA_VISIBLE_DEVICES",
		deviceListStrategies: deviceListStrategies,
		socket:               pluginapi.DevicePluginPath + "nvidia-" + name + ".sock",
		cdiHandler:           cdiHandler,
		cdiEnabled:           cdiEnabled,
		cdiAnnotationPrefix:  *config.Flags.Plugin.CDIAnnotationPrefix,

		// These will be reinitialized every
		// time the plugin server is restarted.
		server: nil,
		health: nil,
		stop:   nil,
	}
}

func (plugin *NvidiaDevicePlugin) initialize() {
	plugin.server = grpc.NewServer([]grpc.ServerOption{}...)
	plugin.health = make(chan *rm.Device)
	plugin.stop = make(chan interface{})
}

// Serve starts the gRPC server of the device plugin.
func (plugin *NvidiaDevicePlugin) Serve() error {
	os.Remove(plugin.socket)
	sock, err := net.Listen("unix", plugin.socket)
	if err != nil {
		return err
	}

	pluginapi.RegisterDevicePluginServer(plugin.server, plugin)

	go func() {
		lastCrashTime := time.Now()
		restartCount := 0
		for {
			klog.Infof("Starting GRPC server for '%s'", plugin.rm.Resource())
			err := plugin.server.Serve(sock)
			if err == nil {
				break
			}

			klog.Infof("GRPC server for '%s' crashed with error: %v", plugin.rm.Resource(), err)

			// restart if it has not been too often
			// i.e. if server has crashed more than 5 times and it didn't last more than one hour each time
			if restartCount > 5 {
				// quit
				klog.Fatalf("GRPC server for '%s' has repeatedly crashed recently. Quitting", plugin.rm.Resource())
			}
			timeSinceLastCrash := time.Since(lastCrashTime).Seconds()
			lastCrashTime = time.Now()
			if timeSinceLastCrash > 3600 {
				// it has been one hour since the last crash.. reset the count
				// to reflect on the frequency
				restartCount = 1
			} else {
				restartCount++
			}
		}
	}()

	// Wait for server to start by launching a blocking connexion
	conn, err := plugin.dial(plugin.socket, 5*time.Second)
	if err != nil {
		return err
	}
	conn.Close()

	return nil
}

// Register registers the device plugin for the given resourceName with Kubelet.
func (plugin *NvidiaDevicePlugin) Register() error {
	conn, err := plugin.dial(pluginapi.KubeletSocket, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	reqt := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(plugin.socket),
		ResourceName: string(plugin.rm.Resource()),
		Options: &pluginapi.DevicePluginOptions{
			GetPreferredAllocationAvailable: true, // 获得可用的优先分配
		},
	}

	_, err = client.Register(context.Background(), reqt)
	if err != nil {
		return err
	}
	return nil
}

// Stop stops the gRPC server.
func (plugin *NvidiaDevicePlugin) Stop() error {
	if plugin == nil || plugin.server == nil {
		return nil
	}
	klog.Infof("Stopping to serve '%s' on %s", plugin.rm.Resource(), plugin.socket)
	plugin.server.Stop()
	if err := os.Remove(plugin.socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	plugin.cleanup()
	return nil
}

// Start starts the gRPC server, registers the device plugin with the Kubelet,
// and starts the device healthchecks.
func (plugin *NvidiaDevicePlugin) Start() error { // nvidia.com/gpu
	plugin.initialize()

	err := plugin.Serve()
	if err != nil {
		klog.Infof("Could not start device plugin for '%s': %s", plugin.rm.Resource(), err)
		plugin.cleanup()
		return err
	}
	klog.Infof("Starting to serve '%s' on %s", plugin.rm.Resource(), plugin.socket)

	err = plugin.Register()
	if err != nil {
		klog.Infof("Could not register device plugin: %s", err)
		plugin.Stop()
		return err
	}
	klog.Infof("Registered device plugin for '%s' with Kubelet", plugin.rm.Resource())

	go func() {
		err := plugin.rm.CheckHealth(plugin.stop, plugin.health)
		if err != nil {
			klog.Infof("Failed to start health check: %v; continuing with health checks disabled", err)
		}
	}()

	go func() {
		plugin.WatchAndRegister()
	}()

	return nil
}

func (plugin *NvidiaDevicePlugin) cleanup() {
	close(plugin.stop)
	plugin.server = nil
	plugin.health = nil
	plugin.stop = nil
}

// Devices returns the full set of devices associated with the plugin.
func (plugin *NvidiaDevicePlugin) Devices() rm.Devices {
	return plugin.rm.Devices()
}

// ListAndWatch lists devices and update that list according to the health status
func (plugin *NvidiaDevicePlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	s.Send(&pluginapi.ListAndWatchResponse{Devices: plugin.apiDevices()})

	for {
		select {
		case <-plugin.stop:
			return nil
		case d := <-plugin.health:
			// FIXME: there is no way to recover from the Unhealthy state.
			d.Health = pluginapi.Unhealthy
			klog.Infof("'%s' device marked unhealthy: %s", plugin.rm.Resource(), d.ID)
			s.Send(&pluginapi.ListAndWatchResponse{Devices: plugin.apiDevices()})
		}
	}
}

// dial establishes the gRPC communication with the registered device plugin.
func (plugin *NvidiaDevicePlugin) dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	c, err := grpc.Dial(unixSocketPath, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithTimeout(timeout),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}),
	)

	if err != nil {
		return nil, err
	}

	return c, nil
}
func (plugin *NvidiaDevicePlugin) apiEnvs(envvar string, deviceIDs []string) map[string]string {
	return map[string]string{
		envvar: strings.Join(deviceIDs, ","),
	}
}

// GetDevicePluginOptions returns the values of the optional settings for this plugin
// 指示是否实现了GetPreferredAllocation并可用于调用
func (plugin *NvidiaDevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	options := &pluginapi.DevicePluginOptions{
		GetPreferredAllocationAvailable: true,
	}
	return options, nil
}

// GetPreferredAllocation returns the preferred allocation from the set of devices specified in the request
func (plugin *NvidiaDevicePlugin) GetPreferredAllocation(ctx context.Context, r *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	response := &pluginapi.PreferredAllocationResponse{}
	/*for _, req := range r.ContainerRequests {
		devices, err := plugin.rm.GetPreferredAllocation(req.AvailableDeviceIDs, req.MustIncludeDeviceIDs, int(req.AllocationSize))
		if err != nil {
			return nil, fmt.Errorf("error getting list of preferred allocation devices: %v", err)
		}

		resp := &pluginapi.ContainerPreferredAllocationResponse{
			DeviceIDs: devices,
		}

		response.ContainerResponses = append(response.ContainerResponses, resp)
	}*/
	return response, nil
}

// 返回声明、env、devices
func (plugin *NvidiaDevicePlugin) getAllocateResponse(requestIds []string) (*pluginapi.ContainerAllocateResponse, error) {
	deviceIDs := plugin.deviceIDsFromAnnotatedDeviceIDs(requestIds)
	// GPU-b0a0a881-3b2a-bc6b-9315-beb717e4c4e4::1  -> GPU-b0a0a881-3b2a-bc6b-9315-beb717e4c4e4

	responseID := uuid.New().String()
	response, err := plugin.getAllocateResponseForCDI(responseID, deviceIDs) // 将deviceIDs 存储在声明里的cdi 位置
	if err != nil {
		return nil, fmt.Errorf("failed to get allocate response for CDI: %v", err)
	}

	response.Envs = plugin.apiEnvs(plugin.deviceListEnvvar, deviceIDs) // 返回 NVIDIA_VISIBLE_DEVICES 环境变量
	//if plugin.deviceListStrategies.Includes(spec.DeviceListStrategyVolumeMounts) || plugin.deviceListStrategies.Includes(spec.DeviceListStrategyEnvvar) {
	//	response.Envs = plugin.apiEnvs(plugin.deviceListEnvvar, deviceIDs)
	//}
	/*
		if plugin.deviceListStrategies.Includes(spec.DeviceListStrategyVolumeMounts) {
			response.Envs = plugin.apiEnvs(plugin.deviceListEnvvar, []string{deviceListAsVolumeMountsContainerPathRoot})
			response.Mounts = plugin.apiMounts(deviceIDs)
		}*/
	if *plugin.config.Flags.Plugin.PassDeviceSpecs { // GPU 透传
		_ = `docker run --net host \
  --device=/dev/nvidiactl \
  --device=/dev/nvidia-uvm \
  --device=/dev/nvidia0 \
  -v /var/lib/nvidia-docker/volumes/nvidia_driver/$NVIDIA_DRIVER_VERSION/:/usr/local/nvidia/ \
  -it nvidia/cuda:11.7.1-devel-ubuntu20.04  /usr/local/nvidia/bin/nvidia-smi`
		// 传递几个设备到容器里，rw 权限
		response.Devices = plugin.apiDeviceSpecs(*plugin.config.Flags.NvidiaDriverRoot, requestIds)
	}
	if *plugin.config.Flags.GDSEnabled {
		response.Envs["NVIDIA_GDS"] = "enabled"
	}
	if *plugin.config.Flags.MOFEDEnabled {
		response.Envs["NVIDIA_MOFED"] = "enabled"
	}

	return &response, nil
}

// getAllocateResponseForCDI returns the allocate response for the specified device IDs.
// This response contains the annotations required to trigger CDI injection in the container engine or nvidia-container-runtime.
// getAllocateResponseForCDI返回指定设备id的分配响应。
// 这个响应包含了在容器引擎或nvidia-container-runtime中触发CDI注入所需的注解。
func (plugin *NvidiaDevicePlugin) getAllocateResponseForCDI(responseID string, deviceIDs []string) (pluginapi.ContainerAllocateResponse, error) {
	response := pluginapi.ContainerAllocateResponse{}

	if !plugin.cdiEnabled {
		return response, nil
	}

	var devices []string
	for _, id := range deviceIDs {
		// cdi.k8s.io/gpu=id
		devices = append(devices, plugin.cdiHandler.QualifiedName("gpu", id))
	}

	if *plugin.config.Flags.GDSEnabled {
		// cdi.k8s.io/gds=all
		devices = append(devices, plugin.cdiHandler.QualifiedName("gds", "all"))
	}
	if *plugin.config.Flags.MOFEDEnabled {
		// cdi.k8s.io/mofed=all
		devices = append(devices, plugin.cdiHandler.QualifiedName("mofed", "all"))
	}

	if len(devices) == 0 {
		return response, nil
	}

	if plugin.deviceListStrategies.Includes(spec.DeviceListStrategyCDIAnnotations) {
		annotations, err := plugin.getCDIDeviceAnnotations(responseID, devices) // 添加一个cdi声明
		if err != nil {
			return response, err
		}
		response.Annotations = annotations
	}

	return response, nil
}

// PreStartContainer is unimplemented for this plugin
func (plugin *NvidiaDevicePlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

// 添加一个声明
func (plugin *NvidiaDevicePlugin) getCDIDeviceAnnotations(id string, devices []string) (map[string]string, error) {
	// cdi.k8s.io/nvidia-device-plugin_id = devices

	annotations, err := cdiapi.UpdateAnnotations(map[string]string{}, "nvidia-device-plugin", id, devices)
	if err != nil {
		return nil, fmt.Errorf("failed to add CDI annotations: %v", err)
	}

	if plugin.cdiAnnotationPrefix == spec.DefaultCDIAnnotationPrefix {
		return annotations, nil
	}

	// update annotations if a custom CDI prefix is configured
	updatedAnnotations := make(map[string]string)
	for k, v := range annotations {
		newKey := plugin.cdiAnnotationPrefix + strings.TrimPrefix(k, spec.DefaultCDIAnnotationPrefix)
		updatedAnnotations[newKey] = v
	}

	return updatedAnnotations, nil
}

func (plugin *NvidiaDevicePlugin) deviceIDsFromAnnotatedDeviceIDs(ids []string) []string {
	var deviceIDs []string
	if *plugin.config.Flags.Plugin.DeviceIDStrategy == spec.DeviceIDStrategyUUID {
		deviceIDs = rm.AnnotatedIDs(ids).GetIDs()
	}
	if *plugin.config.Flags.Plugin.DeviceIDStrategy == spec.DeviceIDStrategyIndex {
		deviceIDs = plugin.rm.Devices().Subset(ids).GetIndices()
	}
	return deviceIDs
}

func (plugin *NvidiaDevicePlugin) apiDevices() []*pluginapi.Device {
	return plugin.rm.Devices().GetPluginDevices()
}
