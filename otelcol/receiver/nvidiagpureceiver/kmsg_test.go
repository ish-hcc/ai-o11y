package nvidiagpureceiver

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"go.opentelemetry.io/collector/scraper/scrapererror"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestFindXID(t *testing.T) {
	for _, c := range []struct {
		line string
		ok   bool
		xid  int64
		pci  string
		raw  string
	}{
		{"NVRM: Xid (PCI:0000:3b:00): 79, pid='<unknown>', name=<unknown>, GPU has fallen off the bus.", true, 79, "00000000:3B:00", "0000:3b:00"},
		{"NVRM: Xid (PCI:0000:3B:00): 13, pid=1, name=a, Graphics Exception", true, 13, "00000000:3B:00", "0000:3B:00"},
		{"NVRM: Xid (0000:86:00): 48, An uncorrectable double bit error", true, 48, "00000000:86:00", "0000:86:00"},
		{"NVRM: Xid (PCI:0001:00:00): 31, pid=1234, name=test, Ch 00000001", true, 31, "00000001:00:00", "0001:00:00"},
		{"NVRM: Xid (PCI:00000001:02:00.0): 94, Contained ECC error.", true, 94, "00000001:02:00", "00000001:02:00.0"},
		{"nvidia-nvswitch0: SXid (PCI:0000:05:00.0): 12028, Non-fatal", false, 0, "", ""},
		{"NVRM: Xid (PCI:0000:3b:20): 79, device number past 0x1f", false, 0, "", ""},
		{"NVRM: GPU 0000:3b:00.0: GPU has fallen off the bus.", false, 0, "", ""},
	} {
		ev, ok := findXID(c.line)
		if ok != c.ok {
			t.Errorf("%q: ok = %v", c.line, ok)
			continue
		}
		if ok && (ev.xid != c.xid || ev.pci.String() != c.pci || ev.raw != c.raw) {
			t.Errorf("%q: xid %d pci %s raw %q, want %d %s %q", c.line, ev.xid, ev.pci, ev.raw, c.xid, c.pci, c.raw)
		}
	}
}

// TestParsePCIMatchesNotations: nvidia-smi writes an 8 digit domain and a
// function number (00000001:00:00.0 on an Azure T4), the kernel a 4 digit
// domain and no function number.
func TestParsePCIMatchesNotations(t *testing.T) {
	for _, pair := range [][2]string{
		{"00000001:00:00.0", "0001:00:00"},
		{"00000000:3B:00.0", "0000:3b:00"},
		{"00000000:3B:00.0", "PCI:0000:3B:00"},
		{"0000:af:1f.3", "00000000:AF:1F.0"},
	} {
		a, okA := parsePCI(pair[0])
		b, okB := parsePCI(pair[1])
		if !okA || !okB || a != b {
			t.Errorf("%s (%v %v) and %s (%v %v) differ", pair[0], a, okA, pair[1], b, okB)
		}
	}
	if a, _ := parsePCI("00000001:00:00.0"); a == (pciKey{}) {
		t.Error("domain 1 read as 0")
	}
	for _, bad := range []string{"", "[N/A]", "0000:3b", "0000:3b:00.", "0000:3b:00.8", "0000:3b:20.0", "zz:00:00.0"} {
		if k, ok := parsePCI(bad); ok {
			t.Errorf("%q read as %v", bad, k)
		}
	}
}

func TestParseKmsg(t *testing.T) {
	seq, msg, ok := parseKmsg("4,1202,5130011,-;NVRM: Xid (PCI:0000:3b:00): 79, a;b\n")
	if !ok || seq != 1202 || msg != "NVRM: Xid (PCI:0000:3b:00): 79, a;b" {
		t.Errorf("record = %d %q %v", seq, msg, ok)
	}
	if _, _, ok := parseKmsg("6,7,8,-,caller=T1;message\n"); !ok {
		t.Error("record with a further header field was rejected")
	}
	for _, bad := range []string{" DEVICE=+pci:0000:3b:00.0\n", "no header\n", "4,x,1,-;m\n", "4,1;m\n", "\n"} {
		if _, _, ok := parseKmsg(bad); ok {
			t.Errorf("%q read as a record", bad)
		}
	}
}

func counterFrom(t *testing.T, name string) *xidCounter {
	t.Helper()
	c := newXIDCounter("", zap.NewNop())
	sc := bufio.NewScanner(bytes.NewReader(read(t, name)))
	for sc.Scan() {
		c.record(sc.Text() + "\n")
	}
	return c
}

