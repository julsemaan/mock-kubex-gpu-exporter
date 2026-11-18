package main

import (
	"bufio"
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/tools/cache"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	corev1 "k8s.io/api/core/v1"
)

func TestReconcileAnnotatedPodExportsConsumedMetrics(t *testing.T) {
	exp, registry := testExporter(t)

	p := pod("default", "gpu-app", "node-a", map[string]string{
		gpuFractionAnnotation:            "0.25",
		mockGPUUUIDAnnotation:            "GPU-123",
		mockGPUModelAnnotation:           "H100",
		metricMemoryBytesAnnotation:      "2048",
		metricMemoryTotalBytesAnnotation: "4096",
		metricSMUtilPercentAnnotation:    "73.5",
	}, "main")

	exp.reconcilePod(p)
	body := scrapeMetrics(t, exp.metricsHandler(registry))

	containerLabels := map[string]string{
		"node":                "node-a",
		"namespace":           "default",
		"pod":                 "gpu-app",
		"container":           "main",
		"container_id":        "mock://default/gpu-app/main",
		"gpu_allocation_type": kaiSchedulerAllocationType,
	}
	perDeviceLabels := copyLabels(containerLabels)
	perDeviceLabels["gpu_uuid"] = "GPU-123"
	perDeviceLabels["gpu_model"] = "H100"

	assertMetricValueFromText(t, body, metricRequestsName, containerLabels, 0.25)
	assertMetricValueFromText(t, body, metricLimitsName, containerLabels, 0.25)
	assertMetricValueFromText(t, body, metricMemoryFootprintName, perDeviceLabels, 50)
	assertMetricValueFromText(t, body, metricSMUtilName, perDeviceLabels, 73.5)

	assertMetricAbsentFromText(t, body, "kubex_gpu_fraction")
	assertMetricValueFromText(t, body, "kubex_gpu_container_memory_bytes", perDeviceLabels, 2048)
	assertMetricValueFromText(t, body, "kubex_gpu_container_memory_total_bytes", perDeviceLabels, 4096)
	assertMetricAbsentFromText(t, body, "kubex_gpu_container_memory_utilization_percent")
}

func TestDefaultsIncludeSyntheticLabels(t *testing.T) {
	exp, registry := testExporter(t)

	p := pod("default", "gpu-app", "node-a", map[string]string{
		gpuFractionAnnotation:            "0.5",
		metricMemoryBytesAnnotation:      "1024",
		metricMemoryTotalBytesAnnotation: "2048",
		metricSMUtilPercentAnnotation:    "17",
	}, "main")

	exp.reconcilePod(p)
	body := scrapeMetrics(t, exp.metricsHandler(registry))

	labels := map[string]string{
		"node":                "node-a",
		"namespace":           "default",
		"pod":                 "gpu-app",
		"container":           "main",
		"container_id":        "mock://default/gpu-app/main",
		"gpu_allocation_type": kaiSchedulerAllocationType,
		"gpu_uuid":            "mock-node-a",
		"gpu_model":           defaultMockGPUModelLabel,
	}

	assertMetricValueFromText(t, body, metricMemoryFootprintName, labels, 50)
	assertMetricValueFromText(t, body, metricSMUtilName, labels, 17)
}

func TestUpdateChangesValuesAndDeletesRemovedMetrics(t *testing.T) {
	exp, registry := testExporter(t)

	p := pod("default", "gpu-app", "node-a", map[string]string{
		gpuFractionAnnotation:            "0.25",
		metricMemoryBytesAnnotation:      "2048",
		metricMemoryTotalBytesAnnotation: "4096",
		metricSMUtilPercentAnnotation:    "33",
	}, "main")
	exp.reconcilePod(p)

	updated := p.DeepCopy()
	updated.Annotations = map[string]string{
		metricMemoryBytesAnnotation:      "1024",
		metricMemoryTotalBytesAnnotation: "2048",
	}
	exp.reconcilePod(updated)
	body := scrapeMetrics(t, exp.metricsHandler(registry))

	containerLabels := map[string]string{
		"node":                "node-a",
		"namespace":           "default",
		"pod":                 "gpu-app",
		"container":           "main",
		"container_id":        "mock://default/gpu-app/main",
		"gpu_allocation_type": kaiSchedulerAllocationType,
	}
	perDeviceLabels := copyLabels(containerLabels)
	perDeviceLabels["gpu_uuid"] = "mock-node-a"
	perDeviceLabels["gpu_model"] = defaultMockGPUModelLabel

	assertMetricAbsentFromText(t, body, metricRequestsName)
	assertMetricValueFromText(t, body, metricMemoryFootprintName, perDeviceLabels, 50)
	assertMetricAbsentFromText(t, body, metricSMUtilName)
}

