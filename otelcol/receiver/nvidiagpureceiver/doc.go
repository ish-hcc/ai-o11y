// Package nvidiagpureceiver is an OpenTelemetry Collector receiver for NVIDIA
// GPUs. It reads GPU samples from a source and emits them under the source's
// own names: the nvidia-smi source runs nvidia-smi and names each metric after
// its query field, nvidia_smi.<field> (nvidia_smi.utilization.gpu), with the
// GPU's index, uuid and name as attributes. Mapping these to common fields is
// ai-o11y's metric catalog's job, not the receiver's.
//
// With xid enabled it also counts the XID errors the NVIDIA driver logs to the
// kernel log (/dev/kmsg), as nvidia_kmsg.xid.errors: a cumulative count since
// boot per GPU and XID number, the GPU found by the PCI address the line names.
//
// Sources are swappable: nvidia-smi is the first, an NVML source can follow
// without touching the receiver.
package nvidiagpureceiver
