//go:build unix

package system

import (
	"context"
	"net"
	"runtime"
	"strings"
	
	"golang.org/x/sys/unix"

	"github.com/moby/moby/api/types/image"

	"github.com/acobaugh/osrelease"

	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
)

type Information struct {
	Version string            `json:"version"`
	Docker  DockerInformation `json:"docker"`
	System  System            `json:"system"`
}

type DockerInformation struct {
	Version    string           `json:"version"`
	Cgroups    DockerCgroups    `json:"cgroups"`
	Containers DockerContainers `json:"containers"`
	Storage    DockerStorage    `json:"storage"`
	Runc       DockerRunc       `json:"runc"`
}

type DockerCgroups struct {
	Driver  string `json:"driver"`
	Version string `json:"version"`
}

type DockerContainers struct {
	Total   int `json:"total"`
	Running int `json:"running"`
	Paused  int `json:"paused"`
	Stopped int `json:"stopped"`
}

type DockerStorage struct {
	Driver     string `json:"driver"`
	Filesystem string `json:"filesystem"`
}

type DockerRunc struct {
	Version string `json:"version"`
}

type System struct {
	Architecture  string `json:"architecture"`
	CPUThreads    int    `json:"cpu_threads"`
	MemoryBytes   int64  `json:"memory_bytes"`
	KernelVersion string `json:"kernel_version"`
	OS            string `json:"os"`
	OSType        string `json:"os_type"`
}

type IpAddresses struct {
	IpAddresses []string `json:"ip_addresses"`
}

type DiskInfo struct {
	Device     string   `json:"device"`
	Mountpoint string   `json:"mountpoint"`
	TotalSpace uint64   `json:"total_space"`
	UsedSpace  uint64   `json:"used_space"`
	Tags       []string `json:"tags"`
}

type Utilization struct {
	MemoryTotal uint64     `json:"memory_total"`
	MemoryUsed  uint64     `json:"memory_used"`
	SwapTotal   uint64     `json:"swap_total"`
	SwapUsed    uint64     `json:"swap_used"`
	LoadAvg1    float64    `json:"load_average1"`
	LoadAvg5    float64    `json:"load_average5"`
	LoadAvg15   float64    `json:"load_average15"`
	CpuPercent  float64    `json:"cpu_percent"`
	DiskTotal   uint64     `json:"disk_total"`
	DiskUsed    uint64     `json:"disk_used"`
	DiskDetails []DiskInfo `json:"disk_details"`
}

type DockerDiskUsage struct {
	ContainersSize int64 `json:"containers_size"`
	ImagesTotal    int   `json:"images_total"`
	ImagesActive   int64 `json:"images_active"`
	ImagesSize     int64 `json:"images_size"`
	BuildCacheSize int64 `json:"build_cache_size"`
}

func GetSystemInformation() (*Information, error) {
	k, err := KernelVersion()
	if err != nil {
		return nil, err
	}

	version, info, err := GetDockerInfo(context.Background())
	if err != nil {
		return nil, err
	}

	var os string
	if runtime.GOOS == "darwin" {
		os = "macOS"
	} else {
		release, err := osrelease.Read()
		if err != nil {
			return nil, err
		}
		if release["PRETTY_NAME"] != "" {
			os = release["PRETTY_NAME"]
		} else if release["NAME"] != "" {
			os = release["NAME"]
		} else {
			os = info.OperatingSystem
		}
	}

	var filesystem string
	for _, v := range info.DriverStatus {
		if v[0] != "Backing Filesystem" {
			continue
		}
		filesystem = v[1]
		break
	}

	return &Information{
		Version: Version,
		Docker: DockerInformation{
			Version: version.Version,
			Cgroups: DockerCgroups{
				Driver:  info.CgroupDriver,
				Version: info.CgroupVersion,
			},
			Containers: DockerContainers{
				Total:   info.Containers,
				Running: info.ContainersRunning,
				Paused:  info.ContainersPaused,
				Stopped: info.ContainersStopped,
			},
			Storage: DockerStorage{
				Driver:     info.Driver,
				Filesystem: filesystem,
			},
			Runc: DockerRunc{
				Version: info.RuncCommit.ID,
			},
		},
		System: System{
			Architecture:  runtime.GOARCH,
			CPUThreads:    runtime.NumCPU(),
			MemoryBytes:   info.MemTotal,
			KernelVersion: k,
			OS:            os,
			OSType:        runtime.GOOS,
		},
	}, nil
}

func GetSystemIps() ([]string, error) {
	var ip_addrs []string
	iface_addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range iface_addrs {
		ipNet, valid := addr.(*net.IPNet)
		if valid && !ipNet.IP.IsLoopback() && (len(ipNet.IP) == net.IPv6len && !ipNet.IP.IsLinkLocalUnicast()) {
			ip_addrs = append(ip_addrs, ipNet.IP.String())
		}
	}
	return ip_addrs, nil
}

// getDiskForPath finds the mountpoint where the given path is stored
func getDiskForPath(path string, partitions []disk.PartitionStat) (string, string, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return "", "", err
	}

	for _, part := range partitions {
		var pStat unix.Statfs_t
		if err := unix.Statfs(part.Mountpoint, &pStat); err != nil {
			continue
		}
		if stat.Fsid == pStat.Fsid {
			return part.Device, part.Mountpoint, nil
		}
	}

	return "", "", nil // No error, but couldn't find the disk
}