func TestInvalidAnnotationDoesNotLeakStaleMetric(t *testing.T) {
	exp, registry := testExporter(t)

	p := pod("default", "gpu-app", "node-a", map[string]string{
		metricMemoryBytesAnnotation:      "1024",
		metricMemoryTotalBytesAnnotation: "2048",
		metricSMUtilPercentAnnotation:    "10",
	}, "main")
	exp.reconcilePod(p)

	updated := p.DeepCopy()
	updated.Annotations[metricMemoryTotalBytesAnnotation] = "nope"
	updated.Annotations[metricSMUtilPercentAnnotation] = "bad"
	exp.reconcilePod(updated)
	body := scrapeMetrics(t, exp.metricsHandler(registry))

	assertMetricAbsentFromText(t, body, metricMemoryFootprintName)
	assertMetricAbsentFromText(t, body, metricSMUtilName)
}

func TestMissingOrZeroMemoryTotalSkipsFootprint(t *testing.T) {
	t.Run("missing total", func(t *testing.T) {
		exp, registry := testExporter(t)
		exp.reconcilePod(pod("default", "gpu-app", "node-a", map[string]string{
			metricMemoryBytesAnnotation: "1024",
		}, "main"))
		body := scrapeMetrics(t, exp.metricsHandler(registry))
		assertMetricAbsentFromText(t, body, metricMemoryFootprintName)
	})

	t.Run("zero total", func(t *testing.T) {
		exp, registry := testExporter(t)
		exp.reconcilePod(pod("default", "gpu-app", "node-a", map[string]string{
			metricMemoryBytesAnnotation:      "1024",
			metricMemoryTotalBytesAnnotation: "0",
		}, "main"))
		body := scrapeMetrics(t, exp.metricsHandler(registry))
		assertMetricAbsentFromText(t, body, metricMemoryFootprintName)
	})
}

func TestDeleteRemovesSeries(t *testing.T) {
	exp, registry := testExporter(t)

	p := pod("default", "gpu-app", "node-a", map[string]string{
		gpuFractionAnnotation: "0.4",
	}, "main")
	exp.reconcilePod(p)
	exp.onDelete(p)

	body := scrapeMetrics(t, exp.metricsHandler(registry))
	assertMetricAbsentFromText(t, body, metricRequestsName)
}

func TestTerminalPodRemovesMetrics(t *testing.T) {
	exp, registry := testExporter(t)

	p := pod("default", "gpu-app", "node-a", map[string]string{
		gpuFractionAnnotation: "0.75",
	}, "main")
	exp.reconcilePod(p)

	completed := p.DeepCopy()
	completed.Status.Phase = corev1.PodSucceeded
	exp.reconcilePod(completed)

	body := scrapeMetrics(t, exp.metricsHandler(registry))
	assertMetricAbsentFromText(t, body, metricRequestsName)
}

func TestResolvesAnnotatedContainerName(t *testing.T) {
	exp, registry := testExporter(t)

	p := pod("default", "gpu-app", "node-a", map[string]string{
		gpuFractionAnnotation: "0.5",
		targetContainerAnnot:  "gpu",
	}, "sidecar", "gpu")
	exp.reconcilePod(p)
	body := scrapeMetrics(t, exp.metricsHandler(registry))

	assertMetricValueFromText(t, body, metricRequestsName, map[string]string{
		"node":                "node-a",
		"namespace":           "default",
		"pod":                 "gpu-app",
		"container":           "gpu",
		"container_id":        "mock://default/gpu-app/gpu",
		"gpu_allocation_type": kaiSchedulerAllocationType,
	}, 0.5)
}

const cumulativeTestMetric = "kubex_gpu_container_sm_utilization_percent_seconds_total"

