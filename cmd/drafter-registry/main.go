package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/loopholelabs/drafter/pkg/common"
	"github.com/loopholelabs/drafter/pkg/registry"
	"github.com/loopholelabs/goroutine-manager/pkg/manager"
)

func main() {
	defDevices := make([]registry.RegistryDevice, 0)
	for _, n := range common.KnownNames {
		defDevices = append(defDevices, registry.RegistryDevice{
			Name:      n,
			Input:     filepath.Join("out", "package", common.DeviceFilenames[n]),
			BlockSize: 1024 * 64,
		})
	}
	defaultDevices, err := json.Marshal(defDevices)
	if err != nil {
		panic(err)
	}

	rawDevices := flag.String("devices", string(defaultDevices), "Devices configuration")

	laddr := flag.String("laddr", ":1600", "Address to listen on")

	concurrency := flag.Int("concurrency", 1024, "Number of concurrent workers to use in migrations")

	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var devices []registry.RegistryDevice
	if err := json.Unmarshal([]byte(*rawDevices), &devices); err != nil {
		panic(err)
	}

	lis, err := net.Listen("tcp", *laddr)
	if err != nil {
		panic(err)
	}
	defer lis.Close()

	log.Println("Serving on", lis.Addr())

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
	defer goroutineManager.CreateBackgroundPanicCollector()()

	go func() {
		done := make(chan os.Signal, 1)
		signal.Notify(done, os.Interrupt)

		<-done

		log.Println("Exiting gracefully")

		cancel()

		defer goroutineManager.CreateForegroundPanicCollector()()

		if lis != nil {
			if err := lis.Close(); err != nil {
				panic(err)
			}
		}
	}()

l:
	for {
		conn, err := lis.Accept()
		if err != nil {
			select {
			case <-goroutineManager.Context().Done():
				break l

			default:
				panic(err)
			}
		}

		log.Println("Migrating to", conn.RemoteAddr())

		go func() {
			defer conn.Close()
			defer func() {
				if err := recover(); err != nil {
					var e error
					if v, ok := err.(error); ok {
						e = v
					} else {
						e = fmt.Errorf("%v", err)
					}

					log.Printf("Registry client disconnected with error: %v", e)
				}
			}()

			openedDevices, defers, err := registry.OpenDevices(
				devices,

				registry.OpenDevicesHooks{
					OnDeviceOpened: func(deviceID uint32, name string) {
						log.Println("Opened device", deviceID, "with name", name)
					},
				},
			)
			if err != nil {
				panic(err)
			}

			for _, deferFunc := range defers {
				defer deferFunc()
			}

			if err := registry.MigrateTo(
				goroutineManager.Context(),

				openedDevices,
				*concurrency,

				[]io.Reader{conn},
				[]io.Writer{conn},

				registry.MigrateToHooks{
					OnDeviceSent: func(deviceID uint32) {
						log.Println("Sent device", deviceID)
					},
					OnDeviceAuthoritySent: func(deviceID uint32) {
						log.Println("Sent authority for device", deviceID)
					},
					OnDeviceMigrationProgress: func(deviceID uint32, ready, total int) {
						log.Println("Migrated", ready, "of", total, "blocks for device", deviceID)
					},
					OnDeviceMigrationCompleted: func(deviceID uint32) {
						log.Println("Completed migration of device", deviceID)
					},

					OnAllDevicesSent: func() {
						log.Println("Sent all devices")
					},
					OnAllMigrationsCompleted: func() {
						log.Println("Completed all device migrations")
					},
				},
			); err != nil {
				panic(err)
			}
		}()
	}

	log.Println("Shutting down")
}
