package catalog

import (
	"fmt"
	"slices"
	"strings"

	"github.com/cloud-barista/ai-o11y/internal/monitoring"
)

// Collectors an expression can come from.
const (
	// CollectorHostmetrics is the OpenTelemetry hostmetrics receiver, sent over
	// OTLP. Its series keep the receiver's metric and attribute names
	// (system.cpu.time, state): the backend stores what collectors send.
	CollectorHostmetrics = "hostmetrics"
	// CollectorNvidiaSMI is the nvidia_gpu receiver built into the collector,
	// reading nvidia-smi; it writes nvidia_smi.<query field>.
	CollectorNvidiaSMI = "nvidia_smi"
	// CollectorAMDSMI is the amd_gpu receiver built into the collector,
	// reading amd-smi metric --json; it writes amd_smi.<path in the JSON>.
	CollectorAMDSMI = "amd_smi"
	// CollectorNVML is the NVML source the nvidia_gpu receiver does not have
	// yet; fields only it can give name it.
	CollectorNVML = "nvml"
)

// Capabilities a row can require.
const (
	RequiresLinux = "os:linux"
)

// Builtin returns the catalog rows ai-o11y ships with. Common fields carry
// the OpenTelemetry semantic conventions' names (v1.45.0) and meaning and say
// how the collectors (hostmetrics, nvidia-smi, amd-smi) provide them;
// collector metrics with no counterpart there are fields of that collector's
// own.
func Builtin() []monitoring.MetricRow {
	var rows []monitoring.MetricRow
	rows = append(rows, cpuRows()...)
	rows = append(rows, memoryRows()...)
	rows = append(rows, pagingRows()...)
	rows = append(rows, diskRows()...)
	rows = append(rows, filesystemRows()...)
	rows = append(rows, networkRows()...)
	rows = append(rows, systemRows()...)
	rows = append(rows, processRows()...)
	rows = append(rows, gpuRows()...)
	return rows
}

func hm(expr string) map[string]string { return map[string]string{CollectorHostmetrics: expr} }

// hmReporting is 0 for every target hostmetrics is reporting from: the CPU
// scraper sends CPU time every scrape. It fills the fields whose hostmetrics
// series is left out when the value is zero.
var hmReporting = map[string]string{
	CollectorHostmetrics: `0 * group by (entity_id) (` + series("system.cpu.time") + `)`,
}

// relabel copies the hostmetrics label from into the attribute label of attr,
// renaming the values whose OpenTelemetry name differs (rename maps the
// hostmetrics value to it).
func relabel(expr, from, attr string, rename map[string]string) string {
	out := fmt.Sprintf(`label_replace(%s, %q, "$1", %q, "(.*)")`, expr, attr, from)
	keys := make([]string, 0, len(rename))
	for k := range rename {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		out = fmt.Sprintf(`label_replace(%s, %q, %q, %q, %q)`, out, attr, rename[k], from, k)
	}
	return out
}

// by joins entity_id with the grouping labels, quoted where PromQL needs it.
func by(labels ...string) string {
	out := []string{"entity_id"}
	for _, l := range labels {
		out = append(out, monitoring.PromQLName(l))
	}
	return strings.Join(out, ", ")
}

// series selects the collector series name for the targets: the matchers, then
// $sel. series("system.cpu.time", `state="idle"`) is
// {"system.cpu.time", state="idle", $sel}.
func series(name string, matchers ...string) string {
	return "{" + strings.Join(append(append([]string{monitoring.PromQLName(name)}, matchers...), monitoring.PlaceholderSelector), ", ") + "}"
}