// Gets the system release name.
func getSystemName() (string, error) {
	if runtime.GOOS == "darwin" {
		return "darwin", nil
	}
	// use osrelease to get release version and ID
	release, err := osrelease.Read()
	if err != nil {
		return "", err
	}
	return release["ID"], nil
}

func GetSystemUtilization(root, logs, data, archive, backup, temp string) (*Utilization, error) {
	c, err := cpu.Percent(0, false)
	if err != nil {
		return nil, err
	}
	m, err := mem.VirtualMemory()
	if err != nil {
		return nil, err
	}
	s, err := mem.SwapMemory()
	if err != nil {
		return nil, err
	}
	l, err := load.Avg()
	if err != nil {
		return nil, err
	}

	// Define paths to check with their tags
	paths := map[string]string{
		"Root":    root,
		"Logs":    logs,
		"Data":    data,
		"Archive": archive,
		"Backup":  backup,
		"Temp":    temp,
	}

	partitions, err := disk.Partitions(false)
	if err != nil {
		return nil, err
	}

	sysName, err := getSystemName()
	if err != nil {
		return nil, err
	}

	// We are in docker
	runningInContainer := (sysName == "distroless")

	diskMap := make(map[string]*DiskInfo)
	seenDevices := make(map[string]bool)
	var totalDiskSpace uint64
	var usedDiskSpace uint64

	// Collect disk usage from valid partitions and avoid overcounting
	for _, partition := range partitions {
		// Skip pseudo or irrelevant filesystems
		if strings.HasPrefix(partition.Fstype, "tmpfs") ||
			strings.HasPrefix(partition.Fstype, "devtmpfs") ||
			(strings.HasPrefix(partition.Fstype, "overlay") && !runningInContainer) ||
			strings.HasPrefix(partition.Fstype, "squashfs") ||
			partition.Fstype == "" {
			continue
		}

		// Avoid counting the same physical device multiple times
		if _, seen := seenDevices[partition.Device]; seen {
			continue
		}

		usage, err := disk.Usage(partition.Mountpoint)
		if err != nil {
			continue
		}

		totalDiskSpace += usage.Total
		usedDiskSpace += usage.Used
		seenDevices[partition.Device] = true

		diskMap[partition.Mountpoint] = &DiskInfo{
			Device:     partition.Device,
			Mountpoint: partition.Mountpoint,
			TotalSpace: usage.Total,
			UsedSpace:  usage.Used,
			Tags:       []string{},
		}
	}

	// Add tags to corresponding disks based on paths
	for tag, path := range paths {
		_, mountpoint, err := getDiskForPath(path, partitions)
		if err == nil && mountpoint != "" {
			if disk, exists := diskMap[mountpoint]; exists {
				disk.Tags = append(disk.Tags, tag)
			}
		}
	}

	// Convert disk map to slice for return
	var diskDetails []DiskInfo
	for _, disk := range diskMap {
		diskDetails = append(diskDetails, *disk)
	}

	return &Utilization{
		MemoryTotal: m.Total,
		MemoryUsed:  m.Used,
		SwapTotal:   s.Total,
		SwapUsed:    s.Used,
		CpuPercent:  c[0],
		LoadAvg1:    l.Load1,
		LoadAvg5:    l.Load5,
		LoadAvg15:   l.Load15,
		DiskTotal:   totalDiskSpace,
		DiskUsed:    usedDiskSpace,
		DiskDetails: diskDetails,
	}, nil
}

func GetDockerDiskUsage(ctx context.Context) (*DockerDiskUsage, error) {
	// TODO: find a way to re-use the client from the docker environment.
	c, err := client.New(client.FromEnv)
	if err != nil {
		return &DockerDiskUsage{}, err
	}
	defer c.Close()

	d, err := c.DiskUsage(ctx, client.DiskUsageOptions{
		Containers: true,
		Images:     true,
		BuildCache: true,
	})
	if err != nil {
		return &DockerDiskUsage{}, err
	}

	return &DockerDiskUsage{
		ImagesTotal:    int(d.Images.TotalCount),
		ImagesActive:   d.Images.ActiveCount,
		ImagesSize:     d.Images.TotalSize,
		ContainersSize: d.Containers.TotalSize,
		BuildCacheSize: d.BuildCache.TotalSize,
	}, nil
}

func PruneDockerImages(ctx context.Context) (image.PruneReport, error) {
	// TODO: find a way to re-use the client from the docker environment.
	c, err := client.New(client.FromEnv)
	if err != nil {
		return image.PruneReport{}, err
	}
	defer c.Close()

	prune, err := c.ImagePrune(ctx, client.ImagePruneOptions{})
	if err != nil {
		return image.PruneReport{}, err
	}
	return prune.Report, nil
}

func GetDockerInfo(ctx context.Context) (client.ServerVersionResult, system.Info, error) {
	// TODO: find a way to re-use the client from the docker environment.
	c, err := client.New(client.FromEnv)
	if err != nil {
		return client.ServerVersionResult{}, system.Info{}, err
	}
	defer c.Close()

	dockerVersion, err := c.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return client.ServerVersionResult{}, system.Info{}, err
	}

	dockerInfo, err := c.Info(ctx, client.InfoOptions{})
	if err != nil {
		return client.ServerVersionResult{}, system.Info{}, err
	}

	return dockerVersion, dockerInfo.Info, nil
}
