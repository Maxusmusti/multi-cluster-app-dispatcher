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
/*
Copyright 2019, 2021 The Multi-Cluster App Dispatcher Authors.

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
package provider

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/emicklei/go-restful"
	"k8s.io/klog/v2"

	apierr "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/metrics/pkg/apis/custom_metrics"
	"k8s.io/metrics/pkg/apis/external_metrics"

	clusterstatecache "github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/clusterstate/cache"
	"github.com/kubernetes-sigs/custom-metrics-apiserver/pkg/provider"
	"github.com/kubernetes-sigs/custom-metrics-apiserver/pkg/provider/helpers"
)

// CustomMetricResource wraps provider.CustomMetricInfo in a struct which stores the Name and Namespace of the resource
// So that we can accurately store and retrieve the metric as if this were an actual metrics server.
type CustomMetricResource struct {
	provider.CustomMetricInfo
	types.NamespacedName
}

// externalMetric provides examples for metrics which would otherwise be reported from an external source
// TODO (damemi): add dynamic external metrics instead of just hardcoded examples
type ExternalMetric struct {
	info   provider.ExternalMetricInfo
	labels map[string]string
	Value  external_metrics.ExternalMetricValue
}

var (
	defaultValueExternalMetrics = []ExternalMetric{
		{
			info: provider.ExternalMetricInfo{
				Metric: "cluster-external-metric",
			},
			labels: map[string]string{"cluster": "cpu"},
			Value: external_metrics.ExternalMetricValue{
				MetricName: "cluster-external-metric",
				MetricLabels: map[string]string{
					"cluster": "cpu",
				},
				Value: *resource.NewQuantity(0, resource.DecimalSI),
			},
		},
		{
			info: provider.ExternalMetricInfo{
				Metric: "cluster-external-metric",
			},
			labels: map[string]string{"cluster": "memory"},
			Value: external_metrics.ExternalMetricValue{
				MetricName: "cluster-external-metric",
				MetricLabels: map[string]string{
					"cluster": "memory",
				},
				Value: *resource.NewQuantity(0, resource.DecimalSI),
			},
		},
		{
			info: provider.ExternalMetricInfo{
				Metric: "cluster-external-metric",
			},
			labels: map[string]string{"cluster": "gpu"},
			Value: external_metrics.ExternalMetricValue{
				MetricName: "cluster-external-metric",
				MetricLabels: map[string]string{
					"cluster": "gpu",
				},
				Value: *resource.NewQuantity(0, resource.DecimalSI),
			},
		},
		{
			info: provider.ExternalMetricInfo{
				Metric: "other-external-metric",
			},
			labels: map[string]string{},
			Value: external_metrics.ExternalMetricValue{
				MetricName:   "other-external-metric",
				MetricLabels: map[string]string{},
				Value:        *resource.NewQuantity(44, resource.DecimalSI),
			},
		},
	}
)

type metricValue struct {
	labels labels.Set
	value  resource.Quantity
}

// clusterMetricsProvider is a sample implementation of provider.MetricsProvider which stores a map of fake metrics
type clusterMetricsProvider struct {
	client dynamic.Interface
	mapper apimeta.RESTMapper

	valuesLock      sync.RWMutex
	values          map[CustomMetricResource]metricValue
	externalMetrics []ExternalMetric
	cache2          clusterstatecache.Cache
}

// NewFakeProvider returns an instance of clusterMetricsProvider, along with its restful.WebService that opens endpoints to post new fake metrics
func NewFakeProvider(client dynamic.Interface, mapper apimeta.RESTMapper, clusterStateCache clusterstatecache.Cache) (provider.MetricsProvider, *restful.WebService) {
	klog.V(10).Infof("[NewFakeProvider] Entered NewFakeProvider()")
	provider := &clusterMetricsProvider{
		client:          client,
		mapper:          mapper,
		values:          make(map[CustomMetricResource]metricValue),
		externalMetrics: defaultValueExternalMetrics,
		cache2:          clusterStateCache,
	}
	return provider, provider.webService()
}

// webService creates a restful.WebService with routes set up for receiving fake metrics
// These writing routes have been set up to be identical to the format of routes which metrics are read from.
// There are 3 metric types available: namespaced, root-scoped, and namespaces.
// (Note: Namespaces, we're assuming, are themselves namespaced resources, but for consistency with how metrics are retreived they have a separate route)
func (p *clusterMetricsProvider) webService() *restful.WebService {
	klog.V(10).Infof("Entered webService()")
	ws := new(restful.WebService)

	ws.Path("/write-metrics")

	// Namespaced resources
	ws.Route(ws.POST("/namespaces/{namespace}/{resourceType}/{name}/{metric}").To(p.updateMetric).
		Param(ws.BodyParameter("Value", "Value to set metric").DataType("integer").DefaultValue("0")))

	// Root-scoped resources
	ws.Route(ws.POST("/{resourceType}/{name}/{metric}").To(p.updateMetric).
		Param(ws.BodyParameter("Value", "Value to set metric").DataType("integer").DefaultValue("0")))

	// Namespaces, where {resourceType} == "namespaces" to match API
	ws.Route(ws.POST("/{resourceType}/{name}/metrics/{metric}").To(p.updateMetric).
		Param(ws.BodyParameter("Value", "Value to set metric").DataType("integer").DefaultValue("0")))
	return ws
}

// updateMetric writes the metric provided by a restful request and stores it in memory
func (p *clusterMetricsProvider) updateMetric(request *restful.Request, response *restful.Response) {
	p.valuesLock.Lock()
	defer p.valuesLock.Unlock()

	namespace := request.PathParameter("namespace")
	resourceType := request.PathParameter("resourceType")
	namespaced := false
	if len(namespace) > 0 || resourceType == "namespaces" {
		namespaced = true
	}
	name := request.PathParameter("name")
	metricName := request.PathParameter("metric")

	value := new(resource.Quantity)
	err := request.ReadEntity(value)
	if err != nil {
		response.WriteErrorString(http.StatusBadRequest, err.Error())
		return
	}

	groupResource := schema.ParseGroupResource(resourceType)

	metricLabels := labels.Set{}
	sel := request.QueryParameter("labels")
	if len(sel) > 0 {
		metricLabels, err = labels.ConvertSelectorToLabelsMap(sel)
		if err != nil {
			response.WriteErrorString(http.StatusBadRequest, err.Error())
			return
		}
	}

	info := provider.CustomMetricInfo{
		GroupResource: groupResource,
		Metric:        metricName,
		Namespaced:    namespaced,
	}

	info, _, err = info.Normalized(p.mapper)
	if err != nil {
		klog.Errorf("Error normalizing info: %s", err)
	}
	namespacedName := types.NamespacedName{
		Name:      name,
		Namespace: namespace,
	}

	metricInfo := CustomMetricResource{
		CustomMetricInfo: info,
		NamespacedName:   namespacedName,
	}
	p.values[metricInfo] = metricValue{
		labels: metricLabels,
		value:  *value,
	}
}

// valueFor is a helper function to get just the value of a specific metric
func (p *clusterMetricsProvider) valueFor(info provider.CustomMetricInfo, name types.NamespacedName, metricSelector labels.Selector) (resource.Quantity, error) {
	info, _, err := info.Normalized(p.mapper)
	if err != nil {
		return resource.Quantity{}, err
	}
	metricInfo := CustomMetricResource{
		CustomMetricInfo: info,
		NamespacedName:   name,
	}

	value, found := p.values[metricInfo]
	if !found {
		return resource.Quantity{}, provider.NewMetricNotFoundForError(info.GroupResource, info.Metric, name.Name)
	}

	if !metricSelector.Matches(value.labels) {
		return resource.Quantity{}, provider.NewMetricNotFoundForSelectorError(info.GroupResource, info.Metric, name.Name, metricSelector)
	}

	return value.value, nil
}

// metricFor is a helper function which formats a value, metric, and object info into a MetricValue which can be returned by the metrics API
func (p *clusterMetricsProvider) metricFor(value resource.Quantity, name types.NamespacedName, selector labels.Selector, info provider.CustomMetricInfo, metricSelector labels.Selector) (*custom_metrics.MetricValue, error) {
	objRef, err := helpers.ReferenceFor(p.mapper, name, info)
	if err != nil {
		return nil, err
	}

	metric := &custom_metrics.MetricValue{
		DescribedObject: objRef,
		Metric: custom_metrics.MetricIdentifier{
			Name: info.Metric,
		},
		Timestamp: metav1.Time{time.Now()},
		Value:     value,
	}

	if len(metricSelector.String()) > 0 {
		sel, err := metav1.ParseToLabelSelector(metricSelector.String())
		if err != nil {
			return nil, err
		}
		metric.Metric.Selector = sel
	}

	return metric, nil
}

// metricsFor is a wrapper used by GetMetricBySelector to format several metrics which match a resource selector
func (p *clusterMetricsProvider) metricsFor(namespace string, selector labels.Selector, info provider.CustomMetricInfo, metricSelector labels.Selector) (*custom_metrics.MetricValueList, error) {
	names, err := helpers.ListObjectNames(p.mapper, p.client, namespace, selector, info)
	if err != nil {
		return nil, err
	}

	res := make([]custom_metrics.MetricValue, 0, len(names))
	for _, name := range names {
		namespacedName := types.NamespacedName{Name: name, Namespace: namespace}
		value, err := p.valueFor(info, namespacedName, metricSelector)
		if err != nil {
			if apierr.IsNotFound(err) {
				continue
			}
			return nil, err
		}

		metric, err := p.metricFor(value, namespacedName, selector, info, metricSelector)
		if err != nil {
			return nil, err
		}
		res = append(res, *metric)
	}

	return &custom_metrics.MetricValueList{
		Items: res,
	}, nil
}

func (p *clusterMetricsProvider) GetMetricByName(name types.NamespacedName, info provider.CustomMetricInfo, metricSelector labels.Selector) (*custom_metrics.MetricValue, error) {
	p.valuesLock.RLock()
	defer p.valuesLock.RUnlock()

	value, err := p.valueFor(info, name, metricSelector)
	if err != nil {
		return nil, err
	}
	return p.metricFor(value, name, labels.Everything(), info, metricSelector)
}

func (p *clusterMetricsProvider) GetMetricBySelector(namespace string, selector labels.Selector, info provider.CustomMetricInfo, metricSelector labels.Selector) (*custom_metrics.MetricValueList, error) {
	p.valuesLock.RLock()
	defer p.valuesLock.RUnlock()

	return p.metricsFor(namespace, selector, info, metricSelector)
}

func (p *clusterMetricsProvider) ListAllMetrics() []provider.CustomMetricInfo {
	p.valuesLock.RLock()
	defer p.valuesLock.RUnlock()

	// Get unique CustomMetricInfos from wrapper CustomMetricResources
	infos := make(map[provider.CustomMetricInfo]struct{})
	for resource := range p.values {
		infos[resource.CustomMetricInfo] = struct{}{}
	}

	// Build slice of CustomMetricInfos to be returns
	metrics := make([]provider.CustomMetricInfo, 0, len(infos))
	for info := range infos {
		metrics = append(metrics, info)
	}

	return metrics
}

func (p *clusterMetricsProvider) GetExternalMetric(namespace string, metricSelector labels.Selector,
	info provider.ExternalMetricInfo) (*external_metrics.ExternalMetricValueList, error) {
	klog.V(10).Infof("[GetExternalMetric] Entered GetExternalMetric()")
	klog.V(9).Infof("[GetExternalMetric] metricsSelector: %s, metricsInfo: %s", metricSelector.String(), info.Metric)
	p.valuesLock.RLock()
	defer p.valuesLock.RUnlock()

	matchingMetrics := []external_metrics.ExternalMetricValue{}
	for _, metric := range p.externalMetrics {
		klog.V(9).Infof("[GetExternalMetric] externalMetricsInfo: %s, externalMetricValue: %v, externalMetricLabels: %v ",
			metric.info.Metric, metric.Value, metric.labels)
		if metric.info.Metric == info.Metric && metricSelector.Matches(labels.Set(metric.labels)) {
			metricValue := metric.Value
			labelVal := metric.labels["cluster"]
			klog.V(9).Infof("[GetExternalMetric] cluster label value: %s, ", labelVal)
			// Set memory Value
			if strings.Compare(labelVal, "memory") == 0 {
				resources := p.cache2.GetUnallocatedResources()
				klog.V(9).Infof("[GetExternalMetric] Cache resources: %v", resources)

				klog.V(10).Infof("[GetExternalMetric] Setting memory metric Value: %f.", resources.Memory)
				metricValue.Value = *resource.NewQuantity(int64(resources.Memory), resource.DecimalSI)
				//metricValue.Value = *resource.NewQuantity(4500000000, resource.DecimalSI)
			} else if strings.Compare(labelVal, "cpu") == 0 {
				// Set cpu Value
				resources := p.cache2.GetUnallocatedResources()
				klog.V(9).Infof("[GetExternalMetric] Cache resources: %f", resources)

				klog.V(10).Infof("[GetExternalMetric] Setting cpu metric Value: %v.", resources.MilliCPU)
				metricValue.Value = *resource.NewQuantity(int64(resources.MilliCPU), resource.DecimalSI)
			} else if strings.Compare(labelVal, "gpu") == 0 {
				// Set gpu Value
				resources := p.cache2.GetUnallocatedResources()
				klog.V(9).Infof("[GetExternalMetric] Cache resources: %f", resources)

				klog.V(10).Infof("[GetExternalMetric] Setting gpu metric Value: %v.", resources.GPU)
				metricValue.Value = *resource.NewQuantity(resources.GPU, resource.DecimalSI)
			} else {
				klog.V(10).Infof("[GetExternalMetric] Not setting cpu/memory metric Value")
			}

			metricValue.Timestamp = metav1.Now()
			matchingMetrics = append(matchingMetrics, metricValue)
		}
	}
	return &external_metrics.ExternalMetricValueList{
		Items: matchingMetrics,
	}, nil
}

func (p *clusterMetricsProvider) ListAllExternalMetrics() []provider.ExternalMetricInfo {
	klog.V(10).Infof("Entered ListAllExternalMetrics()")
	p.valuesLock.RLock()
	defer p.valuesLock.RUnlock()

	externalMetricsInfo := []provider.ExternalMetricInfo{}
	for _, metric := range p.externalMetrics {
		externalMetricsInfo = append(externalMetricsInfo, metric.info)
		klog.V(9).Infof("Add metric=%v to externalMetricsInfo", metric)
	}
	klog.V(9).Infof("ExternalMetricsInfo=%v", externalMetricsInfo)
	return externalMetricsInfo
}
