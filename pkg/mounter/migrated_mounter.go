package mounter

import (
	"context"
	"fmt"
	"io"
	"path"
	"sync"
	"time"

	"github.com/loopholelabs/drafter/pkg/common"
	"github.com/loopholelabs/logging/types"
	"github.com/loopholelabs/silo/pkg/storage/config"
	"github.com/loopholelabs/silo/pkg/storage/devicegroup"
	"github.com/loopholelabs/silo/pkg/storage/metrics"
)

type MigratedDevice struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type MakeMigratableDevice struct {
	Name string `json:"name"`

	Expiry time.Duration `json:"expiry"`
}
type MigratedMounter struct {
	Devices []MigratedDevice

	Wait  func() error
	Close func() error

	Dg     *devicegroup.DeviceGroup
	DgLock sync.Mutex
}

type MigrateFromAndMountDevice struct {
	Name string `json:"name"`

	Base    string `json:"base"`
	Overlay string `json:"overlay"`
	State   string `json:"state"`

	BlockSize uint32 `json:"blockSize"`
}

type MigrateFromHooks struct {
	OnLocalDeviceRequested func(localDeviceID uint32, name string)
	OnLocalDeviceExposed   func(localDeviceID uint32, path string)

	OnLocalAllDevicesRequested func()

	OnXferCustomData func([]byte)
}

func MigrateFromAndMount(
	mounterCtx context.Context,
	migrateFromCtx context.Context,

	devices []MigrateFromAndMountDevice,

	readers []io.Reader,
	writers []io.Writer,

	hooks MigrateFromHooks,
) (
	migratedMounter *MigratedMounter,

	errs error,
) {

	// TODO: Pass these in
	// TODO: This schema tweak function should be exposed / passed in
	var log types.Logger
	var met metrics.SiloMetrics
	tweakRemote := func(index int, name string, schema *config.DeviceSchema) *config.DeviceSchema {

		for _, d := range devices {
			if d.Name == schema.Name {
				// Convert it...
				m := &common.MigrateFromDevice{
					Name:      d.Name,
					Base:      d.Base,
					Overlay:   d.Overlay,
					State:     d.State,
					BlockSize: d.BlockSize,
					Shared:    false,
				}
				newSchema, err := common.CreateIncomingSiloDevSchema(m, schema)
				if err == nil {
					fmt.Printf("Tweaked schema %s\n", newSchema.EncodeAsBlock())
					return newSchema
				}
			}
		}

		// FIXME: Error. We didn't find the local device, or couldn't set it up.

		fmt.Printf("ERROR, didn't find local device defined %s\n", name)

		return schema
	}
	// TODO: Add the sync stuff here...
	tweakLocal := func(index int, name string, schema *config.DeviceSchema) *config.DeviceSchema {
		return schema
	}

	migratedMounter = &MigratedMounter{
		Devices: []MigratedDevice{},

		Wait: func() error {
			return nil
		},
		Close: func() error {
			return nil
		},
	}

	migratedMounter.Close = func() (errs error) {

		// Close any Silo devices
		migratedMounter.DgLock.Lock()
		if migratedMounter.Dg != nil {
			err := migratedMounter.Dg.CloseAll()
			if err != nil {
				migratedMounter.DgLock.Unlock()
				return err
			}
		}
		migratedMounter.DgLock.Unlock()
		return nil
	}

	// Migrate the devices from a protocol
	if len(readers) > 0 && len(writers) > 0 {
		protocolCtx, cancelProtocolCtx := context.WithCancel(migrateFromCtx)

		dg, err := common.MigrateFromPipe(log, met, "", protocolCtx, readers, writers, tweakRemote, hooks.OnXferCustomData)
		if err != nil {
			return nil, err
		}

		migratedMounter.Wait = sync.OnceValue(func() error {
			defer cancelProtocolCtx()

			if dg != nil {
				err := dg.WaitForCompletion()
				if err != nil {
					return err
				}
			}
			return nil
		})

		// Save dg for future migrations, AND for things like reading config
		migratedMounter.DgLock.Lock()
		migratedMounter.Dg = dg
		migratedMounter.DgLock.Unlock()
	}

	//
	// IF all devices are local
	//

	if len(readers) == 0 && len(writers) == 0 {
		newDevices := make([]common.MigrateFromDevice, 0)
		for _, d := range devices {
			newDevices = append(newDevices, common.MigrateFromDevice{
				Name:      d.Name,
				Base:      d.Base,
				Overlay:   d.Overlay,
				State:     d.State,
				BlockSize: d.BlockSize,
				Shared:    false,
			})
		}

		dg, err := common.MigrateFromFS(log, met, "", newDevices, tweakLocal)
		if err != nil {
			return nil, err
		}

		// Save dg for later usage, when we want to migrate from here etc
		migratedMounter.DgLock.Lock()
		migratedMounter.Dg = dg
		migratedMounter.DgLock.Unlock()

		if hook := hooks.OnLocalAllDevicesRequested; hook != nil {
			hook()
		}
	}

	migratedMounter.DgLock.Lock()
	lookupDg := migratedMounter.Dg
	migratedMounter.DgLock.Unlock()
	if lookupDg != nil {
		for i, d := range devices {
			if hooks.OnLocalDeviceExposed != nil {
				exp := lookupDg.GetExposedDeviceByName(d.Name)
				if exp != nil {
					hooks.OnLocalDeviceExposed(uint32(i), path.Join("/dev", exp.Device()))
				}
			}
		}
	}

	return
}

func (migratedMounter *MigratedMounter) MakeMigratable(
	ctx context.Context,
	devices []MakeMigratableDevice,
) (migratableMounter *MigratableMounter, errs error) {
	return &MigratableMounter{
		Dg:    migratedMounter.Dg,
		Close: func() {},
	}, nil
}