func TestCumulativeUtilizationUsesElapsedTimeAndOldValue(t *testing.T) {
	for _, frequent := range []bool{false, true} {
		t.Run(strconv.FormatBool(frequent), func(t *testing.T) {
			now := time.Unix(100, 0)
			exp, registry := testExporterWithOptions(t, exporterOptions{now: func() time.Time { return now }})
			handler := exp.metricsHandler(registry)
			p := pod("default", "gpu-app", "node-a", map[string]string{metricSMUtilPercentAnnotation: "25"}, "main")
			exp.onAdd(p)
			assertMetricValueFromText(t, scrapeMetrics(t, handler), cumulativeTestMetric, defaultTestLabels(), 0)
			for i := 0; i < 4; i++ {
				now = now.Add(250 * time.Millisecond)
				if frequent {
					_ = scrapeMetrics(t, handler)
				}
			}
			p.Annotations[metricSMUtilPercentAnnotation] = "75"
			exp.onUpdate(nil, p)
			now = now.Add(2 * time.Second)
			assertMetricValueFromText(t, scrapeMetrics(t, handler), cumulativeTestMetric, defaultTestLabels(), 175)
			assertMetricValueFromText(t, scrapeMetrics(t, handler), cumulativeTestMetric, defaultTestLabels(), 175)
			exp.onUpdate(nil, p)
			now = now.Add(time.Second)
			assertMetricValueFromText(t, scrapeMetrics(t, handler), cumulativeTestMetric, defaultTestLabels(), 250)
		})
	}
}

func TestTelemetryCleanupAndRestart(t *testing.T) {
	for _, action := range []string{"annotation removal", "invalid", "NaN", "+Inf", "-Inf", "succeeded", "failed", "delete", "tombstone", "pointer tombstone", "node move"} {
		t.Run(action, func(t *testing.T) {
			now := time.Unix(100, 0)
			exp, registry := testExporterWithOptions(t, exporterOptions{now: func() time.Time { return now }})
			p := pod("default", "gpu-app", "node-a", map[string]string{
				metricSMUtilPercentAnnotation: "50", metricMemoryBytesAnnotation: "10", metricMemoryTotalBytesAnnotation: "20",
			}, "main")
			exp.onAdd(p)
			now = now.Add(time.Second)
			assertMetricValueFromText(t, scrapeMetrics(t, exp.metricsHandler(registry)), cumulativeTestMetric, defaultTestLabels(), 50)
			updated := p.DeepCopy()
			switch action {
			case "annotation removal":
				updated.Annotations = nil
			case "invalid", "NaN", "+Inf", "-Inf":
				for key := range updated.Annotations {
					updated.Annotations[key] = action
				}
			case "succeeded":
				updated.Status.Phase = corev1.PodSucceeded
			case "failed":
				updated.Status.Phase = corev1.PodFailed
			case "delete":
				exp.onDelete(p)
			case "tombstone":
				exp.onDelete(cache.DeletedFinalStateUnknown{Obj: p})
			case "pointer tombstone":
				exp.onDelete(&cache.DeletedFinalStateUnknown{Obj: p})
			case "node move":
				updated.Spec.NodeName = "node-b"
			}
			if action != "delete" && !strings.Contains(action, "tombstone") {
				exp.onUpdate(p, updated)
			}
			now = now.Add(time.Hour)
			body := scrapeMetrics(t, exp.metricsHandler(registry))
			for _, name := range []string{metricSMUtilName, cumulativeTestMetric, "kubex_gpu_container_memory_bytes", "kubex_gpu_container_memory_total_bytes", metricMemoryFootprintName} {
				assertMetricAbsentFromText(t, body, name)
			}
			exp.onAdd(p)
			assertMetricValueFromText(t, scrapeMetrics(t, exp.metricsHandler(registry)), cumulativeTestMetric, defaultTestLabels(), 0)
		})
	}
}

func TestIdentityChangesRemoveOldSeries(t *testing.T) {
	for _, identity := range []string{"uuid", "model", "container"} {
		t.Run(identity, func(t *testing.T) {
			now := time.Unix(100, 0)
			exp, registry := testExporterWithOptions(t, exporterOptions{now: func() time.Time { return now }})
			p := pod("default", "gpu-app", "node-a", map[string]string{metricSMUtilPercentAnnotation: "20", metricMemoryBytesAnnotation: "10", metricMemoryTotalBytesAnnotation: "20"}, "main", "gpu")
			exp.onAdd(p)
			now = now.Add(time.Second)
			labels := defaultTestLabels()
			switch identity {
			case "uuid":
				p.Annotations[mockGPUUUIDAnnotation] = "new-uuid"
				labels["gpu_uuid"] = "new-uuid"
			case "model":
				p.Annotations[mockGPUModelAnnotation] = "new-model"
				labels["gpu_model"] = "new-model"
			case "container":
				p.Annotations[targetContainerAnnot] = "gpu"
				labels["container"] = "gpu"
				labels["container_id"] = "mock://default/gpu-app/gpu"
			}
			exp.onUpdate(nil, p)
			now = now.Add(2 * time.Second)
			body := scrapeMetrics(t, exp.metricsHandler(registry))
			for name, value := range map[string]float64{cumulativeTestMetric: 40, metricSMUtilName: 20, "kubex_gpu_container_memory_bytes": 10, "kubex_gpu_container_memory_total_bytes": 20, metricMemoryFootprintName: 50} {
				assertMetricValueFromText(t, body, name, labels, value)
				if strings.Count(body, name+"{") != 1 {
					t.Fatalf("old %s identity still present:\n%s", name, body)
				}
			}
		})
	}
}

