package peer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/loopholelabs/drafter/pkg/ipc"
	"github.com/loopholelabs/drafter/pkg/mounter"
	"github.com/loopholelabs/drafter/pkg/runner"
	"github.com/loopholelabs/drafter/pkg/snapshotter"
	"github.com/loopholelabs/goroutine-manager/pkg/manager"
	"github.com/loopholelabs/logging/types"
	"github.com/loopholelabs/silo/pkg/storage/config"
	"github.com/loopholelabs/silo/pkg/storage/metrics"
)

type Peer[L ipc.AgentServerLocal, R ipc.AgentServerRemote[G], G any] struct {
	VMPath string
	VMPid  int

	Wait  func() error
	Close func() error

	hypervisorCtx context.Context

	runner *runner.Runner[L, R, G]
}

func StartPeer[L ipc.AgentServerLocal, R ipc.AgentServerRemote[G], G any](
	hypervisorCtx context.Context,
	rescueCtx context.Context,

	hypervisorConfiguration snapshotter.HypervisorConfiguration,

	stateName string,
	memoryName string,
) (
	peer *Peer[L, R, G],

	errs error,
) {
	peer = &Peer[L, R, G]{
		hypervisorCtx: hypervisorCtx,

		Wait: func() error {
			return nil
		},
		Close: func() error {
			return nil
		},
	}

	goroutineManager := manager.NewGoroutineManager(
		hypervisorCtx,
		&errs,
		manager.GoroutineManagerHooks{},
	)
	defer goroutineManager.Wait()
	defer goroutineManager.StopAllGoroutines()
	defer goroutineManager.CreateBackgroundPanicCollector()()

	var err error
	peer.runner, err = runner.StartRunner[L, R](
		hypervisorCtx,
		rescueCtx,

		hypervisorConfiguration,

		stateName,
		memoryName,
	)

	// We set both of these even if we return an error since we need to have a way to wait for rescue operations to complete
	peer.Wait = peer.runner.Wait
	peer.Close = func() error {
		if err := peer.runner.Close(); err != nil {
			return err
		}

		return peer.Wait()
	}

	if err != nil {
		panic(errors.Join(ErrCouldNotStartRunner, err))
	}

	peer.VMPath = peer.runner.VMPath
	peer.VMPid = peer.runner.VMPid

	// We don't track this because we return the wait function
	goroutineManager.StartBackgroundGoroutine(func(_ context.Context) {
		if err := peer.runner.Wait(); err != nil {
			panic(err)
		}
	})

	return
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

	// TODO: Pass these in
	// TODO: This schema tweak function should be exposed / passed in
	var log types.Logger
	var met metrics.SiloMetrics
	tweakRemote := func(index int, name string, schema *config.DeviceSchema) *config.DeviceSchema {

		for _, d := range devices {
			if d.Name == schema.Name {
				newSchema, err := createIncomingSiloDevSchema(&d, schema)
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

	migratedPeer = &MigratedPeer[L, R, G]{
		devices: devices,
		runner:  peer.runner,
		Wait: func() error {
			return nil
		},
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

	// Migrate the devices from a protocol
	if len(readers) > 0 && len(writers) > 0 {
		protocolCtx, cancelProtocolCtx := context.WithCancel(ctx)

		dg, err := migrateFromPipe(log, met, migratedPeer.runner.VMPath, protocolCtx, readers, writers, tweakRemote, hooks.OnXferCustomData)
		if err != nil {
			return nil, err
		}

		// Save dg for later usage, when we want to migrate from here etc
		migratedPeer.DgLock.Lock()
		migratedPeer.Dg = dg
		migratedPeer.DgLock.Unlock()

		migratedPeer.Wait = sync.OnceValue(func() error {
			defer cancelProtocolCtx()

			if dg != nil {
				err := dg.WaitForCompletion()
				if err != nil {
					return err
				}

				// Save dg for future migrations.
				migratedPeer.DgLock.Lock()
				migratedPeer.Dg = dg
				migratedPeer.DgLock.Unlock()
			}
			return nil
		})
	}

	//
	// IF all devices are local
	//

	if len(readers) == 0 && len(writers) == 0 {
		dg, err := migrateFromFS(log, met, migratedPeer.runner.VMPath, devices, tweakLocal)
		if err != nil {
			return nil, err
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
