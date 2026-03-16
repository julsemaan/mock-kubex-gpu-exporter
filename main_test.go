package main

import (
	"bytes"
	"log"
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

func testExporter(t *testing.T) (*exporter, *prometheus.Registry) {
	t.Helper()

	registry := prometheus.NewRegistry()
	logger := log.New(&bytes.Buffer{}, "", 0)
	return newExporter("node-a", registry, logger), registry
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