func TestMemoryByteGaugesAreIndependent(t *testing.T) {
	exp, registry := testExporter(t)
	p := pod("default", "gpu-app", "node-a", map[string]string{metricMemoryBytesAnnotation: "1024"}, "main")
	exp.onAdd(p)
	body := scrapeMetrics(t, exp.metricsHandler(registry))
	assertMetricValueFromText(t, body, "kubex_gpu_container_memory_bytes", defaultTestLabels(), 1024)
	assertMetricAbsentFromText(t, body, "kubex_gpu_container_memory_total_bytes")
	assertMetricAbsentFromText(t, body, metricMemoryFootprintName)
	delete(p.Annotations, metricMemoryBytesAnnotation)
	p.Annotations[metricMemoryTotalBytesAnnotation] = "0"
	exp.onUpdate(nil, p)
	body = scrapeMetrics(t, exp.metricsHandler(registry))
	assertMetricAbsentFromText(t, body, "kubex_gpu_container_memory_bytes")
	assertMetricValueFromText(t, body, "kubex_gpu_container_memory_total_bytes", defaultTestLabels(), 0)
	assertMetricAbsentFromText(t, body, metricMemoryFootprintName)
}

func TestConcurrentCollectionAndUpdates(t *testing.T) {
	exp, registry := testExporter(t)
	p := pod("default", "gpu-app", "node-a", map[string]string{metricSMUtilPercentAnnotation: "50"}, "main")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			exp.onAdd(p)
			exp.onDelete(p)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if _, err := registry.Gather(); err != nil {
				t.Errorf("concurrent gather: %v", err)
			}
		}
	}()
	wg.Wait()
}

func TestWholeGPUResourcesAndFractionOverride(t *testing.T) {
	for _, tc := range []struct {
		name, fraction, target string
		requests, limits       float64
	}{
		{"first container", "", "", 2, 3},
		{"selected container", "", "gpu", 4, 5},
		{"fraction overrides", "0.25", "gpu", 0.25, 0.25},
		{"fraction clamps", "2", "gpu", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exp, registry := testExporter(t)
			p := pod("default", "gpu-app", "node-a", map[string]string{targetContainerAnnot: tc.target}, "main", "gpu")
			p.Spec.Containers[0].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("2")},
				Limits:   corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("3")},
			}
			p.Spec.Containers[1].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")},
				Limits:   corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("5")},
			}
			if tc.fraction != "" {
				p.Annotations[gpuFractionAnnotation] = tc.fraction
			}
			exp.reconcilePod(p)
			body := scrapeMetrics(t, exp.metricsHandler(registry))
			labels := defaultTestLabels()
			delete(labels, "gpu_uuid")
			delete(labels, "gpu_model")
			if tc.target != "" {
				labels["container"] = tc.target
				labels["container_id"] = "mock://default/gpu-app/" + tc.target
			}
			assertMetricValueFromText(t, body, metricRequestsName, labels, tc.requests)
			assertMetricValueFromText(t, body, metricLimitsName, labels, tc.limits)
			delete(p.Annotations, gpuFractionAnnotation)
			p.Spec.Containers[0].Resources.Requests = nil
			p.Spec.Containers[1].Resources.Requests = nil
			exp.reconcilePod(p)
			body = scrapeMetrics(t, exp.metricsHandler(registry))
			assertMetricAbsentFromText(t, body, metricRequestsName)
			wantLimit := 3.0
			if tc.target != "" {
				wantLimit = 5
			}
			assertMetricValueFromText(t, body, metricLimitsName, labels, wantLimit)
		})
	}
}

func defaultTestLabels() map[string]string {
	return map[string]string{
		"node": "node-a", "namespace": "default", "pod": "gpu-app", "container": "main",
		"container_id": "mock://default/gpu-app/main", "gpu_allocation_type": kaiSchedulerAllocationType,
		"gpu_uuid": "mock-node-a", "gpu_model": defaultMockGPUModelLabel,
	}
}

