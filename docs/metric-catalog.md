# Metric catalog

ai-o11y answers queries in terms of the fields of its metric catalog
(`internal/monitoring/catalog`), not in the names each collector writes.
`GET /aio11y/metrics` lists the catalog; `GET /aio11y/targets/{targetId}/metrics`
lists it for one target.

## Principles

1. **Collectors keep their own names.** The OpenTelemetry hostmetrics receiver,
   `nvidia-smi`, `amd-smi` or NVML write metrics under the names they define. ai-o11y does
   not rename them at collection time.
2. **The catalog maps them to fields.** Each field says, per collector, which
   PromQL computes it from that collector's series.
   - A **common field** means the same whichever collector provides it. Its
     name and its meaning are those of the
     [OpenTelemetry semantic conventions](https://github.com/open-telemetry/semantic-conventions)
     (v1.45.0), such as `system.cpu.utilization`. Where a collector's value
     differs from what the convention defines, the field is not provided for
     that collector.
   - A **collector's own field** has no counterpart in the semantic
     conventions, such as hostmetrics' load average. It keeps the collector's
     name, and its `schema` names the collector, which tells why its shape
     differs. A query names it with `metric` and `schema`.
3. **Every field states whether a target has it.** A field no collector
   provides yet carries the reason (`gap`). For a target, a field is
   unsupported when none of the collectors running on it provide it, or when
   the target lacks what the field requires (such as Linux).

## Attributes

Following OpenTelemetry, a field is split by attributes rather than by name:
`system.cpu.utilization` is one field, and `cpu.mode` (`user`, `system`,
`idle`, ...) splits it.

- Without `where` or `splitBy`, a query answers the field's **default value**,
  the one usually shown, which the field's description states: for
  `system.cpu.utilization`, the share of CPU time not idle.
- `"where": {"cpu.mode": "user"}` answers that value of the attribute.
- `"splitBy": ["cpu.mode"]` answers one series per value.
- A **dimension** splits a field within one target, such as
  `system.device.name` for disks; `within` collapses it.

Answers carry `entity_id` and the attributes by their OpenTelemetry names.
Series keep the metric and attribute names collectors send (`system.cpu.time`,
`state`), so the backend must store them unchanged; inside PromQL a name with
dots is quoted (`{"system.cpu.time"}`, `by ("cpu.mode")`).

## Zero values

Some collectors leave a series out when its value is zero, such as hostmetrics
for a process state with no processes or paging on a host without swap. A field
that may be missing for that reason has `zeroFill`; a query with
`"fillZero": true` answers 0 for those targets that are otherwise reporting.
The default is off.

## Fields

GPU fields come from ai-o11y's NVIDIA GPU receiver (`otelcol/receiver/nvidiagpureceiver`), which reads `nvidia-smi` and writes each query field as `nvidia_smi.<field>`, and its AMD GPU receiver (`otelcol/receiver/amdgpureceiver`), which reads `amd-smi metric --json` and writes each value as `amd_smi.<path in the JSON>`. Both put the GPU's `index`, `uuid` and `name` on every point. Within a target GPU fields are split by the GPU's UUID, `hw.id` for common fields and `uuid` for a collector's own.

The AMD GPU receiver writes these, leaving out a value amd-smi gives as `N/A` or in another unit (amd-smi's `MB` is MiB):

| Metric | Unit |
|---|---|
| `amd_smi.usage.gfx_activity`, `amd_smi.usage.umc_activity`, `amd_smi.usage.mm_activity` | % |
| `amd_smi.mem_usage.{total,used,free}_vram`, `amd_smi.mem_usage.{total,used,free}_visible_vram`, `amd_smi.mem_usage.{total,used,free}_gtt` | MiBy |
| `amd_smi.power.socket_power` | W |
| `amd_smi.power.gfx_voltage`, `amd_smi.power.soc_voltage`, `amd_smi.power.mem_voltage` | mV |
| `amd_smi.energy.total_energy_consumption` | J |
| `amd_smi.temperature.edge`, `amd_smi.temperature.hotspot`, `amd_smi.temperature.mem` | Cel |
| `amd_smi.clock.<domain>.clk`, one per clock domain (`gfx_0`, `mem_0`, `vclk_0`, ...) | MHz |
| `amd_smi.pcie.width`, `amd_smi.pcie.replay_count` |  |
| `amd_smi.ecc.total_correctable_count`, `amd_smi.ecc.total_uncorrectable_count`, `amd_smi.ecc.total_deferred_count` |  |

| Field | Schema | Unit | Attribute (values) | Dimension | Provided |
|---|---|---|---|---|---|
| `system.cpu.utilization` | common | 1 | `cpu.mode` (user, system, nice, idle, iowait, interrupt, steal, softirq) |  | yes |
| `system.cpu.logical.count` | common | {cpu} |  |  | yes |
| `system.cpu.physical.count` | common | {cpu} |  |  | yes |
| `system.memory.usage` | common | By | `system.memory.state` (used, free, buffers, cached, inactive, slab_reclaimable, slab_unreclaimable) |  | yes |
| `system.memory.limit` | common | By |  |  | yes |
| `system.memory.utilization` | common | 1 | `system.memory.state` (used, free, buffers, cached, inactive, slab_reclaimable, slab_unreclaimable) |  | yes |
| `system.memory.linux.available` | common | By |  |  | yes |
| `system.memory.linux.shared` | common | By |  |  | yes |
| `system.paging.usage` | common | By | `system.paging.state` (used, free, cached) |  | yes |
| `system.paging.utilization` | common | 1 | `system.paging.state` (used, free, cached) |  | yes |
| `system.paging.faults` | common | {fault}/s |  |  | no: hostmetrics reports Linux page faults multiplied by 4096 (gopsutil), not a count of faults |
| `system.paging.operations` | common | {operation}/s |  |  | no: hostmetrics reports the bytes swapped as its major paging operations on Linux, not a count of operations |
| `system.disk.io` | common | By/s | `disk.io.direction` (read, write) | `system.device.name` | yes |
| `system.disk.operations` | common | {operation}/s | `disk.io.direction` (read, write) | `system.device.name` | yes |
| `system.disk.operation_time` | common | s/s | `disk.io.direction` (read, write) | `system.device.name` | yes |
| `system.disk.merged` | common | {operation}/s | `disk.io.direction` (read, write) | `system.device.name` | yes |
| `system.disk.io_time` | common | s/s |  | `system.device.name` | yes |
| `system.disk.pending_operations` | hostmetrics | {operation} |  | `device` | yes |
| `system.filesystem.usage` | common | By | `system.filesystem.state` (used, free, reserved) | `system.filesystem.mountpoint` | yes |
| `system.filesystem.limit` | common | By |  | `system.filesystem.mountpoint` | yes |
| `system.filesystem.utilization` | common | 1 | `system.filesystem.state` (used, free, reserved) | `system.filesystem.mountpoint` | yes |
| `system.filesystem.inodes.usage` | hostmetrics | {inode} | `state` (used, free) | `mountpoint` | yes |
| `system.network.io` | common | By/s | `network.io.direction` (transmit, receive) | `network.interface.name` | yes |
| `system.network.packet.count` | common | {packet}/s | `network.io.direction` (transmit, receive) | `system.device.name` | yes |
| `system.network.errors` | common | {error}/s | `network.io.direction` (transmit, receive) | `network.interface.name` | yes |
| `system.network.packet.dropped` | common | {packet}/s | `network.io.direction` (transmit, receive) | `network.interface.name` | yes |
| `system.uptime` | common | s |  |  | yes |
| `system.cpu.load_average.1m` | hostmetrics | {thread} |  |  | yes |
| `system.cpu.load_average.5m` | hostmetrics | {thread} |  |  | yes |
| `system.cpu.load_average.15m` | hostmetrics | {thread} |  |  | yes |
| `system.process.count` | common | {process} | `process.state` (running, sleeping, stopped, defunct, blocked, idle) |  | yes |
| `process.cpu.utilization` | common | 1 |  | `process.executable.name` | no: per-process collection is waiting on how process ids are kept (top-N in the collector or grouping by name) |
| `process.memory.utilization` | common | 1 |  | `process.executable.name` | no: per-process collection is waiting on how process ids are kept (top-N in the collector or grouping by name) |
| `hw.gpu.utilization` | common | 1 | `hw.gpu.task` (general, encoder, decoder) | `hw.id` | yes |
| `hw.gpu.memory.usage` | common | By |  | `hw.id` | yes |
| `hw.gpu.memory.limit` | common | By |  | `hw.id` | yes |
| `hw.gpu.memory.utilization` | common | 1 |  | `hw.id` | yes |
| `hw.power` | common | W |  | `hw.id` | yes |
| `utilization.memory` | nvidia_smi | % |  | `uuid` | yes |
| `temperature.gpu` | nvidia_smi | Cel |  | `uuid` | yes |
| `temperature.memory` | nvidia_smi | Cel |  | `uuid` | yes |
| `fan.speed` | nvidia_smi | % |  | `uuid` | yes |
| `clocks.sm` | nvidia_smi | MHz |  | `uuid` | yes |
| `clocks.mem` | nvidia_smi | MHz |  | `uuid` | yes |
| `pstate` | nvidia_smi |  |  | `uuid` | yes |
| `clocks_event_reasons.active` | nvidia_smi |  |  | `uuid` | yes |
| `ecc.errors.corrected.volatile.total` | nvidia_smi | {error} |  | `uuid` | yes |
| `ecc.errors.uncorrected.volatile.total` | nvidia_smi | {error} |  | `uuid` | yes |
| `ecc.errors.corrected.aggregate.total` | nvidia_smi | {error} |  | `uuid` | yes |
| `ecc.errors.uncorrected.aggregate.total` | nvidia_smi | {error} |  | `uuid` | yes |
| `retired_pages.double_bit.count` | nvidia_smi | {page} |  | `uuid` | yes |
| `retired_pages.pending` | nvidia_smi |  |  | `uuid` | yes |
| `remapped_rows.correctable` | nvidia_smi | {row} |  | `uuid` | yes |
| `remapped_rows.uncorrectable` | nvidia_smi | {row} |  | `uuid` | yes |
| `remapped_rows.pending` | nvidia_smi |  |  | `uuid` | yes |
| `remapped_rows.failure` | nvidia_smi |  |  | `uuid` | yes |
| `usage.umc_activity` | amd_smi | % |  | `uuid` | yes |
| `usage.mm_activity` | amd_smi | % |  | `uuid` | yes |
| `temperature.edge` | amd_smi | Cel |  | `uuid` | yes |
| `temperature.hotspot` | amd_smi | Cel |  | `uuid` | yes |
| `temperature.mem` | amd_smi | Cel |  | `uuid` | yes |
| `clock.gfx_0.clk` | amd_smi | MHz |  | `uuid` | yes |
| `clock.mem_0.clk` | amd_smi | MHz |  | `uuid` | yes |
| `mem_usage.used_gtt` | amd_smi | MiBy |  | `uuid` | yes |
| `mem_usage.total_gtt` | amd_smi | MiBy |  | `uuid` | yes |
| `energy.total_energy_consumption` | amd_smi | J |  | `uuid` | yes |
| `pcie.replay_count` | amd_smi |  |  | `uuid` | yes |
| `ecc.total_correctable_count` | amd_smi | {error} |  | `uuid` | yes |
| `ecc.total_uncorrectable_count` | amd_smi | {error} |  | `uuid` | yes |
| `ecc.total_deferred_count` | amd_smi | {error} |  | `uuid` | yes |
| `mig.sm.utilization` | nvml | 1 |  |  | no: the GPU receiver has no NVML source yet; nvidia-smi does not report it |
| `xid.errors` | nvml | {error} |  |  | no: the GPU receiver has no NVML source yet; nvidia-smi does not report it |

## Known limits

- The semantic conventions for these fields are mostly at development or
  release-candidate stability; names and attributes may still change there.
- The meaning of a common field follows OpenTelemetry. Where it differs from
  the fields shown by existing screens (for example, CPU user time with or
  without guest time), the difference is not reconciled yet.
- The NVIDIA GPU receiver has only an `nvidia-smi` source: MIG and XID fields
  wait for an NVML source. GPUs of vendors other than NVIDIA and AMD are not
  read.
- The common GPU fields (`hw.gpu.*`, `hw.power`) are computed from `nvidia-smi`
  only: a query renders one collector's expression per field, so AMD GPUs give
  their `amd_smi` fields alone until it can combine two.
- Fields whose value only the existing screens compute, such as average I/O
  wait per operation, are not in the catalog yet.