// TestCountRecordedKmsg counts testdata/kmsg-xid.txt: the record repeated with
// its sequence number is counted once, the same GPU in another case is the
// same GPU, and SXid, other lines and a bad address are not counted.
func TestCountRecordedKmsg(t *testing.T) {
	got := counterFrom(t, "kmsg-xid.txt").snapshot()
	want := []struct {
		pci string
		xid int64
		n   int64
		raw string
	}{
		{"00000000:3B:00", 79, 2, "0000:3b:00"},
		{"00000000:86:00", 48, 1, "0000:86:00"},
		{"00000001:00:00", 31, 2, "0001:00:00"},
	}
	if len(got) != len(want) {
		t.Fatalf("counts = %+v", got)
	}
	for i, w := range want {
		if g := got[i]; g.pci.String() != w.pci || g.xid != w.xid || g.n != w.n || g.raw != w.raw {
			t.Errorf("count %d = %+v (%s), want %+v", i, g, g.pci, w)
		}
	}
}

// steps is a reader that answers each read with the next step.
type steps []struct {
	data string
	err  error
}

func (s *steps) Read(p []byte) (int, error) {
	if len(*s) == 0 {
		return 0, errDone
	}
	st := (*s)[0]
	*s = (*s)[1:]
	return copy(p, st.data), st.err
}

var errDone = errors.New("done")

// TestReadGoesOnAfterOverwrittenRecords: EPIPE (records overwritten before
// they were read) does not stop reading, the end of a regular file waits for
// more, a line split over two reads is one record, and reading the same
// records again, as after opening the file again, counts nothing twice.
func TestReadGoesOnAfterOverwrittenRecords(t *testing.T) {
	c := newXIDCounter("", zap.NewNop())
	c.retry, c.stop = time.Millisecond, make(chan struct{})
	r := &steps{
		{"4,10,1,-;NVRM: Xid (PCI:0000:3b:00): 79, a\n", nil},
		{"", syscall.EPIPE},
		{"4,20,2,-;NVRM: Xid (PCI:00", nil},
		{"", io.EOF},
		{"00:3b:00): 79, b\n", nil},
		{"4,10,1,-;NVRM: Xid (PCI:0000:3b:00): 79, a\n", nil},
	}
	if err := c.read(r); !errors.Is(err, errDone) {
		t.Fatalf("read = %v", err)
	}
	if got := c.snapshot(); len(got) != 1 || got[0].n != 2 {
		t.Errorf("counts = %+v, want 2 of XID 79", got)
	}
}

func TestScrapeEmitsXIDByUUID(t *testing.T) {
	at := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	s := &gpuScraper{now: func() time.Time { return at }, xid: counterFrom(t, "kmsg-xid.txt"), source: fixedSource{gpus: []GPU{
		{Attributes: map[string]string{"index": "0", "uuid": "GPU-a", "name": "A100"}, PCIBusID: "00000000:3B:00.0",
			Values: []Value{{Field: "utilization.gpu", Unit: "%", Value: 1}}},
		{Attributes: map[string]string{"index": "1", "uuid": "GPU-t4", "name": "T4"}, PCIBusID: "00000001:00:00.0"},
	}}}
	md, err := s.scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := findMetric(t, md, xidMetric)
	if m.Unit() != "{error}" || m.Type() != pmetric.MetricTypeSum || !m.Sum().IsMonotonic() ||
		m.Sum().AggregationTemporality() != pmetric.AggregationTemporalityCumulative {
		t.Fatalf("%s: unit %q type %v", m.Name(), m.Unit(), m.Type())
	}
	want := []map[string]any{
		{"value": int64(2), "xid": int64(79), "uuid": "GPU-a"},
		{"value": int64(1), "xid": int64(48), "pci.bus_id": "0000:86:00"},
		{"value": int64(2), "xid": int64(31), "uuid": "GPU-t4"},
	}
	checkPoints(t, m, want)
	if dp := m.Sum().DataPoints().At(0); dp.Timestamp().AsTime() != at {
		t.Errorf("timestamp = %v", dp.Timestamp().AsTime())
	}
	if utilAttrs := findMetric(t, md, "nvidia_smi.utilization.gpu").Gauge().DataPoints().At(0).Attributes(); utilAttrs.Len() != 3 {
		t.Errorf("GPU attributes = %v, want index, uuid and name only", utilAttrs.AsRaw())
	}
}

type failingSource struct{}

func (failingSource) Namespace() string { return SourceNvidiaSMI }
func (failingSource) Read(context.Context) ([]GPU, error) {
	return nil, errors.New("GPU has fallen off the bus")
}

// TestScrapeEmitsXIDWhenSourceFails: XIDs go out when nvidia-smi fails, with
// the uuids learnt from the last read that worked.
func TestScrapeEmitsXIDWhenSourceFails(t *testing.T) {
	s := &gpuScraper{now: time.Now, xid: counterFrom(t, "kmsg-xid.txt"),
		uuids: map[pciKey]string{{domain: 0, bus: 0x3b}: "GPU-a"}, source: failingSource{}}
	md, err := s.scrape(context.Background())
	if !scrapererror.IsPartialScrapeError(err) {
		t.Fatalf("err = %v, want a partial scrape error", err)
	}
	checkPoints(t, findMetric(t, md, xidMetric), []map[string]any{
		{"value": int64(2), "xid": int64(79), "uuid": "GPU-a"},
		{"value": int64(1), "xid": int64(48), "pci.bus_id": "0000:86:00"},
		{"value": int64(2), "xid": int64(31), "pci.bus_id": "0001:00:00"},
	})

	s.xid = newXIDCounter("", zap.NewNop())
	if md, err := s.scrape(context.Background()); err == nil || scrapererror.IsPartialScrapeError(err) || md.MetricCount() != 0 {
		t.Errorf("with nothing counted: %d metrics, err %v; want the source's error alone", md.MetricCount(), err)
	}
}

