package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loopholelabs/drafter/pkg/common"
	"github.com/loopholelabs/drafter/pkg/mounter"
	"github.com/loopholelabs/goroutine-manager/pkg/manager"
	"github.com/loopholelabs/silo/pkg/storage/migrator"
)

type CompositeDevices struct {
	Name string `json:"name"`

	Base    string `json:"base"`
	Overlay string `json:"overlay"`
	State   string `json:"state"`

	BlockSize uint32 `json:"blockSize"`

	Expiry time.Duration `json:"expiry"`

	MaxDirtyBlocks int `json:"maxDirtyBlocks"`
	MinCycles      int `json:"minCycles"`
	MaxCycles      int `json:"maxCycles"`

	CycleThrottle time.Duration `json:"cycleThrottle"`

	MakeMigratable bool `json:"makeMigratable"`
}

func main() {
	defaultDevices, err := json.Marshal([]CompositeDevices{
		{
			Name: common.DeviceStateName,

			Base:    filepath.Join("out", "package", "state.bin"),
			Overlay: filepath.Join("out", "overlay", "state.bin"),
			State:   filepath.Join("out", "state", "state.bin"),

			BlockSize: 1024 * 64,

			Expiry: time.Second,

			MaxDirtyBlocks: 200,
			MinCycles:      5,
			MaxCycles:      20,

			CycleThrottle: time.Millisecond * 500,

			MakeMigratable: true,
		},
		{
			Name: common.DeviceMemoryName,

			Base:    filepath.Join("out", "package", "memory.bin"),
			Overlay: filepath.Join("out", "overlay", "memory.bin"),
			State:   filepath.Join("out", "state", "memory.bin"),

			BlockSize: 1024 * 64,

			Expiry: time.Second,

			MaxDirtyBlocks: 200,
			MinCycles:      5,
			MaxCycles:      20,

			CycleThrottle: time.Millisecond * 500,

			MakeMigratable: true,
		},

		{
			Name: common.DeviceKernelName,

			Base:    filepath.Join("out", "package", "vmlinux"),
			Overlay: filepath.Join("out", "overlay", "vmlinux"),
			State:   filepath.Join("out", "state", "vmlinux"),

			BlockSize: 1024 * 64,

			Expiry: time.Second,

			MaxDirtyBlocks: 200,
			MinCycles:      5,
			MaxCycles:      20,

			CycleThrottle: time.Millisecond * 500,

			MakeMigratable: true,
		},
		{
			Name: common.DeviceDiskName,

			Base:    filepath.Join("out", "package", "rootfs.ext4"),
			Overlay: filepath.Join("out", "overlay", "rootfs.ext4"),
			State:   filepath.Join("out", "state", "rootfs.ext4"),

			BlockSize: 1024 * 64,

			Expiry: time.Second,

			MaxDirtyBlocks: 200,
			MinCycles:      5,
			MaxCycles:      20,

			CycleThrottle: time.Millisecond * 500,

			MakeMigratable: true,
		},

		{
			Name: common.DeviceConfigName,

			Base:    filepath.Join("out", "package", "config.json"),
			Overlay: filepath.Join("out", "overlay", "config.json"),
			State:   filepath.Join("out", "state", "config.json"),

			BlockSize: 1024 * 64,

			Expiry: time.Second,

			MaxDirtyBlocks: 200,
			MinCycles:      5,
			MaxCycles:      20,

			CycleThrottle: time.Millisecond * 500,

			MakeMigratable: true,
		},

		{
			Name: "oci",

			Base:    filepath.Join("out", "package", "oci.ext4"),
			Overlay: filepath.Join("out", "overlay", "oci.ext4"),
			State:   filepath.Join("out", "state", "oci.ext4"),

			BlockSize: 1024 * 64,

			Expiry: time.Second,

			MaxDirtyBlocks: 200,
			MinCycles:      5,
			MaxCycles:      20,

			CycleThrottle: time.Millisecond * 500,

			MakeMigratable: true,
		},
	})
	if err != nil {
		panic(err)
	}

	rawDevices := flag.String("devices", string(defaultDevices), "Devices configuration")

	raddr := flag.String("raddr", "localhost:1337", "Remote address to connect to (leave empty to disable)")
	laddr := flag.String("laddr", "localhost:1337", "Local address to listen on (leave empty to disable)")

	concurrency := flag.Int("concurrency", 1024, "Number of concurrent workers to use in migrations")

	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var devices []CompositeDevices
	if err := json.Unmarshal([]byte(*rawDevices), &devices); err != nil {
		panic(err)
	}

	var errs error
	defer func() {
		if errs != nil {
			panic(errs)
		}
	}()

	goroutineManager := manager.NewGoroutineManager(
		ctx,
		&errs,
		manager.GoroutineManagerHooks{},
	)
	defer goroutineManager.Wait()
	defer goroutineManager.StopAllGoroutines()
	//	defer goroutineManager.CreateBackgroundPanicCollector()()

	bubbleSignals := false

	done := make(chan os.Signal, 1)
	go func() {
		signal.Notify(done, os.Interrupt)

		v := <-done

		if bubbleSignals {
			done <- v

			return
		}

		log.Println("Exiting gracefully")

		cancel()
	}()

	var (
		readers []io.Reader
		writers []io.Writer
	)
	if strings.TrimSpace(*raddr) != "" {
		conn, err := (&net.Dialer{}).DialContext(goroutineManager.Context(), "tcp", *raddr)
		if err != nil {
			panic(err)
		}
		defer conn.Close()

		log.Println("Migrating from", conn.RemoteAddr())

		readers = []io.Reader{conn}
		writers = []io.Writer{conn}
	}

	migrateFromDevices := []mounter.MigrateFromAndMountDevice{}
	for _, device := range devices {
		migrateFromDevices = append(migrateFromDevices, mounter.MigrateFromAndMountDevice{
			Name: device.Name,

			Base:    device.Base,
			Overlay: device.Overlay,
			State:   device.State,

			BlockSize: device.BlockSize,
		})
	}

	migratedMounter, err := mounter.MigrateFromAndMount(
		goroutineManager.Context(),
		goroutineManager.Context(),

		migrateFromDevices,

		readers,
		writers,

		mounter.MigrateFromHooks{
			OnLocalDeviceRequested: func(localDeviceID uint32, name string) {
				log.Println("Requested local device", localDeviceID, "with name", name)
			},
			OnLocalDeviceExposed: func(localDeviceID uint32, path string) {
				log.Println("Exposed local device", localDeviceID, "at", path)
			},

			OnLocalAllDevicesRequested: func() {
				log.Println("Requested all local devices")
			},
		},
	)

	defer func() {
		defer goroutineManager.CreateForegroundPanicCollector()()

		if err := migratedMounter.Wait(); err != nil {
			panic(err)
		}
	}()

	if err != nil {
		panic(err)
	}

	defer func() {
		defer goroutineManager.CreateForegroundPanicCollector()()

		if err := migratedMounter.Close(); err != nil {
			panic(err)
		}
	}()

	goroutineManager.StartForegroundGoroutine(func(_ context.Context) {
		if err := migratedMounter.Wait(); err != nil {
			panic(err)
		}
	})

	log.Println("Mounted devices", func() string {
		deviceMap := ""

		for i, device := range migratedMounter.Devices {
			if i > 0 {
				deviceMap += ", "
			}

			deviceMap += device.Name + " on " + device.Path
		}

		return deviceMap
	}())

	if err := migratedMounter.Wait(); err != nil {
		panic(err)
	}

	if strings.TrimSpace(*laddr) == "" {
		bubbleSignals = true

		select {
		case <-goroutineManager.Context().Done():
			return

		case <-done:
			log.Println("Shutting down")

			return
		}
	}

	var (
		closeLock sync.Mutex
		closed    bool
	)
	lis, err := net.Listen("tcp", *laddr)
	if err != nil {
		panic(err)
	}
	defer func() {
		defer goroutineManager.CreateForegroundPanicCollector()()

		closeLock.Lock()

		closed = true

		closeLock.Unlock()

		if err := lis.Close(); err != nil {
			panic(err)
		}
	}()

	log.Println("Serving on", lis.Addr())

l:
	for {
		var (
			ready       = make(chan struct{})
			signalReady = sync.OnceFunc(func() {
				close(ready) // We can safely close() this channel since the caller only runs once/is `sync.OnceFunc`d
			})
		)

		var conn net.Conn
		goroutineManager.StartForegroundGoroutine(func(_ context.Context) {
			conn, err = lis.Accept()
			if err != nil {
				closeLock.Lock()
				defer closeLock.Unlock()

				if closed && errors.Is(err, net.ErrClosed) { // Don't treat closed errors as errors if we closed the connection
					if err := goroutineManager.Context().Err(); err != nil {
						panic(err)
					}

					return
				}

				panic(err)
			}

			signalReady()
		})

		bubbleSignals = true

	s:
		select {
		case <-goroutineManager.Context().Done():
			return

		case <-done:
			break l

		case <-ready:
			break s
		}

		if err := func() error {
			defer conn.Close()

			log.Println("Migrating to", conn.RemoteAddr())

			makeMigratableDevices := []mounter.MakeMigratableDevice{}
			for _, device := range devices {
				if !device.MakeMigratable {
					continue
				}

				makeMigratableDevices = append(makeMigratableDevices, mounter.MakeMigratableDevice{
					Name: device.Name,

					Expiry: device.Expiry,
				})
			}

			migratableMounter, err := migratedMounter.MakeMigratable(
				goroutineManager.Context(),

				makeMigratableDevices,
			)

			if err != nil {
				return err
			}

			defer migratableMounter.Close()

			migrateToDevices := []common.MigrateToDevice{}
			for _, device := range devices {
				if !device.MakeMigratable {
					continue
				}

				migrateToDevices = append(migrateToDevices, common.MigrateToDevice{
					Name: device.Name,

					MaxDirtyBlocks: device.MaxDirtyBlocks,
					MinCycles:      device.MinCycles,
					MaxCycles:      device.MaxCycles,

					CycleThrottle: device.CycleThrottle,
				})
			}

			return migratableMounter.MigrateTo(
				goroutineManager.Context(),

				migrateToDevices,

				*concurrency,

				[]io.Reader{conn},
				[]io.Writer{conn},

				mounter.MounterMigrateToHooks{
					OnBeforeGetDirtyBlocks: func(deviceID uint32, remote bool) {
						if remote {
							log.Println("Getting dirty blocks for remote device", deviceID)
						} else {
							log.Println("Getting dirty blocks for local device", deviceID)
						}
					},

					OnDeviceSent: func(deviceID uint32, remote bool) {
						if remote {
							log.Println("Sent remote device", deviceID)
						} else {
							log.Println("Sent local device", deviceID)
						}
					},
					OnDeviceAuthoritySent: func(deviceID uint32, remote bool) {
						if remote {
							log.Println("Sent authority for remote device", deviceID)
						} else {
							log.Println("Sent authority for local device", deviceID)
						}
					},
					OnDeviceInitialMigrationProgress: func(deviceID uint32, remote bool, ready, total int) {
						if remote {
							log.Println("Migrated", ready, "of", total, "initial blocks for remote device", deviceID)
						} else {
							log.Println("Migrated", ready, "of", total, "initial blocks for local device", deviceID)
						}
					},
					OnDeviceContinousMigrationProgress: func(deviceID uint32, remote bool, delta int) {
						if remote {
							log.Println("Migrated", delta, "continous blocks for remote device", deviceID)
						} else {
							log.Println("Migrated", delta, "continous blocks for local device", deviceID)
						}
					},
					OnDeviceFinalMigrationProgress: func(deviceID uint32, remote bool, delta int) {
						if remote {
							log.Println("Migrated", delta, "final blocks for remote device", deviceID)
						} else {
							log.Println("Migrated", delta, "final blocks for local device", deviceID)
						}
					},
					OnDeviceMigrationCompleted: func(deviceID uint32, remote bool) {
						if remote {
							log.Println("Completed migration of remote device", deviceID)
						} else {
							log.Println("Completed migration of local device", deviceID)
						}
					},

					OnAllDevicesSent: func() {
						log.Println("Sent all devices")
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
						log.Printf("# Overall migration Progress # (%d / %d) %.1f%%\n", totalDone, totalSize, perc)

						// Report individual devices
						for name, prog := range p {
							log.Printf(" [%s] Progress Migrated Blocks (%d/%d)  %d ready %d total\n", name, prog.MigratedBlocks, prog.TotalBlocks, prog.ReadyBlocks, prog.TotalMigratedBlocks)
						}

					},
				},
			)
		}(); err != nil {
			fmt.Printf("ERROR %v\n", err)
			panic(err)
		}
	}

	log.Println("Shutting down")
}
