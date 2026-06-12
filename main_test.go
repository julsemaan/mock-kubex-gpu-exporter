package main

import (
	"bufio"
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

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
	assertMetricAbsentFromText(t, body, "kubex_gpu_container_memory_bytes")
	assertMetricAbsentFromText(t, body, "kubex_gpu_container_memory_total_bytes")
	assertMetricAbsentFromText(t, body, "kubex_gpu_container_sm_utilization_percent_seconds_total")
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
	} {
		if !strings.Contains(body, "# TYPE "+metricName+" gauge") {
			t.Fatalf("expected gauge type for %s:\n%s", metricName, body)
		}
	}
	for _, metricName := range []string{
		"kubex_gpu_container_memory_bytes",
		"kubex_gpu_container_memory_total_bytes",
		"kubex_gpu_container_sm_utilization_percent_seconds_total",
	} {
		if strings.Contains(body, "# TYPE "+metricName+" ") {
			t.Fatalf("unexpected type line for %s:\n%s", metricName, body)
		}
	}
}
