package main

import (
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	defaultListenAddr          = ":8080"
	gpuFractionAnnotation      = "gpu-fraction"
	targetContainerAnnot       = "gpu-fraction-container-name"
	mockAnnotationPrefix       = "mock.kubex.ai/"
	mockGPUUUIDAnnotation      = mockAnnotationPrefix + "gpu-uuid"
	mockGPUModelAnnotation     = mockAnnotationPrefix + "gpu-model"
	defaultMockGPUUUIDLabel    = "mock-%s"
	defaultMockGPUModelLabel   = "mock-gpu"
	kaiSchedulerAllocationType = "KaiScheduler"
)

var containerLabelNames = []string{"node", "namespace", "pod", "container", "container_id", "gpu_allocation_type"}
var perDeviceLabelNames = []string{"node", "namespace", "pod", "container", "container_id", "gpu_allocation_type", "gpu_uuid", "gpu_model"}

const (
	metricRequestsName        = "kubex_gpu_container_requests"
	metricLimitsName          = "kubex_gpu_container_limits"
	metricMemoryFootprintName = "kubex_gpu_container_memory_footprint_percent"
	metricSMUtilName          = "kubex_gpu_container_sm_utilization_percent"
	metricSMUtilSecondsName   = "kubex_gpu_container_sm_utilization_percent_seconds_total"
	metricMemoryBytesName     = "kubex_gpu_container_memory_bytes"
	metricMemoryTotalName     = "kubex_gpu_container_memory_total_bytes"

	metricMemoryBytesAnnotation      = mockAnnotationPrefix + metricMemoryBytesName
	metricMemoryTotalBytesAnnotation = mockAnnotationPrefix + metricMemoryTotalName
	metricSMUtilPercentAnnotation    = mockAnnotationPrefix + metricSMUtilName
)

type gaugeSeries struct {
	metricName string
	labels     []string
	value      float64
	total      float64
	last       time.Time
}

func (s gaugeSeries) key() string {
	return s.metricName + "\xff" + strings.Join(s.labels, "\xff")
}

type exporter struct {
	nodeName string
	logger   *log.Logger
	now      func() time.Time
	utilDesc *prometheus.Desc

	mu           sync.Mutex
	gaugeMetrics map[string]*prometheus.GaugeVec
	podGauges    map[types.UID]map[string]gaugeSeries
}

type exporterOptions struct {
	now func() time.Time
}

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags)

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		logger.Fatal("NODE_NAME env var is required")
	}

	listenAddr := envOrDefault("LISTEN_ADDR", defaultListenAddr)

	registry := prometheus.NewRegistry()
	exp := newExporter(nodeName, registry, logger, exporterOptions{})

	clientset, err := newClientset()
	if err != nil {
		logger.Fatalf("failed to create kubernetes client: %v", err)
	}

	informer := newPodInformer(clientset, nodeName)
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    exp.onAdd,
		UpdateFunc: exp.onUpdate,
		DeleteFunc: exp.onDelete,
	}); err != nil {
		logger.Fatalf("failed to register informer handlers: %v", err)
	}

	stopCh := make(chan struct{})
	defer close(stopCh)

	go informer.Run(stopCh)
	logger.Printf("waiting for pod informer sync on node %q", nodeName)
	if !cache.WaitForCacheSync(stopCh, informer.HasSynced) {
		logger.Fatal("timed out waiting for pod informer cache sync")
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", exp.metricsHandler(registry))

	logger.Printf("starting mock gpu exporter on %s", listenAddr)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		logger.Fatalf("http server failed: %v", err)
	}
}

func newClientset() (*kubernetes.Clientset, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	config, err := kubeConfig.ClientConfig()
	if err != nil {
		config, err = rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
	}

	return kubernetes.NewForConfig(config)
}

func newPodInformer(clientset kubernetes.Interface, nodeName string) cache.SharedIndexInformer {
	listWatch := cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		"pods",
		corev1.NamespaceAll,
		fields.OneTermEqualSelector("spec.nodeName", nodeName),
	)

	return cache.NewSharedIndexInformer(
		listWatch,
		&corev1.Pod{},
		0,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)
}

