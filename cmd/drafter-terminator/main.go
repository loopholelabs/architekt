package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/loopholelabs/drafter/pkg/common"
	"github.com/loopholelabs/drafter/pkg/terminator"
	"github.com/loopholelabs/goroutine-manager/pkg/manager"
)

func main() {
	defDevices := make([]terminator.TerminatorDevice, 0)
	for _, n := range common.KnownNames {
		defDevices = append(defDevices, terminator.TerminatorDevice{
			Name:   n,
			Output: filepath.Join("out", "package", common.DeviceFilenames[n]),
		})
	}
	defaultDevices, err := json.Marshal(defDevices)
	if err != nil {
		panic(err)
	}

	rawDevices := flag.String("devices", string(defaultDevices), "Devices configuration")

	raddr := flag.String("raddr", "localhost:1337", "Remote address to connect to")

	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var devices []terminator.TerminatorDevice
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
	defer goroutineManager.CreateBackgroundPanicCollector()()

	go func() {
		done := make(chan os.Signal, 1)
		signal.Notify(done, os.Interrupt)

		<-done

		log.Println("Exiting gracefully")

		cancel()
	}()

	conn, err := (&net.Dialer{}).DialContext(goroutineManager.Context(), "tcp", *raddr)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	log.Println("Migrating from", conn.RemoteAddr())

	if err := terminator.Terminate(
		goroutineManager.Context(),

		devices,

		[]io.Reader{conn},
		[]io.Writer{conn},

		terminator.TerminateHooks{
			OnDeviceReceived: func(deviceID uint32, name string) {
				log.Println("Received device", deviceID, "with name", name)
			},
			OnDeviceAuthorityReceived: func(deviceID uint32) {
				log.Println("Received authority for device", deviceID)
			},
			OnDeviceMigrationCompleted: func(deviceID uint32) {
				log.Println("Completed migration of device", deviceID)
			},

			OnAllDevicesReceived: func() {
				log.Println("Received all devices")
			},
			OnAllMigrationsCompleted: func() {
				log.Println("Completed all device migrations")
			},
		},
	); err != nil {
		panic(err)
	}

	log.Println("Shutting down")
}
