// Package amdgpureceiver is an OpenTelemetry Collector receiver for AMD GPUs.
// It reads GPU samples from a source and emits them under the source's own
// names: the amd-smi source runs `amd-smi metric --json` and names each metric
// after its path in that document, amd_smi.<section>.<key>
// (amd_smi.usage.gfx_activity), with the GPU's index, uuid and name as
// attributes, the same keys the NVIDIA GPU receiver uses. Mapping these to
// common fields is ai-o11y's metric catalog's job, not the receiver's.
package amdgpureceiver