func findMetric(t *testing.T, md pmetric.Metrics, name string) pmetric.Metric {
	t.Helper()
	for i := range md.ResourceMetrics().Len() {
		sms := md.ResourceMetrics().At(i).ScopeMetrics()
		for j := range sms.Len() {
			ms := sms.At(j).Metrics()
			for k := range ms.Len() {
				if ms.At(k).Name() == name {
					return ms.At(k)
				}
			}
		}
	}
	t.Fatalf("no %s", name)
	return pmetric.Metric{}
}

func checkPoints(t *testing.T, m pmetric.Metric, want []map[string]any) {
	t.Helper()
	dps := m.Sum().DataPoints()
	if dps.Len() != len(want) {
		t.Fatalf("%d points, want %d", dps.Len(), len(want))
	}
	for i, w := range want {
		got := dps.At(i).Attributes().AsRaw()
		got["value"] = dps.At(i).IntValue()
		if len(got) != len(w) {
			t.Errorf("point %d = %v, want %v", i, got, w)
			continue
		}
		for k, v := range w {
			if got[k] != v {
				t.Errorf("point %d = %v, want %v", i, got, w)
				break
			}
		}
	}
}

// TestReceiverCountsXIDFromFile runs the receiver on a regular file standing
// in for /dev/kmsg, with no nvidia-smi: the XIDs in the file are reported,
// and so is one appended while it runs.
func TestReceiverCountsXIDFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kmsg")
	if err := os.WriteFile(path, read(t, "kmsg-xid.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := NewFactory()
	cfg := f.CreateDefaultConfig().(*Config)
	cfg.CollectionInterval = 50 * time.Millisecond
	cfg.NvidiaSMI.Path = filepath.Join(t.TempDir(), "no-nvidia-smi")
	cfg.XID = XIDConfig{Enabled: true, Path: path}
	sink := new(consumertest.MetricsSink)
	r, err := f.CreateMetrics(context.Background(), receivertest.NewNopSettings(f.Type()), cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()

	waitFor(t, sink, map[string]int64{"0001:00:00/31": 2, "0000:3b:00/79": 2, "0000:86:00/48": 1})
	af, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := af.WriteString("4,1300,7000000,-;NVRM: Xid (PCI:0001:00:00): 31, pid=1, name=test, Ch 00000001\n"); err != nil {
		t.Fatal(err)
	}
	_ = af.Close()
	waitFor(t, sink, map[string]int64{"0001:00:00/31": 3, "0000:3b:00/79": 2, "0000:86:00/48": 1})
}

// waitFor waits until the last XID metric received has the counts want,
// keyed pci.bus_id/xid.
func waitFor(t *testing.T, sink *consumertest.MetricsSink, want map[string]int64) {
	t.Helper()
	var got map[string]int64
	for range 200 {
		all := sink.AllMetrics()
		for i := len(all) - 1; i >= 0 && got == nil; i-- {
			got = xidCounts(all[i])
		}
		if len(got) == len(want) {
			same := true
			for k, v := range want {
				same = same && got[k] == v
			}
			if same {
				return
			}
		}
		got = nil
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("XID counts never became %v", want)
}

func xidCounts(md pmetric.Metrics) map[string]int64 {
	var out map[string]int64
	for i := range md.ResourceMetrics().Len() {
		sms := md.ResourceMetrics().At(i).ScopeMetrics()
		for j := range sms.Len() {
			ms := sms.At(j).Metrics()
			for k := range ms.Len() {
				if ms.At(k).Name() != xidMetric {
					continue
				}
				out = map[string]int64{}
				dps := ms.At(k).Sum().DataPoints()
				for p := range dps.Len() {
					pci, _ := dps.At(p).Attributes().Get("pci.bus_id")
					xid, _ := dps.At(p).Attributes().Get("xid")
					out[pci.Str()+"/"+xid.AsString()] = dps.At(p).IntValue()
				}
			}
		}
	}
	return out
}

// TestStartWithoutKernelLog: a kernel log that cannot be opened is a warning,
// not a failure of the receiver.
func TestStartWithoutKernelLog(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	c := newXIDCounter(filepath.Join(t.TempDir(), "missing"), zap.New(core))
	if err := c.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	if logs.FilterMessageSnippet("XID errors are not counted").Len() != 1 {
		t.Errorf("logged %v", logs.All())
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
