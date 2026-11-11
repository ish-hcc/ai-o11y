package query

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/cloud-barista/ai-o11y/internal/monitoring"
	"github.com/cloud-barista/ai-o11y/internal/monitoring/catalog"
)

func renderer(t *testing.T) PromQL {
	t.Helper()
	c, err := catalog.New(catalog.Builtin())
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return PromQL{Catalog: c}
}

func targets(ids ...string) monitoring.TargetSet {
	set := make(monitoring.TargetSet, len(ids))
	for i, id := range ids {
		set[i] = monitoring.Target{Kind: "vm", EntityID: id, Name: id}
	}
	return set
}

// cpuUtil is the catalog expression of system.cpu.utilization with the
// placeholders filled.
func cpuUtil(sel, rng string) string {
	return `1 - (sum by (entity_id) (rate({"system.cpu.time", state="idle", ` + sel + `}[` + rng + `]))` +
		` / sum by (entity_id) (rate({"system.cpu.time", ` + sel + `}[` + rng + `])))`
}

const abc = `entity_id=~"a|b|c"`

// The examples below are current screens, expressed as a QuerySpec. Pattern
// numbers refer to the survey of the current queries.

// TestRenderCurrentScreens covers the instant-query patterns of the current
// screens.
func TestRenderCurrentScreens(t *testing.T) {
	r := renderer(t)
	hour := monitoring.Agg{Fn: monitoring.AggAvg, Range: time.Hour}
	for name, tc := range map[string]struct {
		q    monitoring.QuerySpec
		want string
	}{
		// Q2: mean per VM over the last duration (VM overview).
		"Q2 mean per target": {
			monitoring.QuerySpec{Metric: "system.cpu.utilization", Targets: targets("a", "b", "c"), Window: hour},
			cpuUtil(abc, "1h"),
		},
		// Q6: a VM's graph with a point per interval.
		"Q6 graph": {
			monitoring.QuerySpec{
				Metric: "system.cpu.utilization", Targets: targets("a", "b", "c"),
				Window: monitoring.Agg{Fn: monitoring.AggAvg, Range: 5 * time.Minute}, Step: 5 * time.Minute,
				Start: time.Unix(0, 0), End: time.Unix(0, 0).Add(24 * time.Hour),
			},
			cpuUtil(abc, "5m"),
		},
		// Q4: sum inside a target, here the used space of every mount point.
		"Q4 within target": {
			monitoring.QuerySpec{
				Metric: "system.filesystem.usage", Targets: targets("a", "b"), Window: hour,
				Within: &monitoring.Reduce{Label: "mountpoint", Fn: monitoring.AggSum},
			},
			`sum by (entity_id) (avg_over_time((sum by (entity_id, mountpoint) ({"system.filesystem.usage", state="used", entity_id=~"a|b"}))[1h:30s]))`,
		},
		// Q5: top VMs by the mean over the duration (VM overview top 5).
		"Q5 top-k": {
			monitoring.QuerySpec{
				Metric: "system.cpu.utilization", Targets: targets("a", "b", "c"), Window: hour,
				TopK: &monitoring.TopK{N: 5, Desc: true},
			},
			`topk(5, ` + cpuUtil(abc, "1h") + `)`,
		},
		// Q8: one value for a set of targets the mapping layer resolved.
		"Q8 across the set": {
			monitoring.QuerySpec{
				Metric: "system.cpu.utilization", Targets: targets("a", "b", "c"), Window: hour,
				Across: monitoring.AggAvg,
			},
			`avg(` + cpuUtil(abc, "1h") + `)`,
		},
	} {
		got, err := r.Render(tc.q)
		if err != nil {
			t.Errorf("%s: Render: %v", name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", name, got, tc.want)
		}
	}
}

// TestRenderRateRangeFollowsStep: a graph's rate covers one graph interval,
// as the current screens derive each counter from one interval to the next,
// but never less than two collection intervals.
func TestRenderRateRangeFollowsStep(t *testing.T) {
	r := testRenderer(t)
	for step, want := range map[time.Duration]string{
		30 * time.Second: "1m",
		time.Minute:      "1m",
		10 * time.Minute: "10m",
	} {
		got, err := r.Render(monitoring.QuerySpec{Metric: "system.cpu.utilization", Targets: targets("a"), Step: step})
		if err != nil {
			t.Fatalf("Render step %v: %v", step, err)
		}
		if w := cpuUtil(`entity_id="a"`, want); got != w {
			t.Errorf("step %v:\n got %s\nwant %s", step, got, w)
		}
	}
}

// TestRenderTopKTwoStage covers the graph of the top series (GPU dashboard
// top 5): the series are ranked over the whole graph range, as the current
// screen ranks by the same window it draws, and then drawn.
func TestRenderTopKTwoStage(t *testing.T) {
	r := renderer(t)
	start := time.Unix(0, 0)
	q := monitoring.QuerySpec{
		Metric: "system.cpu.utilization", Targets: targets("c", "a", "b"),
		Window: monitoring.Agg{Fn: monitoring.AggAvg, Range: 5 * time.Minute},
		Step:   5 * time.Minute, Start: start, End: start.Add(24 * time.Hour),
		TopK: &monitoring.TopK{N: 5, Desc: true},
	}
	if _, err := r.Render(q); !errors.Is(err, ErrTwoStage) {
		t.Fatalf("Render of a graph top-k: err = %v, want ErrTwoStage", err)
	}
	pick, err := r.RenderTopK(q)
	if err != nil {
		t.Fatalf("RenderTopK: %v", err)
	}
	if want := `topk(5, ` + cpuUtil(abc, "1d") + `)`; pick != want {
		t.Fatalf("first stage ranks over the graph range:\n got %s\nwant %s", pick, want)
	}

	q.TopK, q.Targets = nil, targets("c", "a")
	graph, err := r.Render(q)
	if err != nil {
		t.Fatalf("second stage: %v", err)
	}
	if want := cpuUtil(`entity_id=~"a|c"`, "5m"); graph != want {
		t.Fatalf("second stage:\n got %s\nwant %s", graph, want)
	}
}

func TestRenderWindow(t *testing.T) {
	r := renderer(t)
	for name, tc := range map[string]struct {
		metric string
		window monitoring.Agg
		want   string
	}{
		"rate current value": {"system.cpu.utilization", monitoring.Agg{}, cpuUtil(`entity_id="a"`, "1m")},
		"rate peak goes through a subquery": {
			"system.cpu.utilization", monitoring.Agg{Fn: monitoring.AggMax, Range: time.Hour},
			`max_over_time((` + cpuUtil(`entity_id="a"`, "1m") + `)[1h:30s])`,
		},
		"gauge current value": {
			"system.memory.utilization", monitoring.Agg{},
			`sum by (entity_id) ({"system.memory.utilization", state="used", entity_id="a"})`,
		},
		"gauge last": {
			"system.memory.utilization", monitoring.Agg{Fn: monitoring.AggLast, Range: 10 * time.Minute},
			`last_over_time((sum by (entity_id) ({"system.memory.utilization", state="used", entity_id="a"}))[10m:30s])`,
		},
	} {
		got, err := r.Render(monitoring.QuerySpec{Metric: tc.metric, Targets: targets("a"), Window: tc.window})
		if err != nil {
			t.Errorf("%s: Render: %v", name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", name, got, tc.want)
		}
	}
}

func TestRenderWithinKeepsOtherLabels(t *testing.T) {
	got, err := renderer(t).Render(monitoring.QuerySpec{
		Metric: "hostmetrics:system.disk.pending_operations", Targets: targets("a"),
		Within: &monitoring.Reduce{Label: "device", Fn: monitoring.AggMax},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(got, "max by (entity_id) (") {
		t.Fatalf("reduce kept the dimension or dropped the key:\n%s", got)
	}
}

func TestRenderGroupBy(t *testing.T) {
	got, err := renderer(t).Render(monitoring.QuerySpec{
		Metric: "system.filesystem.usage", Targets: targets("a", "b"),
		Across: monitoring.AggSum, GroupBy: []string{"mountpoint"},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(got, "sum by (mountpoint) (sum by (entity_id, mountpoint) (") {
		t.Fatalf("unexpected grouping:\n%s", got)
	}
}

// TestRenderSelectorIsStable checks that the order and repetition of targets
// do not change the query text, which a results cache is keyed by.
func TestRenderSelectorIsStable(t *testing.T) {
	r := renderer(t)
	q := monitoring.QuerySpec{Metric: "system.memory.usage"}
	q.Targets = targets("b", "a", "c")
	first, err := r.Render(q)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	q.Targets = targets("c", "b", "a", "b")
	if again, _ := r.Render(q); again != first {
		t.Fatalf("query text depends on target order:\n%s\n%s", first, again)
	}
	if !strings.Contains(first, abc) {
		t.Fatalf("selector not sorted: %s", first)
	}
}

func TestRenderEscapesIDs(t *testing.T) {
	got, err := renderer(t).Render(monitoring.QuerySpec{Metric: "system.memory.usage", Targets: targets("a.b", `c"d`)})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got, `entity_id=~"a\\.b|c\"d"`) {
		t.Fatalf("ids not escaped: %s", got)
	}
}

func TestRenderRejects(t *testing.T) {
	r := renderer(t)
	unmapped := targets("a")
	unmapped[0].EntityID = ""
	for name, q := range map[string]monitoring.QuerySpec{
		"unknown metric":       {Metric: "system.nope", Targets: targets("a")},
		"metric with a gap":    {Metric: "system.paging.faults", Targets: targets("a")},
		"no targets":           {Metric: "system.memory.usage"},
		"unmapped target":      {Metric: "system.memory.usage", Targets: unmapped},
		"window without range": {Metric: "system.memory.usage", Targets: targets("a"), Window: monitoring.Agg{Fn: monitoring.AggAvg}},
		"window without fn":    {Metric: "system.memory.usage", Targets: targets("a"), Window: monitoring.Agg{Range: time.Hour}},
		"unknown window fn":    {Metric: "system.memory.usage", Targets: targets("a"), Window: monitoring.Agg{Fn: "median", Range: time.Hour}},
		"window under step":    {Metric: "system.memory.usage", Targets: targets("a"), Window: monitoring.Agg{Fn: monitoring.AggMax, Range: 10 * time.Second}},
		"within no dimension":  {Metric: "system.memory.usage", Targets: targets("a"), Within: &monitoring.Reduce{Label: "device", Fn: monitoring.AggSum}},
		"within wrong label":   {Metric: "system.filesystem.usage", Targets: targets("a"), Within: &monitoring.Reduce{Label: "device", Fn: monitoring.AggSum}},
		"within last":          {Metric: "system.filesystem.usage", Targets: targets("a"), Within: &monitoring.Reduce{Label: "mountpoint", Fn: monitoring.AggLast}},
		"GPU row not built":    {Metric: "gpu.utilization", Targets: targets("a")},
		"graph top-k no range": {Metric: "system.memory.usage", Targets: targets("a"), Step: time.Minute, TopK: &monitoring.TopK{N: 1}},
		"group by without fn":  {Metric: "system.memory.usage", Targets: targets("a"), GroupBy: []string{"entity_id"}},
		"bad group by label":   {Metric: "system.memory.usage", Targets: targets("a"), Across: monitoring.AggSum, GroupBy: []string{"__name__"}},
		"top-k of zero":        {Metric: "system.memory.usage", Targets: targets("a"), TopK: &monitoring.TopK{}},
	} {
		if got, err := r.Render(q); err == nil {
			t.Errorf("%s: Render succeeded: %s", name, got)
		}
	}
	if _, err := (PromQL{}).Render(monitoring.QuerySpec{Metric: "system.memory.usage", Targets: targets("a")}); err == nil {
		t.Error("renderer without a catalog succeeded")
	}
}

// TestRenderEveryRow renders each collected catalog row the common ways a
// screen asks for it; Render parses its output, so a success is valid PromQL.
func TestRenderEveryRow(t *testing.T) {
	r := renderer(t)
	for _, row := range catalog.Builtin() {
		if row.Gap != "" {
			continue
		}
		specs := []monitoring.QuerySpec{{}, {Across: monitoring.AggAvg}}
		if row.Kind != monitoring.KindInfo {
			specs = append(specs,
				monitoring.QuerySpec{Window: monitoring.Agg{Fn: monitoring.AggAvg, Range: time.Hour}},
				monitoring.QuerySpec{Window: monitoring.Agg{Fn: monitoring.AggMax, Range: time.Hour}, TopK: &monitoring.TopK{N: 3, Desc: true}},
			)
		}
		if row.Dimension != "" {
			specs = append(specs, monitoring.QuerySpec{Within: &monitoring.Reduce{Label: row.Dimension, Fn: monitoring.AggSum}})
		}
		if row.Attribute != nil {
			name, value := row.Attribute.Name, row.Attribute.Values[0]
			specs = append(specs,
				monitoring.QuerySpec{SplitBy: []string{name}},
				monitoring.QuerySpec{Where: map[string]string{name: value}},
				monitoring.QuerySpec{Where: map[string]string{name: value}, FillZero: true,
					Window: monitoring.Agg{Fn: monitoring.AggMax, Range: time.Hour}},
			)
		}
		for _, q := range specs {
			q.Metric, q.Targets = row.ID(), targets("a", "b")
			if _, err := r.Render(q); err != nil {
				t.Errorf("%s %+v: %v", row.ID(), q, err)
			}
		}
	}
}

// TestRenderQuotesDottedGroupLabels: an attribute keeps its dotted name as its
// series label, so grouping by it quotes it.
func TestRenderQuotesDottedGroupLabels(t *testing.T) {
	r := renderer(t)
	got, err := r.Render(monitoring.QuerySpec{Metric: "system.cpu.utilization", Targets: targets("a", "b"),
		SplitBy: []string{"cpu.mode"}, Across: monitoring.AggAvg, GroupBy: []string{"cpu.mode"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(got, `avg by ("cpu.mode") (`) {
		t.Errorf("got %s, want the group label quoted", got)
	}
	if _, err := parser.NewParser(parser.Options{}).ParseExpr(strings.NewReplacer("$range", "1m").Replace(got)); err != nil {
		t.Errorf("Prometheus parser refused %s: %v", got, err)
	}
}