func newExporter(nodeName string, registerer prometheus.Registerer, logger *log.Logger, opts exporterOptions) *exporter {
	gaugeMetrics := map[string]*prometheus.GaugeVec{
		metricRequestsName:        prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: metricRequestsName, Help: "Number or fraction of GPUs"}, containerLabelNames),
		metricLimitsName:          prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: metricLimitsName, Help: "Number or fraction of GPUs"}, containerLabelNames),
		metricMemoryFootprintName: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: metricMemoryFootprintName, Help: "Percent"}, perDeviceLabelNames),
		metricSMUtilName:          prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: metricSMUtilName, Help: "Percent"}, perDeviceLabelNames),
		metricMemoryBytesName:     prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: metricMemoryBytesName, Help: "GPU memory used in bytes"}, perDeviceLabelNames),
		metricMemoryTotalName:     prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: metricMemoryTotalName, Help: "Total GPU memory in bytes"}, perDeviceLabelNames),
	}

	if logger == nil {
		logger = log.New(os.Stdout, "", log.LstdFlags)
	}

	if opts.now == nil {
		opts.now = time.Now
	}
	e := &exporter{
		now:          opts.now,
		utilDesc:     prometheus.NewDesc(metricSMUtilSecondsName, "Cumulative SM utilization in percent-seconds", perDeviceLabelNames, nil),
		nodeName:     nodeName,
		logger:       logger,
		gaugeMetrics: gaugeMetrics,
		podGauges:    make(map[types.UID]map[string]gaugeSeries),
	}
	registerer.MustRegister(e)
	return e
}

func (e *exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, gauge := range e.gaugeMetrics {
		gauge.Describe(ch)
	}
	ch <- e.utilDesc
}

func (e *exporter) Collect(ch chan<- prometheus.Metric) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now()
	for _, seriesByKey := range e.podGauges {
		for key, series := range seriesByKey {
			if series.metricName == metricSMUtilName {
				series.integrate(now)
				seriesByKey[key] = series
				ch <- prometheus.MustNewConstMetric(e.utilDesc, prometheus.CounterValue, series.total, series.labels...)
			}
		}
	}
	for _, gauge := range e.gaugeMetrics {
		gauge.Collect(ch)
	}
}

func (s *gaugeSeries) integrate(now time.Time) {
	if now.After(s.last) {
		s.total += s.value * now.Sub(s.last).Seconds()
		s.last = now
	}
}

func (e *exporter) metricsHandler(gatherer prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{})
}

func (e *exporter) onAdd(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	e.reconcilePod(pod)
}

func (e *exporter) onUpdate(_, newObj interface{}) {
	pod, ok := newObj.(*corev1.Pod)
	if !ok {
		return
	}
	e.reconcilePod(pod)
}

func (e *exporter) onDelete(obj interface{}) {
	pod := podFromDeletedObject(obj)
	if pod == nil {
		return
	}
	e.removePodSeries(pod.UID)
}

func (e *exporter) reconcilePod(pod *corev1.Pod) {
	if pod == nil {
		return
	}

	if pod.Spec.NodeName != "" && pod.Spec.NodeName != e.nodeName {
		e.removePodSeries(pod.UID)
		return
	}

	desiredGauges := e.desiredSeriesForPod(pod)
	e.applySeries(pod.UID, desiredGauges)
}

func (e *exporter) desiredSeriesForPod(pod *corev1.Pod) map[string]gaugeSeries {
	if pod == nil || isTerminalPod(pod) {
		return nil
	}

	containerName := resolveContainerName(pod)
	if containerName == "" {
		return nil
	}

	containerID := fmt.Sprintf("mock://%s/%s", pod.UID, containerName)
	gpuUUID := pod.Annotations[mockGPUUUIDAnnotation]
	if gpuUUID == "" {
		gpuUUID = fmt.Sprintf(defaultMockGPUUUIDLabel, e.nodeName)
	}
	gpuModel := strings.TrimSpace(pod.Annotations[mockGPUModelAnnotation])
	if gpuModel == "" {
		gpuModel = defaultMockGPUModelLabel
	}
	containerLabels := []string{e.nodeName, pod.Namespace, pod.Name, containerName, containerID, kaiSchedulerAllocationType}
	perDeviceLabels := append(append([]string(nil), containerLabels...), gpuUUID, gpuModel)

	gaugeSeriesByKey := make(map[string]gaugeSeries, 4)

	if _, hasFraction := pod.Annotations[gpuFractionAnnotation]; hasFraction {
		if fraction, ok := parseAnnotationFloat(e, pod, gpuFractionAnnotation); ok {
			fraction = clamp(fraction, 0, 1)
			for _, metricName := range []string{metricRequestsName, metricLimitsName} {
				series := gaugeSeries{metricName: metricName, labels: append([]string(nil), containerLabels...), value: fraction}
				gaugeSeriesByKey[series.key()] = series
			}
		}
	} else {
		for _, container := range pod.Spec.Containers {
			if container.Name != containerName {
				continue
			}
			for metricName, resources := range map[string]corev1.ResourceList{
				metricRequestsName: container.Resources.Requests,
				metricLimitsName:   container.Resources.Limits,
			} {
				if quantity, ok := resources["nvidia.com/gpu"]; ok {
					series := gaugeSeries{metricName: metricName, labels: containerLabels, value: quantity.AsApproximateFloat64()}
					gaugeSeriesByKey[series.key()] = series
				}
			}
			break
		}
	}

	memoryBytes, hasMemoryBytes := parseAnnotationFloat(e, pod, metricMemoryBytesAnnotation)
	memoryTotalBytes, hasMemoryTotalBytes := parseAnnotationFloat(e, pod, metricMemoryTotalBytesAnnotation)
	for metricName, sample := range map[string]struct {
		value float64
		valid bool
	}{
		metricMemoryBytesName: {memoryBytes, hasMemoryBytes},
		metricMemoryTotalName: {memoryTotalBytes, hasMemoryTotalBytes},
	} {
		if sample.valid {
			series := gaugeSeries{metricName: metricName, labels: perDeviceLabels, value: sample.value}
			gaugeSeriesByKey[series.key()] = series
		}
	}
	if hasMemoryBytes && hasMemoryTotalBytes && memoryTotalBytes > 0 {
		footprint := clamp((memoryBytes/memoryTotalBytes)*100, 0, 100)
		series := gaugeSeries{metricName: metricMemoryFootprintName, labels: append([]string(nil), perDeviceLabels...), value: footprint}
		gaugeSeriesByKey[series.key()] = series
	}

	if smUtilPercent, ok := parseAnnotationFloat(e, pod, metricSMUtilPercentAnnotation); ok {
		series := gaugeSeries{metricName: metricSMUtilName, labels: append([]string(nil), perDeviceLabels...), value: clamp(smUtilPercent, 0, 100)}
		gaugeSeriesByKey[series.key()] = series
	}

	return gaugeSeriesByKey
}

