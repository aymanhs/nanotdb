package collectors

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// DiskCollector reads /proc/diskstats and emits per-device I/O counters.
// Only emits metrics for physical block devices (e.g. sda, nvme0n1, mmcblk0).
// Metrics per device: disk.{dev}.reads, disk.{dev}.reads_merged,
// disk.{dev}.read_sectors, disk.{dev}.read_ms,
// disk.{dev}.writes, disk.{dev}.writes_merged,
// disk.{dev}.write_sectors, disk.{dev}.write_ms,
// disk.{dev}.io_in_progress, disk.{dev}.io_ms, disk.{dev}.io_weighted_ms
//
// Also emits mount-level filesystem metrics:
// diskfs.{mount}.bytes_total, bytes_used, bytes_free, bytes_avail, used_pct
type DiskCollector struct{}

func NewDiskCollector() *DiskCollector { return &DiskCollector{} }

func (c *DiskCollector) Name() string { return "disk" }

func (c *DiskCollector) Collect(ctx context.Context, ch chan<- Metric) {
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		log.Printf("disk collector: open /proc/diskstats: %v", err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(line)
		// /proc/diskstats fields (1-indexed from kernel docs):
		// 1 major, 2 minor, 3 name, 4 reads_completed, 5 reads_merged,
		// 6 sectors_read, 7 ms_reading, 8 writes_completed, 9 writes_merged,
		// 10 sectors_written, 11 ms_writing, ...
		if len(fields) < 14 {
			continue
		}
		dev := fields[2]
		// Skip partition entries (sda1, nvme0n1p1, etc.) and virtual devices
		if !isPhysicalDevice(dev) {
			continue
		}

		type field struct {
			idx  int
			name string
		}
		metrics := []field{
			{3, "reads"},
			{4, "reads_merged"},
			{5, "read_sectors"},
			{6, "read_ms"},
			{7, "writes"},
			{8, "writes_merged"},
			{9, "write_sectors"},
			{10, "write_ms"},
			{11, "io_in_progress"},
			{12, "io_ms"},
			{13, "io_weighted_ms"},
		}

		for _, mf := range metrics {
			v, err := strconv.ParseInt(fields[mf.idx], 10, 64)
			if err != nil {
				log.Printf("disk collector: parse %s %s: %v", dev, mf.name, err)
				continue
			}
			select {
			case ch <- Metric{Name: fmt.Sprintf("disk.%s.%s", dev, mf.name), Value: float32(v)}:
			case <-ctx.Done():
				return
			}
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("disk collector: scan /proc/diskstats: %v", err)
	}

	c.collectFilesystemStats(ctx, ch)
}

func (c *DiskCollector) collectFilesystemStats(ctx context.Context, ch chan<- Metric) {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		log.Printf("disk collector: open /proc/self/mounts: %v", err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	seen := make(map[string]struct{})
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		mount := parts[1]
		fsType := parts[2]
		if _, ok := seen[mount]; ok {
			continue
		}
		seen[mount] = struct{}{}

		if skipFSType(fsType) {
			continue
		}

		var st syscall.Statfs_t
		if err := syscall.Statfs(mount, &st); err != nil {
			continue
		}

		bs := uint64(st.Bsize)
		total := float64(st.Blocks * bs)
		free := float64(st.Bfree * bs)
		avail := float64(st.Bavail * bs)
		used := total - free

		usedPct := float32(0)
		if total > 0 {
			usedPct = float32((used / total) * 100.0)
		}

		name := sanitizeMountName(mount)
		metrics := []Metric{
			{Name: fmt.Sprintf("diskfs.%s.bytes_total", name), Value: float32(total)},
			{Name: fmt.Sprintf("diskfs.%s.bytes_used", name), Value: float32(used)},
			{Name: fmt.Sprintf("diskfs.%s.bytes_free", name), Value: float32(free)},
			{Name: fmt.Sprintf("diskfs.%s.bytes_avail", name), Value: float32(avail)},
			{Name: fmt.Sprintf("diskfs.%s.used_pct", name), Value: usedPct},
		}
		for _, m := range metrics {
			select {
			case ch <- m:
			case <-ctx.Done():
				return
			}
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("disk collector: scan /proc/self/mounts: %v", err)
	}
}

func skipFSType(fsType string) bool {
	skip := map[string]struct{}{
		"proc": {}, "sysfs": {}, "tmpfs": {}, "devtmpfs": {}, "devpts": {},
		"securityfs": {}, "cgroup": {}, "cgroup2": {}, "pstore": {}, "tracefs": {},
		"debugfs": {}, "configfs": {}, "overlay": {}, "squashfs": {}, "ramfs": {},
		"autofs": {}, "mqueue": {}, "fusectl": {}, "binfmt_misc": {},
	}
	_, ok := skip[fsType]
	return ok
}

func sanitizeMountName(mount string) string {
	if mount == "/" {
		return "root"
	}
	s := strings.TrimPrefix(mount, "/")
	s = strings.ReplaceAll(s, "/", "_")
	if s == "" {
		return "root"
	}
	return s
}

// isPhysicalDevice returns true for physical block device names
// (sda, sdb, nvme0n1, mmcblk0, vda, hda) and false for partitions or virtual devices.
func isPhysicalDevice(name string) bool {
	// Skip loop devices, dm- (device mapper), ram
	for _, prefix := range []string{"loop", "dm-", "ram", "zram"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	// NVMe partitions: nvme0n1p1
	if strings.Contains(name, "nvme") && strings.Contains(name, "p") {
		return false
	}
	// MMC partitions: mmcblk0p1
	if strings.Contains(name, "mmcblk") && strings.Contains(name, "p") {
		return false
	}
	// SCSI/virtio partitions: sda1, vda1, hda1 (ends with digit after alpha)
	if len(name) > 0 {
		last := name[len(name)-1]
		if last >= '0' && last <= '9' {
			// nvme0n1 and mmcblk0 are fine (end in digit but no 'p')
			if !strings.Contains(name, "nvme") && !strings.Contains(name, "mmcblk") {
				return false
			}
		}
	}
	return true
}
