/*
 * Copyright (c) 2019-2021, NVIDIA CORPORATION.  All rights reserved.
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

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"time"

	"4pd.io/k8s-vgpu/pkg/device-plugin/nvidiadevice/nvinternal/over_info"
	"4pd.io/k8s-vgpu/pkg/device-plugin/nvidiadevice/nvinternal/plugin"
	"4pd.io/k8s-vgpu/pkg/device-plugin/nvidiadevice/nvinternal/rm"
	"4pd.io/k8s-vgpu/pkg/util"
	spec "github.com/NVIDIA/k8s-device-plugin/api/config/v1"
	"github.com/fsnotify/fsnotify"
	cli "github.com/urfave/cli/v2"

	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func main() {
	var configFile string

	c := cli.NewApp()
	c.Name = "NVIDIA Device Plugin"
	c.Usage = "NVIDIA device plugin for Kubernetes"
	c.Version = over_info.GetVersionString()
	c.Action = func(ctx *cli.Context) error {
		return start(ctx, c.Flags)
	}

	c.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:    "mig-strategy",
			Value:   spec.MigStrategyNone,
			Usage:   "在支持MIG的gpu上公开MIG设备所需的策略:\n\t\t[none | single | mixed]",
			EnvVars: []string{"MIG_STRATEGY"},
		},
		&cli.BoolFlag{
			Name:    "fail-on-init-error",
			Value:   true,
			Usage:   "如果在初始化过程中遇到错误，则失败插件，否则将无限期阻塞",
			EnvVars: []string{"FAIL_ON_INIT_ERROR"},
		},
		&cli.StringFlag{
			Name:    "nvidia-driver-root",
			Value:   "/",
			Usage:   "NVIDIA驱动安装的根路径(typical values are '/' or '/run/nvidia/driver')",
			EnvVars: []string{"NVIDIA_DRIVER_ROOT"},
		},
		&cli.BoolFlag{
			Name:    "pass-device-specs",
			Value:   false,
			Usage:   "传递 devicepec 列表 to the kubelet on Allocate()",
			EnvVars: []string{"PASS_DEVICE_SPECS"},
		},
		&cli.StringSliceFlag{
			Name:    "device-list-strategy",
			Value:   cli.NewStringSlice(string(spec.DeviceListStrategyEnvvar)),
			Usage:   "将设备列表传递到底层运行时所需的策略:\n\t\t[envvar | volume-mounts | cdi-annotations]",
			EnvVars: []string{"DEVICE_LIST_STRATEGY"},
		},
		&cli.StringFlag{
			Name:    "device-id-strategy",
			Value:   spec.DeviceIDStrategyUUID,
			Usage:   "将设备id传递到底层运行时所需的策略:\n\t\t[uuid | index]",
			EnvVars: []string{"DEVICE_ID_STRATEGY"},
		},
		&cli.BoolFlag{
			Name:    "gds-enabled",
			Usage:   "确保容器在启动时启用了NVIDIA_GDS=",
			EnvVars: []string{"GDS_ENABLED"},
		},
		&cli.BoolFlag{
			Name:    "mofed-enabled",
			Usage:   "确保容器在启动时启用了NVIDIA_MOFED=",
			EnvVars: []string{"MOFED_ENABLED"},
		},
		&cli.StringFlag{
			Name:        "config-file",
			Usage:       "配置文件的路径，作为命令行选项或环境变量的替代方案",
			Destination: &configFile,
			EnvVars:     []string{"CONFIG_FILE"},
		},
		&cli.StringFlag{
			Name:    "cdi-annotation-prefix",
			Value:   spec.DefaultCDIAnnotationPrefix,
			Usage:   "用于CDI容器注释键的前缀",
			EnvVars: []string{"CDI_ANNOTATION_PREFIX"},
		},
		&cli.StringFlag{
			Name:    "nvidia-ctk-path",
			Value:   spec.DefaultNvidiaCTKPath,
			Usage:   "生成的CDI规范中用于nvidia-ctk的路径",
			EnvVars: []string{"NVIDIA_CTK_PATH"},
		},
		&cli.StringFlag{
			Name:    "container-driver-root",
			Value:   spec.DefaultContainerDriverRoot,
			Usage:   "NVIDIA驱动根安装在容器中的路径;用于生成CDI规格",
			EnvVars: []string{"CONTAINER_DRIVER_ROOT"},
		},
	}
	c.Flags = append(c.Flags, addFlags()...)
	err := c.Run(os.Args)
	if err != nil {
		klog.Error(err)
		os.Exit(1)
	}
}

func validateFlags(config *spec.Config) error {
	_, err := spec.NewDeviceListStrategies(*config.Flags.Plugin.DeviceListStrategy)
	if err != nil {
		return fmt.Errorf("invalid --device-list-strategy option: %v", err)
	}

	if *config.Flags.Plugin.DeviceIDStrategy != spec.DeviceIDStrategyUUID && *config.Flags.Plugin.DeviceIDStrategy != spec.DeviceIDStrategyIndex {
		return fmt.Errorf("invalid --device-id-strategy option: %v", *config.Flags.Plugin.DeviceIDStrategy)
	}
	return nil
}

func start(c *cli.Context, flags []cli.Flag) error {
	klog.Info("Starting FS watcher.")
	watcher, err := newFSWatcher(pluginapi.DevicePluginPath)
	if err != nil {
		return fmt.Errorf("failed to create FS watcher: %v", err)
	}
	defer watcher.Close()

	/*Loading config files*/
	fmt.Println("NodeName=", util.NodeName)
	err = readFromConfigFile()
	if err != nil {
		fmt.Printf("failed to load config file %s", err.Error())
	}

	klog.Info("Starting OS watcher.")
	sigs := newOSWatcher(syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	var restarting bool
	var restartTimeout <-chan time.Time
	var plugins []plugin.Interface
restart:
	// If we are restarting, stop plugins from previous run.
	if restarting {
		err := stopPlugins(plugins)
		if err != nil {
			return fmt.Errorf("error stopping plugins from previous run: %v", err)
		}
	}

	klog.Info("Starting Plugins.")
	plugins, restartPlugins, err := startPlugins(c, flags, restarting)
	if err != nil {
		return fmt.Errorf("error starting plugins: %v", err)
	}

	if restartPlugins {
		klog.Infof("Failed to start one or more plugins. Retrying in 30s...")
		restartTimeout = time.After(30 * time.Second)
	}

	restarting = true

	// Start an infinite loop, waiting for several indicators to either log
	// some messages, trigger a restart of the plugins, or exit the program.
	for {
		select {
		// If the restart timeout has expired, then restart the plugins
		case <-restartTimeout:
			goto restart

		// Detect a kubelet restart by watching for a newly created
		// 'pluginapi.KubeletSocket' file. When this occurs, restart this loop,
		// restarting all of the plugins in the process.
		case event := <-watcher.Events:
			if event.Name == pluginapi.KubeletSocket && event.Op&fsnotify.Create == fsnotify.Create {
				klog.Infof("inotify: %s created, restarting.", pluginapi.KubeletSocket)
				goto restart
			}

		// Watch for any other fs errors and log them.
		case err := <-watcher.Errors:
			klog.Infof("inotify: %s", err)

		// Watch for any signals from the OS. On SIGHUP, restart this loop,
		// restarting all of the plugins in the process. On all other
		// signals, exit the loop and exit the program.
		case s := <-sigs:
			switch s {
			case syscall.SIGHUP:
				klog.Info("Received SIGHUP, restarting.")
				goto restart
			default:
				klog.Infof("Received signal \"%v\", shutting down.", s)
				goto exit
			}
		}
	}
exit:
	err = stopPlugins(plugins)
	if err != nil {
		return fmt.Errorf("error stopping plugins: %v", err)
	}
	return nil
}

