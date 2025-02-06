package main

import (
	"context"
	"flag"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/loopholelabs/drafter/pkg/common"
	"github.com/loopholelabs/drafter/pkg/ipc"
	"github.com/loopholelabs/drafter/pkg/peer"
	"github.com/loopholelabs/drafter/pkg/runner"
	"github.com/loopholelabs/drafter/pkg/runtimes"
	"github.com/loopholelabs/drafter/pkg/snapshotter"
	"github.com/loopholelabs/logging"
	"github.com/loopholelabs/logging/types"
	"github.com/loopholelabs/silo/pkg/storage/metrics"
	siloprom "github.com/loopholelabs/silo/pkg/storage/metrics/prometheus"
	"github.com/loopholelabs/silo/pkg/storage/migrator"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {

	// General flags
	rawDevices := flag.String("devices", getDefaultDevices(), "Devices configuration")

	raddr := flag.String("raddr", "localhost:1337", "Remote address to connect to (leave empty to disable)")
	laddr := flag.String("laddr", "localhost:1337", "Local address to listen on (leave empty to disable)")
	concurrency := flag.Int("concurrency", 1024, "Number of concurrent workers to use in migrations")

	// Firewcracker flags
	rawFirecrackerBin := flag.String("firecracker-bin", "firecracker", "Firecracker binary")
	rawJailerBin := flag.String("jailer-bin", "jailer", "Jailer binary (from Firecracker)")
	chrootBaseDir := flag.String("chroot-base-dir", filepath.Join("out", "vms"), "chroot base directory")
	uid := flag.Int("uid", 0, "User ID for the Firecracker process")
	gid := flag.Int("gid", 0, "Group ID for the Firecracker process")
	enableOutput := flag.Bool("enable-output", true, "Whether to enable VM stdout and stderr")
	enableInput := flag.Bool("enable-input", false, "Whether to enable VM stdin")
	netns := flag.String("netns", "ark0", "Network namespace to run Firecracker in")
	numaNode := flag.Int("numa-node", 0, "NUMA node to run Firecracker in")
	cgroupVersion := flag.Int("cgroup-version", 2, "Cgroup version to use for Jailer")

	resumeTimeout := flag.Duration("resume-timeout", time.Minute, "Maximum amount of time to wait for agent and liveness to resume")
	rescueTimeout := flag.Duration("rescue-timeout", time.Minute, "Maximum amount of time to wait for rescue operations")

	experimentalMapPrivate := flag.Bool("experimental-map-private", false, "(Experimental) Whether to use MAP_PRIVATE for memory and state devices")
	experimentalMapPrivateStateOutput := flag.String("experimental-map-private-state-output", "", "(Experimental) Path to write the local changes to the shared state to (leave empty to write back to device directly) (ignored unless --experimental-map-private)")
	experimentalMapPrivateMemoryOutput := flag.String("experimental-map-private-memory-output", "", "(Experimental) Path to write the local changes to the shared memory to (leave empty to write back to device directly) (ignored unless --experimental-map-private)")

	serveMetrics := flag.String("metrics", "", "Address to serve metrics from")

	disablePostcopyMigration := flag.Bool("disable-postcopy-migration", false, "Whether to disable post-copy migration")

	flag.Parse()

	var reg *prometheus.Registry
	var siloMetrics metrics.SiloMetrics
	if *serveMetrics != "" {
		reg = prometheus.NewRegistry()

		siloMetrics = siloprom.New(reg, siloprom.DefaultConfig())

		// Add the default go metrics
		reg.MustRegister(
			collectors.NewGoCollector(),
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		)

		http.Handle("/metrics", promhttp.HandlerFor(
			reg,
			promhttp.HandlerOpts{
				// Opt into OpenMetrics to support exemplars.
				EnableOpenMetrics: true,
				// Pass custom registry
				Registry: reg,
			},
		))

		go http.ListenAndServe(*serveMetrics, nil)
	}

	devices, err := decodeDevices(*rawDevices)
	if err != nil {
		panic(err)
	}

	// FIXME: Allow tweak from cmdline
	log := logging.New(logging.Zerolog, "drafter", os.Stderr)
	log.SetLevel(types.DebugLevel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle Interrupt by cancelling context
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt)
	go func() {
		<-done
		log.Info().Msg("Exiting gracefully")
		cancel()
	}()

	firecrackerBin, err := exec.LookPath(*rawFirecrackerBin)
	if err != nil {
		log.Error().Err(err).Msg("Could not find firecracker bin")
		panic(err)
	}

	jailerBin, err := exec.LookPath(*rawJailerBin)
	if err != nil {
		log.Error().Err(err).Msg("Could not find jailer bin")
		panic(err)
	}

	rp := &runtimes.FirecrackerRuntimeProvider[struct{}, ipc.AgentServerRemote[struct{}], struct{}]{
		Log: log,
		HypervisorConfiguration: snapshotter.HypervisorConfiguration{
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
		StateName:        common.DeviceStateName,
		MemoryName:       common.DeviceMemoryName,
		AgentServerLocal: struct{}{},
		AgentServerHooks: ipc.AgentServerAcceptHooks[ipc.AgentServerRemote[struct{}], struct{}]{},
		SnapshotLoadConfiguration: runner.SnapshotLoadConfiguration{
			ExperimentalMapPrivate:             *experimentalMapPrivate,
			ExperimentalMapPrivateStateOutput:  *experimentalMapPrivateStateOutput,
			ExperimentalMapPrivateMemoryOutput: *experimentalMapPrivateMemoryOutput,
		},
	}

	p, err := peer.StartPeer(ctx,
		context.Background(), // Never give up on rescue operations
		log, siloMetrics, rp,
	)

	if err != nil {
		log.Error().Err(err).Msg("Could not start peer")
		panic(err)
	}

	go func() {
		err := <-p.BackgroundErr()
		log.Error().Err(err).Msg("Error from peer background tasks")
		cancel()
	}()

	defer func() {
		err := p.Close()
		if err != nil {
			panic(err)
		}
	}()

	migrateFromDevices := []common.MigrateFromDevice{}
	for _, device := range devices {
		migrateFromDevices = append(migrateFromDevices, common.MigrateFromDevice{
			Name:        device.Name,
			Base:        device.Base,
			Overlay:     device.Overlay,
			State:       device.State,
			BlockSize:   device.BlockSize,
			Shared:      device.Shared,
			SharedBase:  device.SharedBase,
			S3Sync:      device.S3Sync,
			S3AccessKey: device.S3AccessKey,
			S3SecretKey: device.S3SecretKey,
			S3Endpoint:  device.S3Endpoint,
			S3Bucket:    device.S3Bucket,
		})
	}

	var readers []io.Reader
	var writers []io.Writer
	var closer io.Closer
	var remoteAddr string

	if *raddr != "" {
		closer, readers, writers, remoteAddr, err = connectAddr(ctx, *raddr)
		if err != nil {
			panic(err)
		}
		defer closer.Close()

		log.Info().Str("remote", remoteAddr).Msg("Migrating from")
	}

	ready := make(chan struct{})
	err = p.MigrateFrom(ctx, migrateFromDevices, readers, writers,
		peer.MigrateFromHooks{
			OnLocalDeviceRequested: func(localDeviceID uint32, name string) {
				log.Info().Uint32("deviceID", localDeviceID).Str("name", name).Msg("Requested local device")
			},
			OnLocalDeviceExposed: func(localDeviceID uint32, path string) {
				log.Info().Uint32("deviceID", localDeviceID).Str("path", path).Msg("Exposed local device")
			},
			OnLocalAllDevicesRequested: func() {
				log.Info().Msg("Requested all local devices")
			},
			OnCompletion: func() {
				log.Info().Msg("Finished all migrations")

				close(ready)
			},
		},
	)

	if err != nil {
		log.Error().Err(err).Msg("Could not migrate peer")
		panic(err)
	}

	// If we disable post copy migration, wait here until the data is available.
	if *disablePostcopyMigration {
		select {
		case <-ctx.Done():
		case <-ready:
		}
	}

	before := time.Now()

	err = p.Resume(ctx, *resumeTimeout, *rescueTimeout)

	if err != nil {
		log.Error().Err(err).Msg("Could not resume peer")
		panic(err)
	}

	log.Info().Int64("ms", time.Since(before).Milliseconds()).Str("path", p.VMPath).Msg("Resumed VM")

	if strings.TrimSpace(*laddr) != "" {

		log.Info().Str("addr", *laddr).Msg("Listening for connections")
		closer, readers, writers, remoteAddr, err = listenAddr(ctx, *laddr)
		if err != nil {
			if err == context.Canceled {
				// We don't care
			} else {
				log.Error().Err(err).Msg("Error listening for connection")
				panic(err)
			}
		} else {
			defer closer.Close()

			log.Info().Str("addr", remoteAddr).Msg("Migrating to")

			migrateToDevices := []common.MigrateToDevice{}
			for _, device := range devices {
				if !device.MakeMigratable || device.Shared {
					continue
				}

				migrateToDevices = append(migrateToDevices, common.MigrateToDevice{
					Name:           device.Name,
					MaxDirtyBlocks: device.MaxDirtyBlocks,
					MinCycles:      device.MinCycles,
					MaxCycles:      device.MaxCycles,
					CycleThrottle:  device.CycleThrottle,
				})
			}

			before = time.Now()
			err = p.MigrateTo(
				ctx,
				migrateToDevices,
				*resumeTimeout,
				*concurrency,
				readers,
				writers,
				peer.MigrateToHooks{
					OnBeforeSuspend: func() {
						before = time.Now()
					},
					OnAfterSuspend: func() {
						log.Info().Int64("ms", time.Since(before).Milliseconds()).Msg("Suspend")
					},
					OnAllMigrationsCompleted: func() {
						log.Info().Msg("Completed all device migrations")
					},
					OnProgress: func(p map[string]*migrator.MigrationProgress) {
						totalSize := 0
						totalDone := 0
						for _, prog := range p {
							totalSize += (prog.TotalBlocks * prog.BlockSize)
							totalDone += (prog.ReadyBlocks * prog.BlockSize)
						}

						perc := float64(0.0)
						if totalSize > 0 {
							perc = float64(totalDone) * 100 / float64(totalSize)
						}
						// Report overall migration progress
						log.Info().Float64("perc", perc).
							Int("done", totalDone).
							Int("size", totalSize).
							Msg("# Overall migration Progress #")

						// Report individual devices
						for name, prog := range p {
							log.Info().Str("name", name).
								Int("migratedBlocks", prog.MigratedBlocks).
								Int("totalBlocks", prog.TotalBlocks).
								Int("readyBlocks", prog.ReadyBlocks).
								Int("totalMigratedBlocks", prog.TotalMigratedBlocks).
								Msg("Device migration progress")
						}

					},
				},
			)
			if err != nil {
				log.Error().Err(err).Msg("Error migrating to")
				panic(err)
			}
			cancel() // Cancel the context, since we're done.
		}
	}

	// Wait for context done
	<-ctx.Done()

	log.Info().Msg("Shutting down")
}
