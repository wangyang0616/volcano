/*
Copyright 2024 The Volcano Authors.

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

package networkqos

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/agent/apis"
	"volcano.sh/volcano/pkg/agent/config/api"
	"volcano.sh/volcano/pkg/agent/features"
	"volcano.sh/volcano/pkg/agent/utils/cgroup"
	"volcano.sh/volcano/pkg/agent/utils/exec"
	"volcano.sh/volcano/pkg/config"
	"volcano.sh/volcano/pkg/networkqos/priority"
	"volcano.sh/volcano/pkg/networkqos/utils"
)

type NetworkQoSManager interface {
	Init() error
	HealthCheck() error
	EnableNetworkQoS(qosConf *api.NetworkQos) error
	DisableNetworkQoS() error
	SetPodPriority(podUID types.UID, qosClass corev1.PodQOSClass, priority uint32) error
	RemovePodPriority(podUID types.UID) error
	Close() error
}

var networkQoSManager NetworkQoSManager

type NetworkQoSManagerImp struct {
	config             *config.Configuration
	flavorQuotaMinRate int64
	priorityManager    priority.Manager
	supported          bool
}

func NewNetworkQoSManager(config *config.Configuration, cgroupMgr cgroup.CgroupManager) NetworkQoSManager {
	networkQoSManager = &NetworkQoSManagerImp{
		config:          config,
		priorityManager: priority.NewManager(cgroupMgr, utils.CgroupV2ProgramPath),
	}
	return networkQoSManager
}

func GetNetworkQoSManager(config *config.Configuration, cgroupMgr cgroup.CgroupManager) NetworkQoSManager {
	if networkQoSManager == nil {
		return NewNetworkQoSManager(config, cgroupMgr)
	}
	return networkQoSManager
}

func (m *NetworkQoSManagerImp) Init() error {
	checkErr := features.CheckNodeSupportNetworkQoS()
	if checkErr != nil {
		if features.IsUnsupportedError(checkErr) {
			klog.InfoS("Skip initializing network-qos, os/network-mode not supported")
			return nil
		}
		return checkErr
	}

	if err := installNetworkQoS(); err != nil {
		return err
	}
	if err := m.priorityManager.Init(); err != nil {
		return fmt.Errorf("failed to initialize pod network priority manager: %w", err)
	}
	m.supported = true
	return nil
}

func (m *NetworkQoSManagerImp) HealthCheck() error {
	if !m.supported {
		return nil
	}
	if err := CheckNetworkQoSStatus(); err != nil {
		return err
	}
	return m.priorityManager.HealthCheck()
}

func (m *NetworkQoSManagerImp) SetPodPriority(podUID types.UID, qosClass corev1.PodQOSClass, priority uint32) error {
	if !m.supported {
		return nil
	}
	return m.priorityManager.SetPodPriority(podUID, qosClass, priority)
}

func (m *NetworkQoSManagerImp) RemovePodPriority(podUID types.UID) error {
	if !m.supported {
		return nil
	}
	return m.priorityManager.RemovePodPriority(podUID)
}

func (m *NetworkQoSManagerImp) Close() error {
	return m.priorityManager.Close()
}

func (m *NetworkQoSManagerImp) EnableNetworkQoS(qosConf *api.NetworkQos) error {
	onlineBandwidthWatermark, offlineLowBandwidth, offlineHighBandwidth, err := m.GetBandwidthConfigs(qosConf)
	if err != nil {
		return fmt.Errorf("failed to get bandwidth configs: %v", err)
	}

	var checkInterval string
	if qosConf != nil && qosConf.QoSCheckInterval != nil {
		checkInterval = strconv.Itoa(*qosConf.QoSCheckInterval)
	}

	cmdCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := fmt.Sprintf(utils.NetWorkCmdFile+" prepare --%s=%s --%s=%s --%s=%s --%s=%s --%s=%s",
		utils.EnableNetworkQoS, "true",
		utils.OnlineBandwidthWatermarkKey, onlineBandwidthWatermark,
		utils.OfflineLowBandwidthKey, offlineLowBandwidth,
		utils.OfflineHighBandwidthKey, offlineHighBandwidth,
		utils.NetWorkQoSCheckInterval, checkInterval)
	output, err := exec.GetExecutor().CommandContext(cmdCtx, cmd)
	if err != nil {
		return fmt.Errorf("failed to set network qos:%v, output:%s", err, output)
	}
	return nil
}

func (m *NetworkQoSManagerImp) DisableNetworkQoS() error {
	cmdCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := fmt.Sprintf(utils.NetWorkCmdFile+" prepare --%s=%s", utils.EnableNetworkQoS, "false")
	output, err := exec.GetExecutor().CommandContext(cmdCtx, cmd)
	if err != nil {
		return fmt.Errorf("failed to reset network qos:%v, output:%s", err, output)
	}
	return m.priorityManager.Close()
}

func (m *NetworkQoSManagerImp) GetBandwidthConfigs(qosConf *api.NetworkQos) (onlineBandwidthWatermark, offlineLowBandwidth, offlineHighBandwidth string, err error) {
	if m.flavorQuotaMinRate == 0 {
		nodeName := m.config.GenericConfiguration.KubeNodeName
		getCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		node, err := m.config.GenericConfiguration.KubeClient.CoreV1().Nodes().Get(getCtx, nodeName, metav1.GetOptions{})
		if err != nil {
			return "", "", "0", fmt.Errorf("failed to get k8s node(%s): %v", nodeName, err)
		}
		m.flavorQuotaMinRate, err = GetFlavorQuotaMinRate(node)
		if err != nil {
			return "", "", "", fmt.Errorf("failed to get flavor quota min rate, err: %v", err)
		}
	}

	onlineBandwidthWatermark, err = GetOnlineBandwidthWatermark(m.flavorQuotaMinRate, qosConf)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get onlineBandwidthWatermark, err: %v", err)
	}

	offlineLowBandwidth, err = GetOfflineLowBandwidthPercent(m.flavorQuotaMinRate, qosConf)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get offlineLowBandwidth, err: %v", err)
	}

	offlineHighBandwidth, err = GetOfflineHighBandwidthPercent(m.flavorQuotaMinRate, qosConf)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get offlineHighBandwidth, err: %v", err)
	}

	return onlineBandwidthWatermark, offlineLowBandwidth, offlineHighBandwidth, nil
}

func GetFlavorQuotaMinRate(node *corev1.Node) (int64, error) {
	minRate, ok := node.Annotations[apis.NetworkBandwidthRateAnnotationKey]
	if !ok {
		return 0, fmt.Errorf("node %s network bandwidth rate not exists, annotation %s", node.Name, apis.NetworkBandwidthRateAnnotationKey)
	}

	minRateInt, err := strconv.ParseInt(minRate, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to cover net rate spec(%s) to int, err: %v", minRate, err)
	}
	return minRateInt, nil
}

func GetOnlineBandwidthWatermark(serverRateQuota int64, qosConf *api.NetworkQos) (string, error) {
	var onlineBandwidthWatermarkPercent int
	if qosConf != nil && qosConf.OnlineBandwidthWatermarkPercent != nil {
		onlineBandwidthWatermarkPercent = *qosConf.OnlineBandwidthWatermarkPercent
	} else {
		return "", fmt.Errorf("illegal network config, parameter OnlineBandwidthWatermarkPercent missing")
	}

	return strconv.FormatInt(serverRateQuota*int64(onlineBandwidthWatermarkPercent)/100, 10) + "Mbps", nil
}

func GetOfflineLowBandwidthPercent(serverRateQuota int64, qosConf *api.NetworkQos) (string, error) {
	var offlineLowBandwidthPercent int
	if qosConf != nil && qosConf.OfflineLowBandwidthPercent != nil {
		offlineLowBandwidthPercent = *qosConf.OfflineLowBandwidthPercent
	} else {
		return "", fmt.Errorf("illegal network config, parameter OfflineLowBandwidthPercent missing")
	}

	return strconv.FormatInt(serverRateQuota*int64(offlineLowBandwidthPercent)/100, 10) + "Mbps", nil
}

func GetOfflineHighBandwidthPercent(serverRateQuota int64, qosConf *api.NetworkQos) (string, error) {
	var offlineHighBandwidthPercent int
	if qosConf != nil && qosConf.OfflineHighBandwidthPercent != nil {
		offlineHighBandwidthPercent = *qosConf.OfflineHighBandwidthPercent
	} else {
		return "", fmt.Errorf("illegal network config, parameter OfflineHighBandwidthPercent missing")
	}

	return strconv.FormatInt(serverRateQuota*int64(offlineHighBandwidthPercent)/100, 10) + "Mbps", nil
}

func InstallNetworkQoS() error {
	checkErr := features.CheckNodeSupportNetworkQoS()
	if checkErr != nil {
		if features.IsUnsupportedError(checkErr) {
			klog.InfoS("Skip installing network-qos, os/network-mode not supported")
			return nil
		}
		return checkErr
	}
	return installNetworkQoS()
}

func installNetworkQoS() error {
	klog.InfoS("Start to install network-qos")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := "sudo /bin/cp -f /usr/local/bin/bwm_tc.o /usr/share/bwmcli"
	output, err := exec.GetExecutor().CommandContext(ctx, cmd)
	if err != nil {
		klog.ErrorS(err, "Failed to install bwm_tc.o to /usr/share/bwmcli", "output", output)
		return err
	}

	cmd = "sudo /bin/cp -f /usr/local/bin/network-qos /opt/cni/bin"
	output, err = exec.GetExecutor().CommandContext(ctx, cmd)
	if err != nil {
		klog.ErrorS(err, "Failed to install network-qos to /opt/cni/bin; %s", "output", output)
		return err
	}
	klog.InfoS("Successfully installed network-qos")
	return nil
}

func CheckNetworkQoSStatus() error {
	_, err := os.Stat(utils.TCPROGPath)
	if err != nil {
		klog.ErrorS(err, "Failed to check the existence of bwm_tc.o", "path", utils.TCPROGPath)
		return err
	}

	_, err = os.Stat(utils.NetworkQoSPath)
	if err != nil {
		klog.ErrorS(err, "Failed to check the existence of network-qos", "path", utils.NetworkQoSPath)
		return err
	}
	return nil
}
