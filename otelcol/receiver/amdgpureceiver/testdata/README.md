# amd-smi output samples

| File | Command | Source | Changes |
|---|---|---|---|
| `mi300x-dpx-metric.json` | `amd-smi metric --json`, an MI300X split into DPX partitions | Attached for reference by an amdsmi contributor to ROCm/amdsmi issue 134 (https://github.com/ROCm/amdsmi/issues/134) | Only the first two entries (gpu 0 and gpu 1) kept; values unchanged |
| `metric-power-rocm61.json` | `amd-smi metric -p --json`, the ROCm 6.1 format: a bare array | The "amd_smi_lib for ROCm 6.1.0" section of the amdsmi CHANGELOG (ROCm/rocm-systems, `projects/amdsmi/CHANGELOG.md`) | None |
| `static-mi300a-doc.json` | `amd-smi static`, MI300A | The "Example output from amd-smi static" of the amdsmi CLI documentation (`projects/amdsmi/docs/how-to/amdsmi-cli-tool.md`), moved from the human-readable output into the JSON keys | No JSON sample was found; the keys are those of the CLI's JSON. `asic_serial` is masked in the documentation |
| `list-v710-rocm10.json` | `amd-smi list --json`, Radeon PRO V710 MxGPU, ROCm 10.0 | Captured on a V710 virtual function | Only the first entry kept |
| `metric-v710-recorded.json` | `amd-smi metric --json`, AMD Radeon Pro V710 MxGPU | Recorded on an Azure NV4ads_V710_v5 VM (SR-IOV virtual function), Ubuntu 24.04, as root; AMDSMI Tool 27.0.0+6b0e43f3, AMDSMI Library 27.0.0 (amd-smi version reports the ROCm version as N/A) | None. `usage` is the string "N/A" as a whole; power, temperature, clocks and PCIe are "N/A"; only `mem_usage` and the ECC totals are numbers |
| `static-asic-v710-recorded.json` | `amd-smi static --asic --json`, same VM | Recorded on an Azure NV4ads_V710_v5 VM (SR-IOV virtual function), Ubuntu 24.04, as root; AMDSMI Tool 27.0.0+6b0e43f3, AMDSMI Library 27.0.0 (amd-smi version reports the ROCm version as N/A) | None |
| `list-v710-recorded.json` | `amd-smi list --json`, same VM | Recorded on an Azure NV4ads_V710_v5 VM (SR-IOV virtual function), Ubuntu 24.04, as root; AMDSMI Tool 27.0.0+6b0e43f3, AMDSMI Library 27.0.0 (amd-smi version reports the ROCm version as N/A) | None |

`mem_usage`'s "MB" is bytes divided by 1024 * 1024 in the amd-smi CLI
(`amdsmi_cli/subcommands/metric.py`), so the receiver reads it as MiB.
