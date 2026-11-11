// Package topprocessprocessor is an OpenTelemetry Collector processor that
// keeps the per-process metrics of the hostmetrics process scraper for the top
// N processes only. It ranks processes by CPU use, by memory use or by both;
// with both it keeps the union of the CPU top N and the memory top N, and
// drops the metrics of every other process. A resource without process.pid,
// such as the cpu or memory scraper's, passes through unchanged.
//
// It ranks the processes of one batch against each other, so it must see one
// collection at a time: put it before the batch processor.
//
// The process scraper puts the process's identity on the resource
// (process.pid, process.executable.name, ...). Prometheus's OTLP receiver
// does not turn resource attributes into series labels (they go to
// target_info), VictoriaMetrics does. The processor copies
// process.executable.name and process.pid of every process it keeps onto its
// data points, leaving a point attribute that is already there alone, so both
// backends label each series with the process name to group by and the pid
// that keeps two processes of the same name apart; without the pid they would
// write the same label set and collide. VictoriaMetrics, which also turns the
// resource attributes into labels, keeps one label per name and the last one
// given wins (storage.MetricName.sortTags); point attributes come after the
// resource's and carry the same value, so the series stays the same.
package topprocessprocessor
