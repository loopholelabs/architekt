package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/loopholelabs/drafter/pkg/common"
	"github.com/loopholelabs/drafter/pkg/snapshotter"
)

func main() {
	rawFirecrackerBin := flag.String("firecracker-bin", "firecracker", "Firecracker binary")
	rawJailerBin := flag.String("jailer-bin", "jailer", "Jailer binary (from Firecracker)")

	chrootBaseDir := flag.String("chroot-base-dir", filepath.Join("out", "vms"), "chroot base directory")

	uid := flag.Int("uid", 0, "User ID for the Firecracker process")
	gid := flag.Int("gid", 0, "Group ID for the Firecracker process")

	enableOutput := flag.Bool("enable-output", true, "Whether to enable VM stdout and stderr")
	enableInput := flag.Bool("enable-input", false, "Whether to enable VM stdin")

	resumeTimeout := flag.Duration("resume-timeout", time.Minute, "Maximum amount of time to wait for agent and liveness to resume")

	netns := flag.String("netns", "ark0", "Network namespace to run Firecracker in")
	iface := flag.String("interface", "tap0", "Name of the interface in the network namespace to use")
	mac := flag.String("mac", "02:0e:d9:fd:68:3d", "MAC of the interface in the network namespace to use")

	numaNode := flag.Int("numa-node", 0, "NUMA node to run Firecracker in")
	cgroupVersion := flag.Int("cgroup-version", 2, "Cgroup version to use for Jailer")

	livenessVSockPort := flag.Int("liveness-vsock-port", 25, "Liveness VSock port")
	agentVSockPort := flag.Int("agent-vsock-port", 26, "Agent VSock port")

	defDevices := make([]snapshotter.SnapshotDevice, 0)
	for _, n := range common.KnownNames {
		sd := snapshotter.SnapshotDevice{
			Name:   n,
			Output: filepath.Join("out", "package", common.DeviceFilenames[n]),
		}
		if n == common.DeviceKernelName ||
			n == common.DeviceDiskName ||
			n == common.DeviceOCIName {
			sd.Output = filepath.Join("out", "blueprint", common.DeviceFilenames[n])
		}
		defDevices = append(defDevices, sd)
	}
	defaultDevices, err := json.Marshal(defDevices)

	if err != nil {
		panic(err)
	}

	rawDevices := flag.String("devices", string(defaultDevices), "Devices configuration")

	cpuCount := flag.Int("cpu-count", 1, "CPU count")
	memorySize := flag.Int("memory-size", 1024, "Memory size (in MB)")
	cpuTemplate := flag.String("cpu-template", "None", "Firecracker CPU template (see https://github.com/firecracker-microvm/firecracker/blob/main/docs/cpu_templates/cpu-templates.md#static-cpu-templates for the options)")
	bootArgs := flag.String("boot-args", snapshotter.DefaultBootArgs, "Boot/kernel arguments")

	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var devices []snapshotter.SnapshotDevice
	if err := json.Unmarshal([]byte(*rawDevices), &devices); err != nil {
		panic(err)
	}

	firecrackerBin, err := exec.LookPath(*rawFirecrackerBin)
	if err != nil {
		panic(err)
	}

	jailerBin, err := exec.LookPath(*rawJailerBin)
	if err != nil {
		panic(err)
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt)
	go func() {
		<-done
		log.Println("Exiting gracefully")
		cancel()
	}()

	err = snapshotter.CreateSnapshot(ctx, devices,
		snapshotter.VMConfiguration{
			CPUCount:    *cpuCount,
			MemorySize:  *memorySize,
			CPUTemplate: *cpuTemplate,
			BootArgs:    *bootArgs,
		},
		snapshotter.LivenessConfiguration{
			LivenessVSockPort: uint32(*livenessVSockPort),
			ResumeTimeout:     *resumeTimeout,
		},

		snapshotter.HypervisorConfiguration{
			FirecrackerBin: firecrackerBin,
			JailerBin:      jailerBin,
			ChrootBaseDir:  *chrootBaseDir,
			UID:            *uid,
			GID:            *gid,
			NetNS:          *netns,
			NumaNode:       *numaNode,
			CgroupVersion:  *cgroupVersion,
			EnableOutput:   *enableOutput,
			EnableInput:    *enableInput,
		},
		snapshotter.NetworkConfiguration{
			Interface: *iface,
			MAC:       *mac,
		},
		snapshotter.AgentConfiguration{
			AgentVSockPort: uint32(*agentVSockPort),
			ResumeTimeout:  *resumeTimeout,
		},
	)

	if err != nil {
		panic(err)
	}

	log.Println("Shutting down")
}