func startPlugins(c *cli.Context, flags []cli.Flag, restarting bool) ([]plugin.Interface, bool, error) {
	// Load the configuration file
	klog.Info("Loading configuration.")
	config, err := loadConfig(c, flags) // 配置文件
	if err != nil {
		return nil, false, fmt.Errorf("unable to load config: %v", err)
	}
	disableResourceRenamingInConfig(config) // 修正

	/*Loading config files*/
	//fmt.Println("NodeName=", config.NodeName)
	devConfig, err := generateDeviceConfigFromNvidia(config, c, flags) // ✅ 设置全局变量
	if err != nil {
		fmt.Printf("failed to load config file %s", err.Error())
	}

	// 使用默认资源更新配置文件。
	klog.Info("Updating config with default resource matching patterns.")
	err = rm.AddDefaultResourcesToConfig(&devConfig)
	if err != nil {
		return nil, false, fmt.Errorf("unable to add default resources to config: %v", err)
	}

	// Print the config to the output.
	configJSON, err := json.MarshalIndent(devConfig, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("failed to marshal config to JSON: %v", err)
	}
	klog.Infof("\nRunning with config:\n%v", string(configJSON))

	// Get the set of plugins.
	klog.Info("Retrieving plugins.")
	pluginManager, err := NewPluginManager(&devConfig)
	if err != nil {
		return nil, false, fmt.Errorf("error creating plugin manager: %v", err)
	}
	plugins, err := pluginManager.GetPlugins()
	if err != nil {
		return nil, false, fmt.Errorf("error getting plugins: %v", err)
	}

	// Loop through all plugins, starting them if they have any devices
	// to serve. If even one plugin fails to start properly, try
	// starting them all again.
	started := 0
	for _, p := range plugins {
		// Just continue if there are no devices to serve for plugin p.
		if len(p.Devices()) == 0 {
			continue
		}

		// Start the gRPC server for plugin p and connect it with the kubelet.
		if err := p.Start(); err != nil {
			klog.Error("Could not contact Kubelet. Did you enable the device plugin feature gate?")
			klog.Error("You can check the prerequisites at: https://github.com/NVIDIA/k8s-device-plugin#prerequisites")
			klog.Error("You can learn how to set the runtime at: https://github.com/NVIDIA/k8s-device-plugin#quick-start")
			return plugins, true, nil
		}
		started++
	}

	if started == 0 {
		klog.Info("No devices found. Waiting indefinitely.")
	}

	return plugins, false, nil
}

