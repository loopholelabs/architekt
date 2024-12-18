package peer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/loopholelabs/drafter/pkg/mounter"
	"github.com/loopholelabs/drafter/pkg/registry"
	"github.com/loopholelabs/drafter/pkg/snapshotter"
	"github.com/loopholelabs/goroutine-manager/pkg/manager"
	"github.com/loopholelabs/silo/pkg/storage/config"
	"github.com/loopholelabs/silo/pkg/storage/devicegroup"
	"github.com/loopholelabs/silo/pkg/storage/protocol"
	"github.com/loopholelabs/silo/pkg/storage/protocol/packets"
	"golang.org/x/sys/unix"
)

type MigrateFromDevice struct {
	Name      string `json:"name"`
	Base      string `json:"base"`
	Overlay   string `json:"overlay"`
	State     string `json:"state"`
	BlockSize uint32 `json:"blockSize"`
	Shared    bool   `json:"shared"`
}

// expose a Silo Device as a file within the vm directory
func exposeSiloDeviceAsFile(vmpath string, name string, devicePath string) error {
	deviceInfo, err := os.Stat(devicePath)
	if err != nil {
		return errors.Join(snapshotter.ErrCouldNotGetDeviceStat, err)
	}

	deviceStat, ok := deviceInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return ErrCouldNotGetNBDDeviceStat
	}

	err = unix.Mknod(filepath.Join(vmpath, name), unix.S_IFBLK|0666, int(deviceStat.Rdev))
	if err != nil {
		return errors.Join(ErrCouldNotCreateDeviceNode, err)
	}

	return nil
}

/**
 * This creates a Silo Dev Schema given a MigrateFromDevice
 * If you want to change the type of storage used, or Silo options, you can do so here.
 *
 */
func createSiloDevSchema(i *MigrateFromDevice) (*config.DeviceSchema, error) {
	stat, err := os.Stat(i.Base)
	if err != nil {
		return nil, errors.Join(mounter.ErrCouldNotGetBaseDeviceStat, err)
	}

	ds := &config.DeviceSchema{
		Name:      i.Name,
		BlockSize: fmt.Sprintf("%v", i.BlockSize),
		Expose:    true,
		Size:      fmt.Sprintf("%v", stat.Size()),
	}
	if strings.TrimSpace(i.Overlay) == "" || strings.TrimSpace(i.State) == "" {
		ds.System = "file"
		ds.Location = i.Base
	} else {
		err := os.MkdirAll(filepath.Dir(i.Overlay), os.ModePerm)
		if err != nil {
			return nil, errors.Join(mounter.ErrCouldNotCreateOverlayDirectory, err)
		}

		err = os.MkdirAll(filepath.Dir(i.State), os.ModePerm)
		if err != nil {
			return nil, errors.Join(mounter.ErrCouldNotCreateStateDirectory, err)
		}

		ds.System = "sparsefile"
		ds.Location = i.Overlay

		ds.ROSource = &config.DeviceSchema{
			Name:     i.State,
			System:   "file",
			Location: i.Base,
			Size:     fmt.Sprintf("%v", stat.Size()),
		}
	}
	return ds, nil
}

/**
 * 'migrate' from the local filesystem.
 *
 */
func migrateFromFS(vmpath string, devices []MigrateFromDevice) (*devicegroup.DeviceGroup, error) {
	siloDeviceSchemas := make([]*config.DeviceSchema, 0)
	for _, input := range devices {
		if input.Shared {
			// Deal with shared devices here...
			err := exposeSiloDeviceAsFile(vmpath, input.Name, input.Base)
			if err != nil {
				return nil, err
			}
		} else {
			ds, err := createSiloDevSchema(&input)
			if err != nil {
				return nil, err
			}
			siloDeviceSchemas = append(siloDeviceSchemas, ds)
		}
	}

	// Create a silo deviceGroup from all the schemas
	dg, err := devicegroup.NewFromSchema(siloDeviceSchemas, nil, nil)
	if err != nil {
		return nil, err
	}

	for _, input := range siloDeviceSchemas {
		dev := dg.GetExposedDeviceByName(input.Name)
		err = exposeSiloDeviceAsFile(vmpath, input.Name, filepath.Join("/dev", dev.Device()))
		if err != nil {
			return nil, err
		}
	}
	return dg, nil
}

