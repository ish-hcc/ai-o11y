package nvidiagpureceiver

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.uber.org/zap"
)

// xidRe matches the line the NVIDIA driver logs for a GPU error (XID).
// Depending on the driver the address has "PCI:" or not, a 4 or 8 digit
// domain, a function number or not, and hex digits in either case:
//
//	NVRM: Xid (PCI:0000:3b:00): 79, pid='<unknown>', name=<unknown>, GPU has fallen off the bus.
//	NVRM: Xid (0000:03:00): 13, Graphics SM Warp Exception ...
//
// NVSwitch's "SXid" numbers are another scheme and do not match.
var xidRe = regexp.MustCompile(`NVRM: Xid \((?:PCI:)?(([0-9A-Fa-f]{1,8}):([0-9A-Fa-f]{1,2}):([0-9A-Fa-f]{1,2})(?:\.([0-7]))?)\): (\d+)`)

// pciKey is a PCI device address as numbers, compared whatever its notation.
// It leaves out the function number, which XID lines do not carry.
type pciKey struct {
	domain uint32
	bus    uint8
	device uint8
}

func parsePCIParts(domain, bus, device, function string) (pciKey, bool) {
	d, err1 := strconv.ParseUint(domain, 16, 32)
	b, err2 := strconv.ParseUint(bus, 16, 8)
	v, err3 := strconv.ParseUint(device, 16, 8)
	if err1 != nil || err2 != nil || err3 != nil || v > 0x1f {
		return pciKey{}, false
	}
	if function != "" {
		if f, err := strconv.ParseUint(function, 16, 8); err != nil || f > 7 {
			return pciKey{}, false
		}
	}
	return pciKey{domain: uint32(d), bus: uint8(b), device: uint8(v)}, true
}

// parsePCI reads a PCI address in any of the notations nvidia-smi and the
// kernel log use: "00000001:00:00.0" (nvidia-smi pci.bus_id), "0001:00:00",
// "PCI:0000:3b:00".
func parsePCI(s string) (pciKey, bool) {
	s = strings.TrimSpace(s)
	if len(s) >= 4 && strings.EqualFold(s[:4], "PCI:") {
		s = s[4:]
	}
	var function string
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		function, s = s[i+1:], s[:i]
		if function == "" {
			return pciKey{}, false
		}
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return pciKey{}, false
	}
	return parsePCIParts(parts[0], parts[1], parts[2], function)
}

// xidEvent is one XID line: the XID number and the GPU's address, as written
// and as parsed.
type xidEvent struct {
	xid int64
	pci pciKey
	raw string
}

// findXID finds an XID line in a kernel log message.
func findXID(msg string) (xidEvent, bool) {
	if !strings.Contains(msg, "Xid") {
		return xidEvent{}, false
	}
	m := xidRe.FindStringSubmatch(msg)
	if m == nil {
		return xidEvent{}, false
	}
	pci, ok := parsePCIParts(m[2], m[3], m[4], m[5])
	if !ok {
		return xidEvent{}, false
	}
	n, err := strconv.ParseInt(m[6], 10, 64)
	if err != nil {
		return xidEvent{}, false
	}
	return xidEvent{xid: n, pci: pci, raw: m[1]}, true
}

// parseKmsg reads one /dev/kmsg record, "<pri>,<seq>,<ts>,<flags>[,...];<message>".
// The key=value lines that follow a record start with a space and are not
// records.
func parseKmsg(line string) (seq uint64, msg string, ok bool) {
	line = strings.TrimSuffix(line, "\n")
	header, msg, found := strings.Cut(line, ";")
	if !found || strings.HasPrefix(line, " ") {
		return 0, "", false
	}
	fields := strings.Split(header, ",")
	if len(fields) < 4 {
		return 0, "", false
	}
	if _, err := strconv.ParseUint(fields[0], 10, 32); err != nil {
		return 0, "", false
	}
	seq, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, "", false
	}
	return seq, msg, true
}

// kmsgBufSize holds the largest /dev/kmsg record: a read with a smaller
// buffer fails with EINVAL.
const kmsgBufSize = 64 << 10

// xidCount is how many times one XID was logged for one GPU address.
type xidCount struct {
	xid int64
	pci pciKey
	// raw is the address as the first line counted wrote it.
	raw string
	n   int64
}

type xidKey struct {
	xid int64
	pci pciKey
}