func parseAnnotationFloat(e *exporter, pod *corev1.Pod, annotationKey string) (float64, bool) {
	rawValue, ok := pod.Annotations[annotationKey]
	if !ok || strings.TrimSpace(rawValue) == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(rawValue), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		e.logger.Printf("skipping invalid annotation %q = %q on %s/%s", annotationKey, rawValue, pod.Namespace, pod.Name)
		return 0, false
	}
	return value, true
}

func (e *exporter) applySeries(podUID types.UID, desiredGauges map[string]gaugeSeries) {
	e.mu.Lock()
	defer e.mu.Unlock()

	existingGauges := e.podGauges[podUID]
	for key, oldSeries := range existingGauges {
		if _, ok := desiredGauges[key]; ok {
			continue
		}
		e.gaugeMetrics[oldSeries.metricName].DeleteLabelValues(oldSeries.labels...)
		delete(existingGauges, key)
	}

	if len(desiredGauges) == 0 {
		delete(e.podGauges, podUID)
		return
	}
	if existingGauges == nil {
		existingGauges = make(map[string]gaugeSeries, len(desiredGauges))
		e.podGauges[podUID] = existingGauges
	}
	now := e.now()
	for key, series := range desiredGauges {
		if series.metricName == metricSMUtilName {
			if old, ok := existingGauges[key]; ok {
				old.integrate(now)
				series.total = old.total
			}
			series.last = now
		}
		e.gaugeMetrics[series.metricName].WithLabelValues(series.labels...).Set(series.value)
		existingGauges[key] = series
	}
}

func (e *exporter) removePodSeries(podUID types.UID) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, series := range e.podGauges[podUID] {
		e.gaugeMetrics[series.metricName].DeleteLabelValues(series.labels...)
	}
	delete(e.podGauges, podUID)
}

func resolveContainerName(pod *corev1.Pod) string {
	if pod == nil || len(pod.Spec.Containers) == 0 {
		return ""
	}

	requested := strings.TrimSpace(pod.Annotations[targetContainerAnnot])
	if requested == "" {
		return pod.Spec.Containers[0].Name
	}

	for _, container := range pod.Spec.Containers {
		if container.Name == requested {
			return requested
		}
	}

	return pod.Spec.Containers[0].Name
}

func isTerminalPod(pod *corev1.Pod) bool {
	if pod == nil {
		return true
	}
	return pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded
}

func podFromDeletedObject(obj interface{}) *corev1.Pod {
	switch t := obj.(type) {
	case *corev1.Pod:
		return t
	case *cache.DeletedFinalStateUnknown:
		if t != nil {
			return podFromDeletedObject(t.Obj)
		}
	case cache.DeletedFinalStateUnknown:
		if pod, ok := t.Obj.(*corev1.Pod); ok {
			return pod
		}
	case runtime.Object:
		if pod, ok := t.(*corev1.Pod); ok {
			return pod
		}
	}
	return nil
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func clamp(value, min, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func pod(namespace, name, nodeName string, annotations map[string]string, containers ...string) *corev1.Pod {
	specContainers := make([]corev1.Container, 0, len(containers))
	for _, containerName := range containers {
		specContainers = append(specContainers, corev1.Container{Name: containerName})
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   namespace,
			Name:        name,
			UID:         types.UID(namespace + "/" + name),
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: specContainers,
		},
	}
}