func (peer *Peer[L, R, G]) MigrateFrom(
	ctx context.Context,
	devices []MigrateFromDevice,
	readers []io.Reader,
	writers []io.Writer,
	hooks mounter.MigrateFromHooks,
) (
	migratedPeer *MigratedPeer[L, R, G],
	errs error,
) {

	migratedPeer = &MigratedPeer[L, R, G]{
		Wait: func() error {
			return nil
		},

		devices: devices,
		runner:  peer.runner,
	}

	migratedPeer.Close = func() (errs error) {
		// We have to close the runner before we close the devices
		if err := peer.runner.Close(); err != nil {
			return err
		}

		// Close any Silo devices
		migratedPeer.DgLock.Lock()
		if migratedPeer.Dg != nil {
			err := migratedPeer.Dg.CloseAll()
			if err != nil {
				migratedPeer.DgLock.Unlock()
				return err
			}
		}
		migratedPeer.DgLock.Unlock()
		return nil
	}

	///////////////////////////////////////////////////////////////////////////////
	// Under here is still WIP
	///////////////////////////////////////////////////////////////////////////////
	///////////////////////////////////////////////////////////////////////////////
	///////////////////////////////////////////////////////////////////////////////
	///////////////////////////////////////////////////////////////////////////////

	goroutineManager := manager.NewGoroutineManager(
		ctx,
		&errs,
		manager.GoroutineManagerHooks{},
	)
	defer goroutineManager.Wait()
	defer goroutineManager.StopAllGoroutines()
	defer goroutineManager.CreateBackgroundPanicCollector()()

	// Migrate the devices from a protocol
	if len(readers) > 0 && len(writers) > 0 { // Only open the protocol if we want passed in readers and writers
		var pro *protocol.RW
		protocolCtx, cancelProtocolCtx := context.WithCancel(ctx)
		allRemoteDevicesReady := make(chan struct{})

		pro = protocol.NewRW(protocolCtx, readers, writers, nil)

		// Start a goroutine to do the protocol Handle()
		goroutineManager.StartForegroundGoroutine(func(_ context.Context) {
			err := pro.Handle()
			if err != nil && !errors.Is(err, io.EOF) {
				panic(errors.Join(registry.ErrCouldNotHandleProtocol, err))
			}
		})

		fmt.Printf("MigrateFrom...\n")
		// For now...
		names := make([]string, 0)
		var namesLock sync.Mutex
		tweak := func(index int, name string, schema string) string {
			namesLock.Lock()
			names = append(names, name)
			namesLock.Unlock()

			s := strings.ReplaceAll(schema, "instance-0", "instance-1")
			fmt.Printf("Tweaked schema for %s...\n%s\n\n", name, s)
			return string(s)
		}
		events := func(e *packets.Event) {}
		dg, err := devicegroup.NewFromProtocol(context.TODO(), pro, tweak, events, nil, nil)
		if err != nil {
			fmt.Printf("Error migrating %v\n", err)
		}

		// TODO: Setup goroutine better etc etc
		go func() {
			err := dg.HandleCustomData(func(data []byte) {
				fmt.Printf("\n\nCustomData %v\n\n", data)
				if len(data) == 1 && data[0] == byte(registry.EventCustomTransferAuthority) {
					close(allRemoteDevicesReady)
				}
			})
			if err != nil {
				fmt.Printf("HandleCustomData returned %v\n", err)
			}
		}()

		for _, n := range names {
			dev := dg.GetExposedDeviceByName(n)
			if dev != nil {
				err := exposeSiloDeviceAsFile(migratedPeer.runner.VMPath, n, filepath.Join("/dev", dev.Device()))
				if err != nil {
					fmt.Printf("Error migrating %v\n", err)
				}
			}
		}

		migratedPeer.Wait = sync.OnceValue(func() error {
			defer cancelProtocolCtx()
			fmt.Printf(" ### migratedPeer.Wait called\n")

			dg.WaitForCompletion() // FIXME: Should probably return error

			// Save dg for future migrations.
			migratedPeer.DgLock.Lock()
			migratedPeer.Dg = dg
			migratedPeer.DgLock.Unlock()
			return nil
		})

		select {
		case <-goroutineManager.Context().Done():
			if err := goroutineManager.Context().Err(); err != nil {
				panic(errors.Join(ErrPeerContextCancelled, err))
			}

			return
		case <-allRemoteDevicesReady:
			fmt.Printf(" # allRemoteDevicesReady\n")

			break
		}
	}

	//
	// IF all devices are local
	//

	if len(readers) == 0 && len(writers) == 0 {
		dg, err := migrateFromFS(migratedPeer.runner.VMPath, devices)
		if err != nil {
			panic(err)
		}

		// Save dg for later usage, when we want to migrate from here etc
		migratedPeer.DgLock.Lock()
		migratedPeer.Dg = dg
		migratedPeer.DgLock.Unlock()

		if hook := hooks.OnLocalAllDevicesRequested; hook != nil {
			hook()
		}
	}

	return
}