func cpuRows() []monitoring.MetricRow {
	const time = `rate({"system.cpu.time", $sel}[$range])`
	total := `sum by (entity_id) (` + time + `)`
	idle := `sum by (entity_id) (rate({"system.cpu.time", state="idle", $sel}[$range]))`
	mode := "cpu.mode"
	return []monitoring.MetricRow{
		{
			Name: "system.cpu.utilization", Area: "cpu", Unit: "1", Kind: monitoring.KindRate,
			Description: "Share of CPU time in each cpu.mode. Without one, the share not idle.",
			Exprs:       hm("1 - (" + idle + " / " + total + ")"),
			Attribute: &monitoring.Attribute{Name: "cpu.mode",
				Values: []string{"user", "system", "nice", "idle", "iowait", "interrupt", "steal", "softirq"}},
			SplitExprs: hm(`sum by (` + by(mode) + `) (` + relabel(time, "state", "cpu.mode", map[string]string{"wait": "iowait"}) + `)` +
				` / ignoring (` + monitoring.PromQLName(mode) + `) group_left ` + total),
		},
		{
			Name: "system.cpu.logical.count", Area: "cpu", Unit: "{cpu}", Kind: monitoring.KindGauge,
			Description: "Logical CPUs.",
			Exprs:       hm(`max by (entity_id) ({"system.cpu.logical.count", $sel})`),
		},
		{
			Name: "system.cpu.physical.count", Area: "cpu", Unit: "{cpu}", Kind: monitoring.KindGauge,
			Description: "Physical CPUs.",
			Exprs:       hm(`max by (entity_id) ({"system.cpu.physical.count", $sel})`),
			Enables:     []string{"system.cpu.physical.count"},
		},
	}
}

func memoryRows() []monitoring.MetricRow {
	state := "system.memory.state"
	split := func(metric string) string {
		return `sum by (` + by(state) + `) (` + relabel(series(metric), "state", "system.memory.state",
			map[string]string{"buffered": "buffers"}) + `)`
	}
	attr := &monitoring.Attribute{Name: "system.memory.state",
		Values: []string{"used", "free", "buffers", "cached", "inactive", "slab_reclaimable", "slab_unreclaimable"}}
	return []monitoring.MetricRow{
		{
			Name: "system.memory.usage", Area: "memory", Unit: "By", Kind: monitoring.KindGauge,
			Description: "Memory in each system.memory.state. Without one, memory used.",
			Exprs:       hm(`sum by (entity_id) ({"system.memory.usage", state="used", $sel})`),
			Attribute:   attr, SplitExprs: hm(split("system.memory.usage")),
		},
		{
			Name: "system.memory.limit", Area: "memory", Unit: "By", Kind: monitoring.KindGauge,
			Description: "Total memory.",
			Exprs:       hm(`sum by (entity_id) ({"system.memory.limit", $sel})`),
			Enables:     []string{"system.memory.limit"},
		},
		{
			Name: "system.memory.utilization", Area: "memory", Unit: "1", Kind: monitoring.KindGauge,
			Description: "Share of memory in each system.memory.state. Without one, the share used.",
			Exprs:       hm(`sum by (entity_id) ({"system.memory.utilization", state="used", $sel})`),
			Attribute:   attr, SplitExprs: hm(split("system.memory.utilization")),
			Enables: []string{"system.memory.utilization"},
		},
		{
			Name: "system.memory.linux.available", Area: "memory", Unit: "By", Kind: monitoring.KindGauge,
			Description: "Memory available for new work (MemAvailable).",
			Exprs:       hm(`sum by (entity_id) ({"system.linux.memory.available", $sel})`),
			Requires:    []string{RequiresLinux},
			Enables:     []string{"system.linux.memory.available"},
		},
		{
			Name: "system.memory.linux.shared", Area: "memory", Unit: "By", Kind: monitoring.KindGauge,
			Description: "Shared memory (Shmem).",
			Exprs:       hm(`sum by (entity_id) ({"system.memory.linux.shared", $sel})`),
			Requires:    []string{RequiresLinux},
			Enables:     []string{"system.memory.linux.shared"},
		},
	}
}