// disableResourceRenamingInConfig temporarily disable the resource renaming feature of the plugin.
// We plan to reeenable this feature in a future release.
func disableResourceRenamingInConfig(config *spec.Config) {
	// Disable resource renaming through config.Resource
	if len(config.Resources.GPUs) > 0 || len(config.Resources.MIGs) > 0 {
		klog.Infof("在配置中还不支持自定义'resources'字段. Ignoring...")
	}
	config.Resources.GPUs = nil
	config.Resources.MIGs = nil

	// 禁用sharing . timeslic . resources中的重命名/设备选择
	renameByDefault := config.Sharing.TimeSlicing.RenameByDefault
	setsNonDefaultRename := false
	setsDevices := false
	for i, r := range config.Sharing.TimeSlicing.Resources {
		if !renameByDefault && r.Rename != "" {
			setsNonDefaultRename = true
			config.Sharing.TimeSlicing.Resources[i].Rename = ""
		}
		if renameByDefault && r.Rename != r.Name.DefaultSharedRename() {
			setsNonDefaultRename = true
			config.Sharing.TimeSlicing.Resources[i].Rename = r.Name.DefaultSharedRename()
		}
		if !r.Devices.All {
			setsDevices = true
			config.Sharing.TimeSlicing.Resources[i].Devices.All = true
			config.Sharing.TimeSlicing.Resources[i].Devices.Count = 0
			config.Sharing.TimeSlicing.Resources[i].Devices.List = nil
		}
	}
	if setsNonDefaultRename {
		klog.Warning("Setting the 'rename' field in sharing.timeSlicing.resources is not yet supported in the config. Ignoring...")
	}
	if setsDevices {
		klog.Warning("Customizing the 'devices' field in sharing.timeSlicing.resources is not yet supported in the config. Ignoring...")
	}
}
func stopPlugins(plugins []plugin.Interface) error {
	klog.Info("Stopping plugins.")
	for _, p := range plugins {
		p.Stop()
	}
	return nil
}
func loadConfig(c *cli.Context, flags []cli.Flag) (*spec.Config, error) {
	config, err := spec.NewConfig(c, flags)
	if err != nil {
		return nil, fmt.Errorf("unable to finalize config: %v", err)
	}
	err = validateFlags(config)
	if err != nil {
		return nil, fmt.Errorf("unable to validate flags: %v", err)
	}
	config.Flags.GFD = nil
	return config, nil
}
