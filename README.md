# mock-kubex-gpu-exporter

`mock-kubex-gpu-exporter` is a drop-in mock for `gpu-process-exporter`.

It exposes the same Prometheus metric names on `/metrics`, but reads values from pod annotations instead of NVML and host PID inspection. The process is node-local: it watches only pods scheduled to the node named in `NODE_NAME`.

## Metrics

The exporter emits the same gauges and labels as the current real exporter:

- `kubex_gpu_fraction`
- `kubex_gpu_container_memory_bytes`
- `kubex_gpu_container_sm_utilization_percent`
- `kubex_gpu_container_enc_utilization_percent`
- `kubex_gpu_container_dec_utilization_percent`
- `kubex_gpu_container_memory_utilization_percent`
- `kubex_gpu_container_memory_footprint_percent`

Each series uses labels:

- `gpu_uuid`
- `node`
- `namespace`
- `pod`
- `container`

## Annotation contract

These annotations drive the exported values:

- `gpu-fraction`
- `mock.kubex.ai/kubex_gpu_container_memory_bytes`
- `mock.kubex.ai/kubex_gpu_container_sm_utilization_percent`
- `mock.kubex.ai/kubex_gpu_container_enc_utilization_percent`
- `mock.kubex.ai/kubex_gpu_container_dec_utilization_percent`
- `mock.kubex.ai/kubex_gpu_container_memory_utilization_percent`
- `mock.kubex.ai/kubex_gpu_container_memory_footprint_percent`

Optional annotations:

- `gpu-fraction-container-name`: use this container label instead of the first regular container
- `mock.kubex.ai/gpu-uuid`: override the exported `gpu_uuid` label; default is `mock-<NODE_NAME>`

If an annotation is missing, that metric is not exported for the pod. If an annotation value is invalid, that metric is skipped and the old series is removed.

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
    mock.kubex.ai/kubex_gpu_container_memory_bytes: "2147483648"
    mock.kubex.ai/kubex_gpu_container_sm_utilization_percent: "72.5"
    mock.kubex.ai/kubex_gpu_container_memory_utilization_percent: "48"
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