// xidCounter counts the XID lines of the kernel log, read from /dev/kmsg.
//
// Opening /dev/kmsg reads the kernel's ring buffer from its oldest record, so
// the counts are since boot, as far back as the ring buffer still holds: a
// collector that restarts reads the same lines again and reports the same
// counts, rather than starting from zero. Records carry a sequence number;
// a record whose number was already read is not counted again, which keeps
// the counts right when the file is opened again after a read error. Records
// the kernel overwrote before they were read (EPIPE) are skipped.
type xidCounter struct {
	path   string
	logger *zap.Logger
	// retry is the wait at the end of the file (a regular file standing in
	// for /dev/kmsg) and before opening it again after a read error.
	retry time.Duration

	mu      sync.Mutex
	counts  map[xidKey]*xidCount
	seen    bool
	lastSeq uint64
	file    *os.File

	stop chan struct{}
	done chan struct{}
}

func newXIDCounter(path string, logger *zap.Logger) *xidCounter {
	return &xidCounter{path: path, logger: logger, retry: time.Second, counts: map[xidKey]*xidCount{}}
}

// Start opens the kernel log and reads it in the background. When it cannot
// be opened XIDs are not counted, and the receiver goes on without them.
func (c *xidCounter) Start(context.Context, component.Host) error {
	f, err := os.Open(c.path)
	if err != nil {
		c.logger.Warn("XID errors are not counted: cannot read the kernel log", zap.String("path", c.path), zap.Error(err))
		return nil
	}
	c.mu.Lock()
	c.file = f
	c.mu.Unlock()
	c.stop, c.done = make(chan struct{}), make(chan struct{})
	go c.run(f)
	return nil
}

// Shutdown stops reading: closing the file ends a read waiting for a record.
func (c *xidCounter) Shutdown(context.Context) error {
	if c.stop == nil {
		return nil
	}
	close(c.stop)
	c.mu.Lock()
	f := c.file
	c.file = nil
	c.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
	<-c.done
	c.stop = nil
	return nil
}

func (c *xidCounter) run(f *os.File) {
	defer close(c.done)
	for {
		err := c.read(f)
		if c.stopped() {
			return
		}
		c.logger.Warn("reading the kernel log failed; opening it again", zap.String("path", c.path), zap.Error(err))
		c.mu.Lock()
		if c.file == f {
			c.file = nil
		}
		c.mu.Unlock()
		_ = f.Close()
		for f = nil; f == nil; {
			if c.wait() {
				return
			}
			if f, err = os.Open(c.path); err != nil {
				f = nil
				continue
			}
			c.mu.Lock()
			if c.stopped() {
				c.mu.Unlock()
				_ = f.Close()
				return
			}
			c.file = f
			c.mu.Unlock()
		}
	}
}

func (c *xidCounter) stopped() bool {
	select {
	case <-c.stop:
		return true
	default:
		return false
	}
}

// wait sleeps for the retry interval and reports whether Shutdown came first.
func (c *xidCounter) wait() bool {
	t := time.NewTimer(c.retry)
	defer t.Stop()
	select {
	case <-c.stop:
		return true
	case <-t.C:
		return false
	}
}

// read counts the records of f until a read fails other than at the end of
// the file or for overwritten records.
func (c *xidCounter) read(f io.Reader) error {
	r := bufio.NewReaderSize(f, kmsgBufSize)
	var partial string
	for {
		line, err := r.ReadString('\n')
		if err == nil {
			c.record(partial + line)
			partial = ""
			continue
		}
		partial += line
		switch {
		case errors.Is(err, syscall.EPIPE):
			partial = ""
		case errors.Is(err, io.EOF):
			if c.wait() {
				return nil
			}
		default:
			return err
		}
	}
}

// record counts one record when it is an XID line read for the first time.
func (c *xidCounter) record(line string) {
	seq, msg, ok := parseKmsg(line)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen && seq <= c.lastSeq {
		return
	}
	c.seen, c.lastSeq = true, seq
	ev, ok := findXID(msg)
	if !ok {
		return
	}
	k := xidKey{xid: ev.xid, pci: ev.pci}
	if n, ok := c.counts[k]; ok {
		n.n++
		return
	}
	c.counts[k] = &xidCount{xid: ev.xid, pci: ev.pci, raw: ev.raw, n: 1}
}

// snapshot returns the counts so far, ordered by address, then XID.
func (c *xidCounter) snapshot() []xidCount {
	c.mu.Lock()
	out := make([]xidCount, 0, len(c.counts))
	for _, n := range c.counts {
		out = append(out, *n)
	}
	c.mu.Unlock()
	slices.SortFunc(out, func(a, b xidCount) int {
		return cmp.Or(cmp.Compare(a.pci.domain, b.pci.domain), cmp.Compare(a.pci.bus, b.pci.bus),
			cmp.Compare(a.pci.device, b.pci.device), cmp.Compare(a.xid, b.xid))
	})
	return out
}

func (k pciKey) String() string { return fmt.Sprintf("%08X:%02X:%02X", k.domain, k.bus, k.device) }