func pagingRows() []monitoring.MetricRow {
	const usage = "system.paging.usage"
	state := "system.paging.state"
	total := `sum by (entity_id) (` + series(usage) + `)`
	byState := `sum by (` + by(state) + `) (` + relabel(series(usage), "state", "system.paging.state", nil) + `)`
	attr := &monitoring.Attribute{Name: "system.paging.state", Values: []string{"used", "free", "cached"}}
	// hostmetrics sends no paging usage on a host without swap.
	return []monitoring.MetricRow{
		{
			Name: "system.paging.usage", Area: "paging", Unit: "By", Kind: monitoring.KindGauge,
			Description: "Swap space in each system.paging.state. Without one, swap used.",
			Exprs:       hm(`sum by (entity_id) (` + series(usage, `state="used"`) + `)`),
			Attribute:   attr, SplitExprs: hm(byState), ZeroFill: hmReporting,
		},
		{
			Name: "system.paging.utilization", Area: "paging", Unit: "1", Kind: monitoring.KindGauge,
			Description: "Share of swap space in each system.paging.state. Without one, the share used.",
			Exprs:       hm(`sum by (entity_id) (` + series(usage, `state="used"`) + `) / ` + total),
			Attribute:   attr, SplitExprs: hm(byState + ` / ignoring (` + monitoring.PromQLName(state) + `) group_left ` + total),
			ZeroFill: hmReporting,
		},
		{
			Name: "system.paging.faults", Area: "paging", Unit: "{fault}/s", Kind: monitoring.KindRate,
			Description: "Page faults.",
			Gap:         "hostmetrics reports Linux page faults multiplied by 4096 (gopsutil), not a count of faults",
		},
		{
			Name: "system.paging.operations", Area: "paging", Unit: "{operation}/s", Kind: monitoring.KindRate,
			Description: "Paging operations.",
			Gap:         "hostmetrics reports the bytes swapped as its major paging operations on Linux, not a count of operations",
		},
	}
}

// directionSplit is a rate field split by a direction attribute that
// hostmetrics carries in its direction label with the same values.
func directionSplit(name, area, unit, desc, metric, dim, dimName, attr string, values []string) monitoring.MetricRow {
	r := `rate(` + series(metric) + `[$range])`
	return monitoring.MetricRow{
		Name: name, Area: area, Unit: unit, Kind: monitoring.KindRate, Description: desc,
		Exprs:     hm(`sum by (` + by(dim) + `) (` + r + `)`),
		Attribute: &monitoring.Attribute{Name: attr, Values: values},
		SplitExprs: hm(`sum by (` + by(dim, attr) + `) (` +
			relabel(r, "direction", attr, nil) + `)`),
		Dimension: dim, DimensionName: dimName,
	}
}

func diskRows() []monitoring.MetricRow {
	rw := []string{"read", "write"}
	const dev, devName, dir = "device", "system.device.name", "disk.io.direction"
	return []monitoring.MetricRow{
		directionSplit("system.disk.io", "disk", "By/s", "Bytes read and written. Without disk.io.direction, both.",
			"system.disk.io", dev, devName, dir, rw),
		directionSplit("system.disk.operations", "disk", "{operation}/s", "Completed reads and writes. Without disk.io.direction, both.",
			"system.disk.operations", dev, devName, dir, rw),
		directionSplit("system.disk.operation_time", "disk", "s/s", "Time spent in reads and writes. Without disk.io.direction, both.",
			"system.disk.operation_time", dev, devName, dir, rw),
		directionSplit("system.disk.merged", "disk", "{operation}/s", "Merged reads and writes. Without disk.io.direction, both.",
			"system.disk.merged", dev, devName, dir, rw),
		{
			Name: "system.disk.io_time", Area: "disk", Unit: "s/s", Kind: monitoring.KindRate,
			Description: "Time the device was busy, per second: its busy share.",
			Exprs:       hm(`sum by (entity_id, device) (rate({"system.disk.io_time", $sel}[$range]))`),
			Dimension:   dev, DimensionName: devName,
		},
		{
			Name: "system.disk.pending_operations", Schema: CollectorHostmetrics, Area: "disk", Unit: "{operation}",
			Kind: monitoring.KindGauge, Description: "I/O operations in progress.",
			Exprs:     hm(`sum by (entity_id, device) ({"system.disk.pending_operations", $sel})`),
			Dimension: dev, DimensionName: "device",
		},
	}
}

