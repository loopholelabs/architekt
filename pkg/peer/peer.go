package peer

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/loopholelabs/drafter/pkg/common"
	"github.com/loopholelabs/drafter/pkg/runtimes"
	"github.com/loopholelabs/logging/types"
	"github.com/loopholelabs/silo/pkg/storage/config"
	"github.com/loopholelabs/silo/pkg/storage/devicegroup"
	"github.com/loopholelabs/silo/pkg/storage/metrics"
	"github.com/loopholelabs/silo/pkg/storage/migrator"
)

type Peer struct {
	log types.Logger
	met metrics.SiloMetrics

	// Devices
	dgLock  sync.Mutex
	dg      *devicegroup.DeviceGroup
	devices []common.MigrateFromDevice

	// Runtime
	runtimeProvider runtimes.RuntimeProviderIfc
	VMPath          string
	VMPid           int

	// Errors
	backgroundErr chan error
}

// Callbacks for MigrateTO
type MigrateToHooks struct {
	OnBeforeSuspend          func()
	OnAfterSuspend           func()
	OnAllMigrationsCompleted func()
	OnProgress               func(p map[string]*migrator.MigrationProgress)
	GetXferCustomData        func() []byte
}

// Callbacks for MigrateFrom
type MigrateFromHooks struct {
	OnLocalDeviceRequested func(localDeviceID uint32, name string)
	OnLocalDeviceExposed   func(localDeviceID uint32, path string)
	OnCompletion           func()

	OnLocalAllDevicesRequested func()

	OnXferCustomData func([]byte)
}

func (peer *Peer) Close() error {
	if peer.log != nil {
		peer.log.Debug().Msg("Peer.Close")
	}

	// Try to close the runtimeProvider first
	errRuntime := peer.runtimeProvider.Close()
	if errRuntime != nil {
		peer.log.Error().Err(errRuntime).Msg("Error closing runtime")
	}

	if peer.log != nil {
		peer.log.Debug().Msg("Closing dg")
	}
	errDG := peer.closeDG()
	// Return
	return errors.Join(errRuntime, errDG)
}

func (peer *Peer) BackgroundErr() chan error {
	return peer.backgroundErr
}

func (peer *Peer) setDG(dg *devicegroup.DeviceGroup) {
	peer.dgLock.Lock()
	peer.dg = dg
	peer.dgLock.Unlock()
}

func (peer *Peer) closeDG() error {
	var err error
	peer.dgLock.Lock()
	if peer.dg != nil {
		err = peer.dg.CloseAll()
		peer.dg = nil
	}
	peer.dgLock.Unlock()
	return err
}

func StartPeer(ctx context.Context, rescueCtx context.Context,
	log types.Logger, met metrics.SiloMetrics, rp runtimes.RuntimeProviderIfc) (*Peer, error) {

	peer := &Peer{
		log:             log,
		met:             met,
		runtimeProvider: rp,
		backgroundErr:   make(chan error, 12),
	}

	err := peer.runtimeProvider.Start(ctx, rescueCtx, peer.backgroundErr)

	if err != nil {
		if log != nil {
			log.Warn().Err(err).Msg("error starting runtime")
		}
		return nil, err
	}

	peer.VMPath = peer.runtimeProvider.DevicePath()
	peer.VMPid = peer.runtimeProvider.GetVMPid()

	if log != nil {
		log.Info().Str("vmpath", peer.VMPath).Int("vmpid", peer.VMPid).Msg("started peer runtime")
	}
	return peer, nil
}

