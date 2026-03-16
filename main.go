package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

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
	defaultListenAddr       = ":8080"
	gpuFractionAnnotation   = "gpu-fraction"
	targetContainerAnnot    = "gpu-fraction-container-name"
	mockAnnotationPrefix    = "mock.kubex.ai/"
	mockGPUUUIDAnnotation   = mockAnnotationPrefix + "gpu-uuid"
	defaultMockGPUUUIDLabel = "mock-%s"
)

var labelNames = []string{"gpu_uuid", "node", "namespace", "pod", "container"}

var metricDefinitions = []metricDefinition{
	{
		Name:           "kubex_gpu_fraction",
		Help:           "GPU fraction annotation",
		AnnotationKey:  gpuFractionAnnotation,
		AnnotationName: gpuFractionAnnotation,
	},
	{
		Name:           "kubex_gpu_container_memory_bytes",
		Help:           "Bytes",
		AnnotationKey:  mockAnnotationPrefix + "kubex_gpu_container_memory_bytes",
		AnnotationName: "kubex_gpu_container_memory_bytes",
	},
	{
		Name:           "kubex_gpu_container_sm_utilization_percent",
		Help:           "Percent",
		AnnotationKey:  mockAnnotationPrefix + "kubex_gpu_container_sm_utilization_percent",
		AnnotationName: "kubex_gpu_container_sm_utilization_percent",
	},
	{
		Name:           "kubex_gpu_container_enc_utilization_percent",
		Help:           "Percent",
		AnnotationKey:  mockAnnotationPrefix + "kubex_gpu_container_enc_utilization_percent",
		AnnotationName: "kubex_gpu_container_enc_utilization_percent",
	},
	{
		Name:           "kubex_gpu_container_dec_utilization_percent",
		Help:           "Percent",
		AnnotationKey:  mockAnnotationPrefix + "kubex_gpu_container_dec_utilization_percent",
		AnnotationName: "kubex_gpu_container_dec_utilization_percent",
	},
	{
		Name:           "kubex_gpu_container_memory_utilization_percent",
		Help:           "Percent",
		AnnotationKey:  mockAnnotationPrefix + "kubex_gpu_container_memory_utilization_percent",
		AnnotationName: "kubex_gpu_container_memory_utilization_percent",
	},
	{
		Name:           "kubex_gpu_container_memory_footprint_percent",
		Help:           "Percent",
		AnnotationKey:  mockAnnotationPrefix + "kubex_gpu_container_memory_footprint_percent",
		AnnotationName: "kubex_gpu_container_memory_footprint_percent",
	},
}

type metricDefinition struct {
	Name           string
	Help           string
	AnnotationKey  string
	AnnotationName string
}

type metricSeries struct {
	metricName string
	labels     []string
	value      float64
}

func (s metricSeries) key() string {
	return s.metricName + "\xff" + strings.Join(s.labels, "\xff")
}

type exporter struct {
	nodeName string
	logger   *log.Logger

	mu        sync.Mutex
	metrics   map[string]*prometheus.GaugeVec
	podSeries map[types.UID]map[string]metricSeries
}

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags)

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		logger.Fatal("NODE_NAME env var is required")
	}

	listenAddr := envOrDefault("LISTEN_ADDR", defaultListenAddr)

	registry := prometheus.NewRegistry()
	exp := newExporter(nodeName, registry, logger)

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
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

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

func newExporter(nodeName string, registerer prometheus.Registerer, logger *log.Logger) *exporter {
	metrics := make(map[string]*prometheus.GaugeVec, len(metricDefinitions))
	for _, def := range metricDefinitions {
		gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: def.Name,
			Help: def.Help,
		}, labelNames)
		registerer.MustRegister(gauge)
		metrics[def.Name] = gauge
	}

	if logger == nil {
		logger = log.New(os.Stdout, "", log.LstdFlags)
	}

	return &exporter{
		nodeName:  nodeName,
		logger:    logger,
		metrics:   metrics,
		podSeries: make(map[types.UID]map[string]metricSeries),
	}
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

	desired := e.desiredSeriesForPod(pod)
	e.applySeries(pod.UID, desired)
}

func (e *exporter) desiredSeriesForPod(pod *corev1.Pod) map[string]metricSeries {
	if pod == nil || isTerminalPod(pod) {
		return nil
	}

	containerName := resolveContainerName(pod)
	if containerName == "" {
		return nil
	}

	gpuUUID := pod.Annotations[mockGPUUUIDAnnotation]
	if gpuUUID == "" {
		gpuUUID = fmt.Sprintf(defaultMockGPUUUIDLabel, e.nodeName)
	}

	labels := []string{gpuUUID, e.nodeName, pod.Namespace, pod.Name, containerName}
	seriesByKey := make(map[string]metricSeries, len(metricDefinitions))

	for _, def := range metricDefinitions {
		rawValue, ok := pod.Annotations[def.AnnotationKey]
		if !ok || strings.TrimSpace(rawValue) == "" {
			continue
		}

		value, err := strconv.ParseFloat(strings.TrimSpace(rawValue), 64)
		if err != nil {
			e.logger.Printf(
				"skipping invalid annotation %q on %s/%s: %v",
				def.AnnotationName,
				pod.Namespace,
				pod.Name,
				err,
			)
			continue
		}

		series := metricSeries{
			metricName: def.Name,
			labels:     append([]string(nil), labels...),
			value:      value,
		}
		seriesByKey[series.key()] = series
	}

	return seriesByKey
}

func (e *exporter) applySeries(podUID types.UID, desired map[string]metricSeries) {
	e.mu.Lock()
	defer e.mu.Unlock()

	existing := e.podSeries[podUID]
	for key, oldSeries := range existing {
		if _, ok := desired[key]; ok {
			continue
		}
		e.metrics[oldSeries.metricName].DeleteLabelValues(oldSeries.labels...)
		delete(existing, key)
	}

	if len(desired) == 0 {
		delete(e.podSeries, podUID)
		return
	}

	if existing == nil {
		existing = make(map[string]metricSeries, len(desired))
		e.podSeries[podUID] = existing
	}

	for key, series := range desired {
		e.metrics[series.metricName].WithLabelValues(series.labels...).Set(series.value)
		existing[key] = series
	}
}

func (e *exporter) removePodSeries(podUID types.UID) {
	e.mu.Lock()
	defer e.mu.Unlock()

	existing := e.podSeries[podUID]
	for _, series := range existing {
		e.metrics[series.metricName].DeleteLabelValues(series.labels...)
	}
	delete(e.podSeries, podUID)
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
