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

func TestReconcileAnnotatedPodExportsExpectedMetrics(t *testing.T) {
	exp, registry := testExporter(t)

	p := pod("default", "gpu-app", "node-a", map[string]string{
		gpuFractionAnnotation: "0.25",
		mockGPUUUIDAnnotation: "GPU-123",
		mockAnnotationPrefix + "kubex_gpu_container_memory_bytes":           "2048",
		mockAnnotationPrefix + "kubex_gpu_container_sm_utilization_percent": "73.5",
	}, "main")

	exp.reconcilePod(p)

	assertGaugeValue(t, registry, "kubex_gpu_fraction", map[string]string{
		"gpu_uuid":  "GPU-123",
		"node":      "node-a",
		"namespace": "default",
		"pod":       "gpu-app",
		"container": "main",
	}, 0.25)
	assertGaugeValue(t, registry, "kubex_gpu_container_memory_bytes", map[string]string{
		"gpu_uuid":  "GPU-123",
		"node":      "node-a",
		"namespace": "default",
		"pod":       "gpu-app",
		"container": "main",
	}, 2048)
	assertGaugeValue(t, registry, "kubex_gpu_container_sm_utilization_percent", map[string]string{
		"gpu_uuid":  "GPU-123",
		"node":      "node-a",
		"namespace": "default",
		"pod":       "gpu-app",
		"container": "main",
	}, 73.5)
}

func TestUpdateChangesValuesAndDeletesRemovedMetrics(t *testing.T) {
	exp, registry := testExporter(t)

	p := pod("default", "gpu-app", "node-a", map[string]string{
		gpuFractionAnnotation: "0.25",
		mockAnnotationPrefix + "kubex_gpu_container_memory_bytes": "2048",
	}, "main")
	exp.reconcilePod(p)

	updated := p.DeepCopy()
	updated.Annotations = map[string]string{
		mockAnnotationPrefix + "kubex_gpu_container_memory_bytes": "1024",
	}
	exp.reconcilePod(updated)

	assertGaugeAbsent(t, registry, "kubex_gpu_fraction")
	assertGaugeValue(t, registry, "kubex_gpu_container_memory_bytes", map[string]string{
		"gpu_uuid":  "mock-node-a",
		"node":      "node-a",
		"namespace": "default",
		"pod":       "gpu-app",
		"container": "main",
	}, 1024)
}

func TestDeleteRemovesSeries(t *testing.T) {
	exp, registry := testExporter(t)

	p := pod("default", "gpu-app", "node-a", map[string]string{
		gpuFractionAnnotation: "0.4",
	}, "main")
	exp.reconcilePod(p)
	exp.onDelete(p)

	assertGaugeAbsent(t, registry, "kubex_gpu_fraction")
}

func TestResolvesAnnotatedContainerName(t *testing.T) {
	exp, registry := testExporter(t)

	p := pod("default", "gpu-app", "node-a", map[string]string{
		gpuFractionAnnotation: "0.5",
		targetContainerAnnot:  "gpu",
	}, "sidecar", "gpu")
	exp.reconcilePod(p)

	assertGaugeValue(t, registry, "kubex_gpu_fraction", map[string]string{
		"gpu_uuid":  "mock-node-a",
		"node":      "node-a",
		"namespace": "default",
		"pod":       "gpu-app",
		"container": "gpu",
	}, 0.5)
}

func TestInvalidAnnotationDoesNotLeakStaleMetric(t *testing.T) {
	exp, registry := testExporter(t)

	p := pod("default", "gpu-app", "node-a", map[string]string{
		mockAnnotationPrefix + "kubex_gpu_container_memory_utilization_percent": "42",
	}, "main")
	exp.reconcilePod(p)

	updated := p.DeepCopy()
	updated.Annotations[mockAnnotationPrefix+"kubex_gpu_container_memory_utilization_percent"] = "nope"
	exp.reconcilePod(updated)

	assertGaugeAbsent(t, registry, "kubex_gpu_container_memory_utilization_percent")
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

	assertGaugeAbsent(t, registry, "kubex_gpu_fraction")
}

func TestMetricsHandlerWithoutJitterEmitsAnnotationValues(t *testing.T) {
	exp, registry := testExporter(t)

	p := pod("default", "gpu-app", "node-a", map[string]string{
		gpuFractionAnnotation: "0.25",
		mockAnnotationPrefix + "kubex_gpu_container_memory_bytes":           "2048",
		mockAnnotationPrefix + "kubex_gpu_container_sm_utilization_percent": "73.5",
	}, "main")
	exp.reconcilePod(p)

	body := scrapeMetrics(t, exp.metricsHandler(registry))

	assertMetricValueFromText(t, body, "kubex_gpu_fraction", map[string]string{
		"gpu_uuid":  "mock-node-a",
		"node":      "node-a",
		"namespace": "default",
		"pod":       "gpu-app",
		"container": "main",
	}, 0.25)
	assertMetricValueFromText(t, body, "kubex_gpu_container_memory_bytes", map[string]string{
		"gpu_uuid":  "mock-node-a",
		"node":      "node-a",
		"namespace": "default",
		"pod":       "gpu-app",
		"container": "main",
	}, 2048)
	assertMetricValueFromText(t, body, "kubex_gpu_container_sm_utilization_percent", map[string]string{
		"gpu_uuid":  "mock-node-a",
		"node":      "node-a",
		"namespace": "default",
		"pod":       "gpu-app",
		"container": "main",
	}, 73.5)
}