func (peer *Peer) MigrateFrom(ctx context.Context, devices []common.MigrateFromDevice,
	readers []io.Reader, writers []io.Writer, hooks MigrateFromHooks) error {

	if peer.log != nil {
		peer.log.Info().Msg("started MigrateFrom")
	}

	tweakRemote := func(index int, name string, schema *config.DeviceSchema) *config.DeviceSchema {

		for _, d := range devices {
			if d.Name == schema.Name {
				newSchema, err := common.CreateIncomingSiloDevSchema(&d, schema)
				if err == nil {
					if peer.log != nil {
						peer.log.Debug().Str("schema", string(newSchema.EncodeAsBlock())).Msg("incoming schema")
					}
					return newSchema
				}
			}
		}

		// FIXME: Error. We didn't find the local device, or couldn't set it up.
		if peer.log != nil {
			peer.log.Error().Str("name", name).Msg("unknown device name")
			// We should probably relay an error here...
		}

		return schema
	}

	tweakLocal := func(index int, name string, schema *config.DeviceSchema) *config.DeviceSchema {
		return schema
	}

	peer.devices = devices

	// Migrate the devices from a protocol
	if len(readers) > 0 && len(writers) > 0 {
		protocolCtx, cancelProtocolCtx := context.WithCancel(ctx)

		var slog types.Logger
		if peer.log != nil {
			slog = peer.log.SubLogger("silo")
		}
		dg, err := common.MigrateFromPipe(slog, peer.met, peer.runtimeProvider.DevicePath(), protocolCtx, readers, writers, tweakRemote, hooks.OnXferCustomData)
		if err != nil {
			if peer.log != nil {
				peer.log.Warn().Err(err).Msg("error migrating from pipe")
			}
			return err
		}

		// Save dg for future migrations, AND for things like reading config
		peer.setDG(dg)

		if peer.log != nil {
			peer.log.Info().Msg("migrated from pipe successfully")
		}

		// Monitor the transfer, and report any error if it happens
		go func() {
			defer cancelProtocolCtx()

			if peer.log != nil {
				peer.log.Trace().Msg("waiting for device migrations to complete")
			}
			err := dg.WaitForCompletion()
			if peer.log != nil {
				if err != nil {
					peer.log.Error().Err(err).Msg("device migrations completed")
				} else {
					peer.log.Debug().Msg("device migrations completed")
				}
			}
			if err == nil {
				err = dg.WaitForReady()
				if peer.log != nil {
					if err != nil {
						peer.log.Error().Err(err).Msg("device migrations completed and ready")
					} else {
						peer.log.Debug().Msg("device migrations completed and ready")
					}
				}

				if err == nil {
					if hooks.OnCompletion != nil {
						hooks.OnCompletion()
					}
				} else {
					select {
					case peer.backgroundErr <- err:
					default:
					}
				}
			} else {
				select {
				case peer.backgroundErr <- err:
				default:
				}
			}
		}()
	}

	//
	// IF all devices are local
	//

	if len(readers) == 0 && len(writers) == 0 {
		var slog types.Logger
		if peer.log != nil {
			slog = peer.log.SubLogger("silo")
		}

		dg, err := common.MigrateFromFS(slog, peer.met, peer.runtimeProvider.DevicePath(), devices, tweakLocal)
		if err != nil {
			if peer.log != nil {
				peer.log.Warn().Err(err).Msg("error migrating from fs")
			}
			return err
		}

		// Save dg for later usage, when we want to migrate from here etc
		peer.setDG(dg)

		if peer.log != nil {
			peer.log.Info().Msg("migrated from fs successfully")
		}

		if hook := hooks.OnLocalAllDevicesRequested; hook != nil {
			hook()
		}

		if hooks.OnCompletion != nil {
			go hooks.OnCompletion()
		}
	}

	return nil
}

func (peer *Peer) Resume(ctx context.Context, resumeTimeout time.Duration, rescueTimeout time.Duration) error {

	if peer.log != nil {
		peer.log.Trace().Msg("resuming runtime")
	}

	err := peer.runtimeProvider.Resume(resumeTimeout, rescueTimeout, peer.backgroundErr)

	if err != nil {
		if peer.log != nil {
			peer.log.Warn().Err(err).Msg("could not resume runtime")
		}
		return err
	}

	if peer.log != nil {
		peer.log.Info().Msg("resumed runtime")
	}

	return nil
}

/**
 * MigrateTo migrates to a remote VM.
 *
 *
 */
func (peer *Peer) MigrateTo(ctx context.Context, devices []common.MigrateToDevice,
	suspendTimeout time.Duration, concurrency int, readers []io.Reader, writers []io.Writer,
	hooks MigrateToHooks) error {

	if peer.log != nil {
		peer.log.Info().Msg("peer.MigrateTo")
	}

	// This manages the status of the VM - if it's suspended or not.
	vmState := common.NewVMStateMgr(ctx,
		peer.runtimeProvider.Suspend,
		suspendTimeout,
		peer.runtimeProvider.FlushData,
		hooks.OnBeforeSuspend,
		hooks.OnAfterSuspend,
	)

	err := common.MigrateToPipe(ctx, readers, writers, peer.dg, concurrency, hooks.OnProgress, vmState, devices, hooks.GetXferCustomData)
	if err != nil {
		if peer.log != nil {
			peer.log.Info().Err(err).Msg("error in peer.MigrateTo")
		}
		return err
	}

	if peer.log != nil {
		peer.log.Info().Msg("peer.MigrateTo completed successfuly")
	}

	return nil
}
