# mock-kubex-gpu-exporter

`mock-kubex-gpu-exporter` is a focused mock for the subset of `gpu-process-exporter` metrics consumed by `container-data-collection`.

It exposes only the consumed Kubex GPU metrics on `/metrics`, but reads values from pod annotations instead of NVML and host PID inspection. The process is node-local: it watches only pods scheduled to the node named in `NODE_NAME`.

## Metrics

The exporter emits only these metrics:

- `kubex_gpu_container_requests`
- `kubex_gpu_container_limits`
- `kubex_gpu_container_sm_utilization_percent`
- `kubex_gpu_container_memory_footprint_percent`

Container-scoped metrics use labels:

- `node`
- `namespace`
- `pod`
- `container`
- `container_id`
- `gpu_allocation_type`

Per-device metrics additionally use:

- `gpu_uuid`
- `gpu_model`

Behavior notes:

- `gpu-fraction` is exported as both `kubex_gpu_container_requests` and `kubex_gpu_container_limits`
- `mock.kubex.ai/kubex_gpu_container_memory_bytes` and `mock.kubex.ai/kubex_gpu_container_memory_total_bytes` are inputs only; they are used to derive `kubex_gpu_container_memory_footprint_percent`
- `kubex_gpu_container_memory_footprint_percent` is emitted only when both memory annotations are valid and total > 0
- `mock.kubex.ai/kubex_gpu_container_sm_utilization_percent` is exported directly as `kubex_gpu_container_sm_utilization_percent` gauge
- `gpu_allocation_type` is always `KaiScheduler`
- `container_id` is synthesized as `mock://<pod-uid>/<container-name>`

## Annotation contract

These annotations drive the exported values:

- `gpu-fraction`
- `mock.kubex.ai/gpu-model`
- `mock.kubex.ai/kubex_gpu_container_memory_bytes`
- `mock.kubex.ai/kubex_gpu_container_memory_total_bytes`
- `mock.kubex.ai/kubex_gpu_container_sm_utilization_percent`

Optional annotations:

- `gpu-fraction-container-name`: use this container label instead of the first regular container
- `mock.kubex.ai/gpu-uuid`: override the exported `gpu_uuid` label; default is `mock-<NODE_NAME>`

If annotation is missing, metric is not exported for pod. Memory byte annotations remain accepted even though raw byte metrics are not exported. If annotation value is invalid, metric is skipped and old series is removed.

## Run locally

```bash
go run .
```

Environment variables:

- `NODE_NAME` required
- `LISTEN_ADDR` optional, defaults to `:8080`

## Example pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-demo
  namespace: default
  annotations:
    gpu-fraction: "0.25"
    gpu-fraction-container-name: "gpu"
    mock.kubex.ai/gpu-uuid: "GPU-mock-001"
    mock.kubex.ai/gpu-model: "H100"
    mock.kubex.ai/kubex_gpu_container_memory_bytes: "2147483648"
    mock.kubex.ai/kubex_gpu_container_memory_total_bytes: "4294967296"
    mock.kubex.ai/kubex_gpu_container_sm_utilization_percent: "72.5"
spec:
  nodeName: gpu-node-1
  containers:
    - name: sidecar
      image: busybox
      command: ["sh", "-c", "sleep 3600"]
    - name: gpu
      image: busybox
      command: ["sh", "-c", "sleep 3600"]
```

## Deployment

`daemonset.yaml` contains a minimal ServiceAccount, RBAC, DaemonSet, and Service for cluster deployment. Unlike the real exporter, it does not require privileged mode, `hostPID`, or host library mounts.

## Container image

The repository includes a GitHub Actions workflow that builds the exporter image and publishes it to `ghcr.io/<owner>/mock-kubex-gpu-exporter` on pushes to `main` and version tags like `v1.2.3`.