func filesystemRows() []monitoring.MetricRow {
	const usage, mnt, mntName = "system.filesystem.usage", "mountpoint", "system.filesystem.mountpoint"
	state := "system.filesystem.state"
	limit := `sum by (entity_id, mountpoint) (` + series(usage) + `)`
	byState := `sum by (` + by(mnt, state) + `) (` + relabel(series(usage), "state", "system.filesystem.state", nil) + `)`
	used := `sum by (entity_id, mountpoint) (` + series(usage, `state="used"`) + `)`
	attr := &monitoring.Attribute{Name: "system.filesystem.state", Values: []string{"used", "free", "reserved"}}
	return []monitoring.MetricRow{
		{
			Name: "system.filesystem.usage", Area: "filesystem", Unit: "By", Kind: monitoring.KindGauge,
			Description: "Space in each system.filesystem.state. Without one, space used.",
			Exprs:       hm(used), Attribute: attr, SplitExprs: hm(byState),
			Dimension: mnt, DimensionName: mntName,
		},
		{
			Name: "system.filesystem.limit", Area: "filesystem", Unit: "By", Kind: monitoring.KindGauge,
			Description: "File system size, reserved space included.",
			Exprs:       hm(limit), Dimension: mnt, DimensionName: mntName,
		},
		{
			Name: "system.filesystem.utilization", Area: "filesystem", Unit: "1", Kind: monitoring.KindGauge,
			Description: "Share of the file system size in each system.filesystem.state. Without one, the share used.",
			Exprs:       hm(used + " / " + limit), Attribute: attr,
			SplitExprs: hm(byState + ` / ignoring (` + monitoring.PromQLName(state) + `) group_left ` + limit),
			Dimension:  mnt, DimensionName: mntName,
		},
		{
			Name: "system.filesystem.inodes.usage", Schema: CollectorHostmetrics, Area: "filesystem", Unit: "{inode}",
			Kind: monitoring.KindGauge, Description: "Inodes in each state. Without one, inodes used.",
			Exprs:      hm(`sum by (entity_id, mountpoint) ({"system.filesystem.inodes.usage", state="used", $sel})`),
			Attribute:  &monitoring.Attribute{Name: "state", Values: []string{"used", "free"}},
			SplitExprs: hm(`sum by (entity_id, mountpoint, state) ({"system.filesystem.inodes.usage", $sel})`),
			Dimension:  mnt, DimensionName: mnt, Requires: []string{RequiresLinux},
		},
	}
}

func networkRows() []monitoring.MetricRow {
	tr := []string{"transmit", "receive"}
	const dev, ifName, dir = "device", "network.interface.name", "network.io.direction"
	return []monitoring.MetricRow{
		directionSplit("system.network.io", "network", "By/s", "Bytes sent and received. Without network.io.direction, both.",
			"system.network.io", dev, ifName, dir, tr),
		directionSplit("system.network.packet.count", "network", "{packet}/s", "Packets sent and received. Without network.io.direction, both.",
			"system.network.packets", dev, "system.device.name", dir, tr),
		directionSplit("system.network.errors", "network", "{error}/s", "Errors sending and receiving. Without network.io.direction, both.",
			"system.network.errors", dev, ifName, dir, tr),
		directionSplit("system.network.packet.dropped", "network", "{packet}/s", "Packets dropped. Without network.io.direction, both.",
			"system.network.dropped", dev, ifName, dir, tr),
	}
}

func systemRows() []monitoring.MetricRow {
	load := func(window string) monitoring.MetricRow {
		return monitoring.MetricRow{
			Name: "system.cpu.load_average." + window, Schema: CollectorHostmetrics, Area: "system", Unit: "{thread}",
			Kind: monitoring.KindGauge, Description: "Load average over " + window + ".",
			Exprs:    hm(`max by (entity_id) (` + series("system.cpu.load_average."+window) + `)`),
			Requires: []string{RequiresLinux},
		}
	}
	return []monitoring.MetricRow{
		{
			Name: "system.uptime", Area: "system", Unit: "s", Kind: monitoring.KindGauge,
			Description: "Time since boot.",
			Exprs:       hm(`max by (entity_id) ({"system.uptime", $sel})`),
		},
		load("1m"), load("5m"), load("15m"),
	}
}

func processRows() []monitoring.MetricRow {
	pstate := "process.state"
	const gap = "per-process collection is waiting on how process ids are kept (top-N in the collector or grouping by name)"
	return []monitoring.MetricRow{
		{
			Name: "system.process.count", Area: "process", Unit: "{process}", Kind: monitoring.KindGauge,
			Description: "Processes in each process.state. Without one, all processes.",
			Exprs:       hm(`sum by (entity_id) ({"system.processes.count", $sel})`),
			Attribute: &monitoring.Attribute{Name: "process.state",
				Values: []string{"running", "sleeping", "stopped", "defunct", "blocked", "idle"}},
			SplitExprs: hm(`sum by (` + by(pstate) + `) (` + relabel(`{"system.processes.count", $sel}`, "status", "process.state",
				map[string]string{"zombies": "defunct"}) + `)`),
			Requires: []string{RequiresLinux},
			// hostmetrics leaves a status with no processes out.
			ZeroFill: hmReporting,
		},
		{Name: "process.cpu.utilization", Area: "process", Unit: "1", Kind: monitoring.KindRate,
			Description: "CPU use of a process, one core = 1.", Dimension: "process", DimensionName: "process.executable.name", Gap: gap},
		{Name: "process.memory.utilization", Area: "process", Unit: "1", Kind: monitoring.KindGauge,
			Description: "Memory share of a process.", Dimension: "process", DimensionName: "process.executable.name", Gap: gap},
	}
}