func TestNonfiniteAnnotationsRemoveMetrics(t *testing.T) {
	for _, invalid := range []string{"NaN", "+Inf", "-Inf", "bad", ""} {
		t.Run(invalid, func(t *testing.T) {
			exp, registry := testExporter(t)
			p := pod("default", "gpu-app", "node-a", map[string]string{
				gpuFractionAnnotation: "0.5", metricMemoryBytesAnnotation: "10",
				metricMemoryTotalBytesAnnotation: "20", metricSMUtilPercentAnnotation: "50",
			}, "main")
			exp.reconcilePod(p)
			for key := range p.Annotations {
				p.Annotations[key] = invalid
			}
			exp.reconcilePod(p)
			body := scrapeMetrics(t, exp.metricsHandler(registry))
			for _, name := range []string{metricRequestsName, metricLimitsName, metricMemoryFootprintName, metricSMUtilName,
				"kubex_gpu_container_memory_bytes", "kubex_gpu_container_memory_total_bytes", "kubex_gpu_container_sm_utilization_percent_seconds_total"} {
				assertMetricAbsentFromText(t, body, name)
			}
		})
	}
}

func testExporter(t *testing.T) (*exporter, *prometheus.Registry) {
	t.Helper()
	return testExporterWithOptions(t, exporterOptions{})
}

func testExporterWithOptions(t *testing.T, opts exporterOptions) (*exporter, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	logger := log.New(&bytes.Buffer{}, "", 0)
	return newExporter("node-a", registry, logger, opts), registry
}

func scrapeMetrics(t *testing.T, handler http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want %d", recorder.Code, http.StatusOK)
	}
	return recorder.Body.String()
}

func assertMetricValueFromText(t *testing.T, body, metricName string, wantLabels map[string]string, wantValue float64) {
	t.Helper()
	if got := metricValueFromText(t, body, metricName, wantLabels); got != wantValue {
		t.Fatalf("metric %s value = %v, want %v", metricName, got, wantValue)
	}
}

func metricValueFromText(t *testing.T, body, metricName string, wantLabels map[string]string) float64 {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(body))
	prefix := metricName + "{"

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		labelsPart, valuePart, ok := strings.Cut(strings.TrimPrefix(line, metricName), "} ")
		if !ok || !strings.HasPrefix(labelsPart, "{") {
			continue
		}
		labels := parseMetricLabels(t, strings.TrimPrefix(labelsPart, "{"))
		if !sameLabels(labels, wantLabels) {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(valuePart), 64)
		if err != nil {
			t.Fatalf("parse metric value %q: %v", valuePart, err)
		}
		return value
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan metrics: %v", err)
	}
	t.Fatalf("metric %s with labels %v not found", metricName, wantLabels)
	return 0
}

func assertMetricAbsentFromText(t *testing.T, body, metricName string) {
	t.Helper()
	if strings.Contains(body, metricName+"{") {
		t.Fatalf("metric %s unexpectedly present in scrape:\n%s", metricName, body)
	}
}

func parseMetricLabels(t *testing.T, raw string) map[string]string {
	t.Helper()
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	labels := make(map[string]string, len(parts))
	for _, part := range parts {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			t.Fatalf("invalid label pair %q", part)
		}
		labels[key] = strings.Trim(value, "\"")
	}
	return labels
}

func sameLabels(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			return false
		}
	}
	return true
}

func copyLabels(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func TestMetricTypes(t *testing.T) {
	exp, registry := testExporter(t)
	p := pod("default", "gpu-app", "node-a", map[string]string{
		gpuFractionAnnotation:            "0.25",
		metricMemoryBytesAnnotation:      "2048",
		metricMemoryTotalBytesAnnotation: "4096",
		metricSMUtilPercentAnnotation:    "10",
	}, "main")
	exp.reconcilePod(p)
	_ = scrapeMetrics(t, exp.metricsHandler(registry))

	var buf bytes.Buffer
	encoder := expfmt.NewEncoder(&buf, expfmt.FmtText)
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if err := encoder.Encode(family); err != nil {
			t.Fatalf("encode metric family: %v", err)
		}
	}
	body := buf.String()
	for _, metricName := range []string{
		metricRequestsName,
		metricLimitsName,
		metricSMUtilName,
		metricMemoryFootprintName,
		"kubex_gpu_container_memory_bytes",
		"kubex_gpu_container_memory_total_bytes",
	} {
		if !strings.Contains(body, "# TYPE "+metricName+" gauge") {
			t.Fatalf("expected gauge type for %s:\n%s", metricName, body)
		}
	}
	if !strings.Contains(body, "# TYPE kubex_gpu_container_sm_utilization_percent_seconds_total counter") {
		t.Fatalf("expected cumulative utilization counter:\n%s", body)
	}
}
