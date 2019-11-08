/*
Copyright 2018 The Kubernetes Authors.

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

package metrics

import (
	"sync"

	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/controller/volume/attachdetach/cache"
	"k8s.io/kubernetes/pkg/controller/volume/attachdetach/util"
	"k8s.io/kubernetes/pkg/volume"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
)

const pluginNameNotAvailable = "N/A"

var (
	inUseVolumeMetricDesc = metrics.NewDesc(
		metrics.BuildFQName("", "storage_count", "attachable_volumes_in_use"),
		"Measure number of volumes in use",
		[]string{"node", "volume_plugin"}, nil,
		metrics.ALPHA, "")

	totalVolumesMetricDesc = metrics.NewDesc(
		metrics.BuildFQName("", "attachdetach_controller", "total_volumes"),
		"Number of volumes in A/D Controller",
		[]string{"plugin_name", "state"}, nil,
		metrics.ALPHA, "")

	forcedDetachMetricCounter = metrics.NewCounter(
		&metrics.CounterOpts{
			Name:           "attachdetach_controller_forced_detaches",
			Help:           "Number of times the A/D Controller performed a forced detach",
			StabilityLevel: metrics.ALPHA,
		})
)
var registerMetrics sync.Once

// Register registers metrics in A/D Controller.
func Register(pvcLister corelisters.PersistentVolumeClaimLister,
	pvLister corelisters.PersistentVolumeLister,
	podLister corelisters.PodLister,
	asw cache.ActualStateOfWorld,
	dsw cache.DesiredStateOfWorld,
	pluginMgr *volume.VolumePluginMgr) {
	registerMetrics.Do(func() {
		legacyregistry.CustomMustRegister(newAttachDetachStateCollector(pvcLister,
			podLister,
			pvLister,
			asw,
			dsw,
			pluginMgr))
		legacyregistry.MustRegister(forcedDetachMetricCounter)
	})
}

type attachDetachStateCollector struct {
	metrics.BaseStableCollector

	pvcLister       corelisters.PersistentVolumeClaimLister
	podLister       corelisters.PodLister
	pvLister        corelisters.PersistentVolumeLister
	asw             cache.ActualStateOfWorld
	dsw             cache.DesiredStateOfWorld
	volumePluginMgr *volume.VolumePluginMgr
}

// volumeCount is a map of maps used as a counter, e.g.:
//     node 172.168.1.100.ec2.internal has 10 EBS and 3 glusterfs PVC in use:
//     {"172.168.1.100.ec2.internal": {"aws-ebs": 10, "glusterfs": 3}}
//     state actual_state_of_world contains a total of 10 EBS volumes:
//     {"actual_state_of_world": {"aws-ebs": 10}}
type volumeCount map[string]map[string]int64

func (v volumeCount) add(typeKey, counterKey string) {
	count, ok := v[typeKey]
	if !ok {
		count = map[string]int64{}
	}
	count[counterKey]++
	v[typeKey] = count
}

func newAttachDetachStateCollector(
	pvcLister corelisters.PersistentVolumeClaimLister,
	podLister corelisters.PodLister,
	pvLister corelisters.PersistentVolumeLister,
	asw cache.ActualStateOfWorld,
	dsw cache.DesiredStateOfWorld,
	pluginMgr *volume.VolumePluginMgr) *attachDetachStateCollector {
	return &attachDetachStateCollector{pvcLister: pvcLister, podLister: podLister, pvLister: pvLister, asw: asw, dsw: dsw, volumePluginMgr: pluginMgr}
}

// Check if our collector implements necessary collector interface
var _ metrics.StableCollector = &attachDetachStateCollector{}

func (collector *attachDetachStateCollector) DescribeWithStability(ch chan<- *metrics.Desc) {
	ch <- inUseVolumeMetricDesc
	ch <- totalVolumesMetricDesc
}

func (collector *attachDetachStateCollector) CollectWithStability(ch chan<- metrics.Metric) {
	nodeVolumeMap := collector.getVolumeInUseCount()
	for nodeName, pluginCount := range nodeVolumeMap {
		for pluginName, count := range pluginCount {
			ch <- metrics.NewLazyConstMetric(inUseVolumeMetricDesc,
				metrics.GaugeValue,
				float64(count),
				string(nodeName),
				pluginName)
		}
	}

	stateVolumeMap := collector.getTotalVolumesCount()
	for stateName, pluginCount := range stateVolumeMap {
		for pluginName, count := range pluginCount {
			ch <- metrics.NewLazyConstMetric(totalVolumesMetricDesc,
				metrics.GaugeValue,
				float64(count),
				pluginName,
				string(stateName))
		}
	}
}

func (collector *attachDetachStateCollector) getVolumeInUseCount() volumeCount {
	pods, err := collector.podLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Error getting pod list")
		return nil
	}

	nodeVolumeMap := make(volumeCount)
	for _, pod := range pods {
		if len(pod.Spec.Volumes) <= 0 {
			continue
		}

		if pod.Spec.NodeName == "" {
			continue
		}
		for _, podVolume := range pod.Spec.Volumes {
			volumeSpec, err := util.CreateVolumeSpec(podVolume, pod.Namespace, collector.pvcLister, collector.pvLister)
			if err != nil {
				continue
			}
			volumePlugin, err := collector.volumePluginMgr.FindPluginBySpec(volumeSpec)
			if err != nil {
				continue
			}
			pluginName := volumeutil.GetFullQualifiedPluginNameForVolume(volumePlugin.GetPluginName(), volumeSpec)
			nodeVolumeMap.add(pod.Spec.NodeName, pluginName)
		}
	}
	return nodeVolumeMap
}

func (collector *attachDetachStateCollector) getTotalVolumesCount() volumeCount {
	stateVolumeMap := make(volumeCount)
	for _, v := range collector.dsw.GetVolumesToAttach() {
		if plugin, err := collector.volumePluginMgr.FindPluginBySpec(v.VolumeSpec); err == nil {
			pluginName := pluginNameNotAvailable
			if plugin != nil {
				pluginName = volumeutil.GetFullQualifiedPluginNameForVolume(plugin.GetPluginName(), v.VolumeSpec)
			}
			stateVolumeMap.add("desired_state_of_world", pluginName)
		}
	}
	for _, v := range collector.asw.GetAttachedVolumes() {
		if plugin, err := collector.volumePluginMgr.FindPluginBySpec(v.VolumeSpec); err == nil {
			pluginName := pluginNameNotAvailable
			if plugin != nil {
				pluginName = volumeutil.GetFullQualifiedPluginNameForVolume(plugin.GetPluginName(), v.VolumeSpec)
			}
			stateVolumeMap.add("actual_state_of_world", pluginName)
		}
	}
	return stateVolumeMap
}

// RecordForcedDetachMetric register a forced detach metric.
func RecordForcedDetachMetric() {
	forcedDetachMetricCounter.Inc()
}