// nvidiaSMI is the series name the nvidia_gpu receiver's nvidia-smi source
// writes a query field under: nvidia_smi.<field>.
func nvidiaSMI(field string) string { return CollectorNvidiaSMI + "." + field }

// amdSMI is the series name the amd_gpu receiver's amd-smi source writes a
// value under: amd_smi.<path in amd-smi metric --json>.
func amdSMI(path string) string { return CollectorAMDSMI + "." + path }

// smi maps a GPU field to its nvidia-smi expression.
func smi(expr string) map[string]string { return map[string]string{CollectorNvidiaSMI: expr} }

// perGPU is the value of a GPU receiver's series per GPU.
func perGPU(name string) string {
	return `max by (entity_id, uuid) (` + series(name) + `)`
}

// gpuRows are the GPU fields. The common ones carry the OpenTelemetry
// hardware semantic conventions' names (hw.gpu.*, hw.power), split within a
// target by hw.id, the GPU's UUID; the rest are nvidia-smi's own query fields
// and amd-smi's own values.
func gpuRows() []monitoring.MetricRow {
	const dim, dimName = "uuid", "hw.id"
	task := "hw.gpu.task"
	util := func(field string) string { return perGPU(nvidiaSMI(field)) + " / 100" }
	byTask := func(field, value string) string {
		return fmt.Sprintf(`label_replace(%s, %q, %q, "", "")`, util(field), task, value)
	}
	const mib = " * 1048576"
	used, total := perGPU(nvidiaSMI("memory.used")), perGPU(nvidiaSMI("memory.total"))

	rows := []monitoring.MetricRow{
		{
			Name: "hw.gpu.utilization", Area: "gpu", Unit: "1", Kind: monitoring.KindGauge,
			Description: "Share of time the GPU spent on each hw.gpu.task. Without one, general work (nvidia-smi utilization.gpu).",
			Exprs:       smi(util("utilization.gpu")),
			Attribute:   &monitoring.Attribute{Name: "hw.gpu.task", Values: []string{"general", "encoder", "decoder"}},
			SplitExprs: smi(`sum by (` + by("uuid", task) + `) (` + byTask("utilization.gpu", "general") + ` or ` +
				byTask("utilization.encoder", "encoder") + ` or ` + byTask("utilization.decoder", "decoder") + `)`),
			Dimension: dim, DimensionName: dimName,
		},
		{
			Name: "hw.gpu.memory.usage", Area: "gpu", Unit: "By", Kind: monitoring.KindGauge,
			Description: "GPU memory used.", Exprs: smi(used + mib), Dimension: dim, DimensionName: dimName,
		},
		{
			Name: "hw.gpu.memory.limit", Area: "gpu", Unit: "By", Kind: monitoring.KindGauge,
			Description: "Size of the GPU memory.", Exprs: smi(total + mib), Dimension: dim, DimensionName: dimName,
		},
		{
			Name: "hw.gpu.memory.utilization", Area: "gpu", Unit: "1", Kind: monitoring.KindGauge,
			Description: "Share of the GPU memory used.", Exprs: smi(used + " / " + total), Dimension: dim, DimensionName: dimName,
		},
		{
			Name: "hw.power", Area: "gpu", Unit: "W", Kind: monitoring.KindGauge,
			Description: "Power drawn by the GPU (hw.type gpu).",
			Exprs:       smi(perGPU(nvidiaSMI("power.draw"))), Dimension: dim, DimensionName: dimName,
		},
	}

	// nvidia-smi's own fields, under its query field names.
	own := []struct{ field, unit, desc string }{
		{"utilization.memory", "%", "Share of time the memory was read or written."},
		{"temperature.gpu", "Cel", "GPU core temperature."},
		{"temperature.memory", "Cel", "GPU memory temperature."},
		{"fan.speed", "%", "Fan speed the GPU asks for."},
		{"clocks.sm", "MHz", "SM clock."},
		{"clocks.mem", "MHz", "Memory clock."},
		{"pstate", "", "Performance state, 0 (P0, highest) to 15 (P15, lowest)."},
		{"clocks_event_reasons.active", "", "Bit mask of the reasons the clocks are held down."},
		{"ecc.errors.corrected.volatile.total", "{error}", "Corrected ECC errors since the driver loaded."},
		{"ecc.errors.uncorrected.volatile.total", "{error}", "Uncorrected ECC errors since the driver loaded."},
		{"ecc.errors.corrected.aggregate.total", "{error}", "Corrected ECC errors over the GPU's life."},
		{"ecc.errors.uncorrected.aggregate.total", "{error}", "Uncorrected ECC errors over the GPU's life."},
		{"retired_pages.double_bit.count", "{page}", "Pages retired for double-bit ECC errors."},
		{"retired_pages.pending", "", "1 when a page waits to be retired at the next reboot."},
		{"remapped_rows.correctable", "{row}", "Rows remapped for correctable errors."},
		{"remapped_rows.uncorrectable", "{row}", "Rows remapped for uncorrectable errors."},
		{"remapped_rows.pending", "", "1 when a row remap waits for a GPU reset."},
		{"remapped_rows.failure", "", "1 when a row remap failed."},
	}
	for _, f := range own {
		rows = append(rows, monitoring.MetricRow{
			Name: f.field, Schema: CollectorNvidiaSMI, Area: "gpu", Unit: f.unit, Kind: monitoring.KindGauge,
			Description: f.desc, Exprs: smi(perGPU(nvidiaSMI(f.field))), Dimension: dim, DimensionName: dim,
		})
	}

	// amd-smi's own fields, under their path in amd-smi metric --json.
	amdOwn := []struct{ field, unit, desc string }{
		{"usage.umc_activity", "%", "Share of time the memory controller was busy."},
		{"usage.mm_activity", "%", "Share of time the multimedia engine was busy."},
		{"temperature.edge", "Cel", "GPU temperature at the edge of the die."},
		{"temperature.hotspot", "Cel", "Hottest point of the GPU die (junction)."},
		{"temperature.mem", "Cel", "GPU memory temperature."},
		{"clock.gfx_0.clk", "MHz", "Clock of the first graphics domain."},
		{"clock.mem_0.clk", "MHz", "Memory clock."},
		{"mem_usage.used_gtt", "MiBy", "Host memory mapped for the GPU (GTT) in use."},
		{"mem_usage.total_gtt", "MiBy", "Size of the host memory the GPU can map (GTT)."},
		{"energy.total_energy_consumption", "J", "Energy the GPU has used, a counter that only grows until it resets."},
		{"pcie.replay_count", "", "PCIe packets replayed, a counter."},
		{"ecc.total_correctable_count", "{error}", "Correctable ECC errors the driver counted."},
		{"ecc.total_uncorrectable_count", "{error}", "Uncorrectable ECC errors the driver counted."},
		{"ecc.total_deferred_count", "{error}", "Deferred ECC errors the driver counted."},
	}
	for _, f := range amdOwn {
		rows = append(rows, monitoring.MetricRow{
			Name: f.field, Schema: CollectorAMDSMI, Area: "gpu", Unit: f.unit, Kind: monitoring.KindGauge,
			Description: f.desc, Exprs: map[string]string{CollectorAMDSMI: perGPU(amdSMI(f.field))}, Dimension: dim, DimensionName: dim,
		})
	}

	// NVIDIA fields only NVML gives; the receiver has no NVML source yet.
	const nvml = "the GPU receiver has no NVML source yet; nvidia-smi does not report it"
	rows = append(rows,
		monitoring.MetricRow{Name: "mig.sm.utilization", Schema: CollectorNVML, Area: "gpu", Unit: "1",
			Kind: monitoring.KindGauge, Description: "SM activity of a MIG instance.", Gap: nvml},
		monitoring.MetricRow{Name: "xid.errors", Schema: CollectorNVML, Area: "gpu", Unit: "{error}",
			Kind: monitoring.KindGauge, Description: "XID errors the driver reported.", Gap: nvml},
	)
	return rows
}