func TestMetricsHandlerWithJitterResamplesEligibleMetrics(t *testing.T) {
	exp, registry := testExporterWithOptions(t, exporterOptions{
		jitterDelta: 10,
		randValue: sequenceFloat64(
			0.0, 0.25,
			0.999999, 0.75,
		),
	})

	p := pod("default", "gpu-app", "node-a", map[string]string{
		gpuFractionAnnotation: "0.5",
		mockAnnotationPrefix + "kubex_gpu_container_memory_bytes":           "2048",
		mockAnnotationPrefix + "kubex_gpu_container_sm_utilization_percent": "50",
	}, "main")
	exp.reconcilePod(p)

	handler := exp.metricsHandler(registry)
	firstBody := scrapeMetrics(t, handler)
	secondBody := scrapeMetrics(t, handler)

	labels := map[string]string{
		"gpu_uuid":  "mock-node-a",
		"node":      "node-a",
		"namespace": "default",
		"pod":       "gpu-app",
		"container": "main",
	}
	firstFraction := metricValueFromText(t, firstBody, "kubex_gpu_fraction", labels)
	secondFraction := metricValueFromText(t, secondBody, "kubex_gpu_fraction", labels)
	firstPercent := metricValueFromText(t, firstBody, "kubex_gpu_container_sm_utilization_percent", labels)
	secondPercent := metricValueFromText(t, secondBody, "kubex_gpu_container_sm_utilization_percent", labels)

	if firstFraction == secondFraction {
		t.Fatalf("fraction metric did not change between scrapes: %v", firstFraction)
	}
	if firstPercent == secondPercent {
		t.Fatalf("percent metric did not change between scrapes: %v", firstPercent)
	}
	if got := metricValueFromText(t, firstBody, "kubex_gpu_container_memory_bytes", labels); got != 2048 {
		t.Fatalf("memory bytes on first scrape = %v, want 2048", got)
	}
	if got := metricValueFromText(t, secondBody, "kubex_gpu_container_memory_bytes", labels); got != 2048 {
		t.Fatalf("memory bytes on second scrape = %v, want 2048", got)
	}
}

func TestMetricsHandlerJitterClampsToValidRanges(t *testing.T) {
	exp, registry := testExporterWithOptions(t, exporterOptions{
		jitterDelta: 20,
		randValue: sequenceFloat64(
			0.999999, 0.999999,
		),
	})

	p := pod("default", "gpu-app", "node-a", map[string]string{
		gpuFractionAnnotation: "0.95",
		mockAnnotationPrefix + "kubex_gpu_container_sm_utilization_percent": "95",
	}, "main")
	exp.reconcilePod(p)

	body := scrapeMetrics(t, exp.metricsHandler(registry))
	labels := map[string]string{
		"gpu_uuid":  "mock-node-a",
		"node":      "node-a",
		"namespace": "default",
		"pod":       "gpu-app",
		"container": "main",
	}

	assertMetricValueFromText(t, body, "kubex_gpu_fraction", labels, 1)
	assertMetricValueFromText(t, body, "kubex_gpu_container_sm_utilization_percent", labels, 100)
}

func TestLoadMetricJitterDelta(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    float64
		wantErr bool
	}{
		{name: "unset", want: 0},
		{name: "zero", value: "0", want: 0},
		{name: "positive", value: "3.5", want: 3.5},
		{name: "negative", value: "-1", wantErr: true},
		{name: "invalid", value: "abc", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value == "" {
				t.Setenv(metricJitterAbsoluteDeltaEnv, "")
			} else {
				t.Setenv(metricJitterAbsoluteDeltaEnv, tc.value)
			}

			got, err := loadMetricJitterDelta()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("loadMetricJitterDelta returned error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("loadMetricJitterDelta = %v, want %v", got, tc.want)
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

func assertGaugeValue(t *testing.T, registry *prometheus.Registry, metricName string, wantLabels map[string]string, wantValue float64) {
	t.Helper()

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	for _, family := range families {
		if family.GetName() != metricName {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := make(map[string]string, len(metric.GetLabel()))
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			if sameLabels(labels, wantLabels) {
				if metric.GetGauge().GetValue() != wantValue {
					t.Fatalf("metric %s value = %v, want %v", metricName, metric.GetGauge().GetValue(), wantValue)
				}
				return
			}
		}
	}

	t.Fatalf("metric %s with labels %v not found", metricName, wantLabels)
}

func assertGaugeAbsent(t *testing.T, registry *prometheus.Registry, metricName string) {
	t.Helper()

	var buf bytes.Buffer
	encoder := expfmt.NewEncoder(&buf, expfmt.FmtText)
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != metricName {
			continue
		}
		if len(family.GetMetric()) > 0 {
			if err := encoder.Encode(family); err != nil {
				t.Fatalf("encode metric family: %v", err)
			}
			t.Fatalf("metric %s unexpectedly present:\n%s", metricName, buf.String())
		}
		return
	}
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

func sequenceFloat64(values ...float64) func() float64 {
	index := 0
	return func() float64 {
		if len(values) == 0 {
			return 0
		}
		if index >= len(values) {
			return values[len(values)-1]
		}
		value := values[index]
		index++
		return value
	}
}
