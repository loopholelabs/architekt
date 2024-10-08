package peer

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loopholelabs/drafter/internal/utils"
	"github.com/loopholelabs/drafter/pkg/ipc"
	"github.com/loopholelabs/drafter/pkg/mounter"
	"github.com/loopholelabs/drafter/pkg/packager"
	"github.com/loopholelabs/drafter/pkg/registry"
	"github.com/loopholelabs/drafter/pkg/runner"
	"github.com/loopholelabs/goroutine-manager/pkg/manager"
	"github.com/loopholelabs/logging/types"
	"github.com/loopholelabs/silo/pkg/storage"
	"github.com/loopholelabs/silo/pkg/storage/migrator"
	"github.com/loopholelabs/silo/pkg/storage/protocol"
	"github.com/loopholelabs/silo/pkg/storage/protocol/packets"
)

type MigrateToHooks struct {
	OnBeforeGetDirtyBlocks func(deviceID uint32, remote bool)

	OnBeforeSuspend func()
	OnAfterSuspend  func()

	OnDeviceSent                       func(deviceID uint32, remote bool)
	OnDeviceAuthoritySent              func(deviceID uint32, remote bool)
	OnDeviceInitialMigrationProgress   func(deviceID uint32, remote bool, ready int, total int)
	OnDeviceContinousMigrationProgress func(deviceID uint32, remote bool, delta int)
	OnDeviceFinalMigrationProgress     func(deviceID uint32, remote bool, delta int)
	OnDeviceMigrationCompleted         func(deviceID uint32, remote bool)

	OnAllDevicesSent         func()
	OnAllMigrationsCompleted func()
}

type MigratablePeer[L ipc.AgentServerLocal, R ipc.AgentServerRemote[G], G any] struct {
	Close func()

	resumedPeer   *ResumedPeer[L, R, G]
	stage4Inputs  []makeMigratableDeviceStage
	resumedRunner *runner.ResumedRunner[L, R, G]
}

func (migratablePeer *MigratablePeer[L, R, G]) MigrateTo(
	logger types.Logger,
	ctx context.Context,

	devices []mounter.MigrateToDevice,

	suspendTimeout time.Duration,
	concurrency int,

	readers []io.Reader,
	writers []io.Writer,

	hooks MigrateToHooks,
) (errs error) {
	goroutineManager := manager.NewGoroutineManager(
		ctx,
		&errs,
		manager.GoroutineManagerHooks{},
	)
	defer goroutineManager.Wait()
	defer goroutineManager.StopAllGoroutines()
	defer goroutineManager.CreateBackgroundPanicCollector()()

	logger.Info().Msg("doing new protocol rw")
	pro := protocol.NewProtocolRW(
		goroutineManager.Context(),
		readers,
		writers,
		nil,
	)

	logger.Info().Msg("doing new protocol rw handler")
	goroutineManager.StartForegroundGoroutine(func(_ context.Context) {
		if err := pro.Handle(); err != nil && !errors.Is(err, io.EOF) {
			panic(errors.Join(registry.ErrCouldNotHandleProtocol, err))
		}
	})

	var (
		devicesLeftToSend                 atomic.Int32
		devicesLeftToTransferAuthorityFor atomic.Int32

		suspendedVMLock sync.Mutex
		suspendedVM     bool
	)

	suspendedVMCh := make(chan struct{})

	suspendAndMsyncVM := sync.OnceValue(func() error {
		if hook := hooks.OnBeforeSuspend; hook != nil {
			hook()
		}

		if err := migratablePeer.resumedPeer.SuspendAndCloseAgentServer(goroutineManager.Context(), suspendTimeout); err != nil {
			return errors.Join(ErrCouldNotSuspendAndCloseAgentServer, err)
		}

		if err := migratablePeer.resumedPeer.resumedRunner.Msync(goroutineManager.Context()); err != nil {
			return errors.Join(ErrCouldNotMsyncRunner, err)
		}

		if hook := hooks.OnAfterSuspend; hook != nil {
			hook()
		}

		suspendedVMLock.Lock()
		suspendedVM = true
		suspendedVMLock.Unlock()

		close(suspendedVMCh) // We can safely close() this channel since the caller only runs once/is `sync.OnceValue`d

		return nil
	})

	stage5Inputs := []migrateToStage{}
	for _, input := range migratablePeer.stage4Inputs {
		var migrateToDevice *mounter.MigrateToDevice
		for _, device := range devices {
			if device.Name == input.prev.prev.name {
				migrateToDevice = &device

				break
			}
		}

		// We don't want to serve this device
		if migrateToDevice == nil {
			continue
		}

		stage5Inputs = append(stage5Inputs, migrateToStage{
			prev: input,

			migrateToDevice: *migrateToDevice,
		})
	}

	logger.Info().Msgf("stage 5 inputs: %+v", stage5Inputs)
	_, deferFuncs, err := utils.ConcurrentMap(
		stage5Inputs,
		func(index int, input migrateToStage, _ *struct{}, _ func(deferFunc func() error)) error {
			to := protocol.NewToProtocol(input.prev.storage.Size(), uint32(index), pro)

			logger.Info().Int("index", index).Msgf("doing dev info")
			if err := to.SendDevInfo(input.prev.prev.prev.name, input.prev.prev.prev.blockSize, ""); err != nil {
				logger.Error().Int("index", index).Err(err).Msgf("dev info Errored out!")
				return errors.Join(mounter.ErrCouldNotSendDevInfo, err)
			}
			logger.Info().Int("index", index).Msgf("done dev info")

			if hook := hooks.OnDeviceSent; hook != nil {
				hook(uint32(index), input.prev.prev.prev.remote)
			}

			devicesLeftToSend.Add(1)
			if devicesLeftToSend.Load() >= int32(len(stage5Inputs)) {
				goroutineManager.StartForegroundGoroutine(func(_ context.Context) {
					logger.Info().Int("index", index).Msgf("doing device left to send")
					if err := to.SendEvent(&packets.Event{
						Type:       packets.EventCustom,
						CustomType: byte(registry.EventCustomAllDevicesSent),
					}); err != nil {
						panic(errors.Join(mounter.ErrCouldNotSendAllDevicesSentEvent, err))
					}

					if hook := hooks.OnAllDevicesSent; hook != nil {
						hook()
					}
				})
			}

			goroutineManager.StartForegroundGoroutine(func(_ context.Context) {
				logger.Info().Int("index", index).Msgf("doing handle need at")
				if err := to.HandleNeedAt(func(offset int64, length int32) {
					// Prioritize blocks
					endOffset := uint64(offset + int64(length))
					if endOffset > uint64(input.prev.storage.Size()) {
						endOffset = uint64(input.prev.storage.Size())
					}

					startBlock := int(offset / int64(input.prev.prev.prev.blockSize))
					endBlock := int((endOffset-1)/uint64(input.prev.prev.prev.blockSize)) + 1
					for b := startBlock; b < endBlock; b++ {
						input.prev.orderer.PrioritiseBlock(b)
					}
				}); err != nil {
					panic(errors.Join(registry.ErrCouldNotHandleNeedAt, err))
				}
			})

			goroutineManager.StartForegroundGoroutine(func(_ context.Context) {
				logger.Info().Int("index", index).Msgf("doing handle not needed")
				if err := to.HandleDontNeedAt(func(offset int64, length int32) {
					// Deprioritize blocks
					endOffset := uint64(offset + int64(length))
					if endOffset > uint64(input.prev.storage.Size()) {
						endOffset = uint64(input.prev.storage.Size())
					}

					startBlock := int(offset / int64(input.prev.storage.Size()))
					endBlock := int((endOffset-1)/uint64(input.prev.storage.Size())) + 1
					for b := startBlock; b < endBlock; b++ {
						input.prev.orderer.Remove(b)
					}
				}); err != nil {
					panic(errors.Join(registry.ErrCouldNotHandleDontNeedAt, err))
				}
			})

			cfg := migrator.NewMigratorConfig().WithBlockSize(int(input.prev.prev.prev.blockSize))
			cfg.Concurrency = map[int]int{
				storage.BlockTypeAny:      concurrency,
				storage.BlockTypeStandard: concurrency,
				storage.BlockTypeDirty:    concurrency,
				storage.BlockTypePriority: concurrency,
			}
			cfg.Locker_handler = func() {
				defer goroutineManager.CreateBackgroundPanicCollector()()

				logger.Info().Int("index", index).Msgf("doing pre lock")
				if err := to.SendEvent(&packets.Event{
					Type: packets.EventPreLock,
				}); err != nil {
					panic(errors.Join(mounter.ErrCouldNotSendPreLockEvent, err))
				}

				input.prev.storage.Lock()

				logger.Info().Int("index", index).Msgf("doing post lock")
				if err := to.SendEvent(&packets.Event{
					Type: packets.EventPostLock,
				}); err != nil {
					panic(errors.Join(mounter.ErrCouldNotSendPostLockEvent, err))
				}
			}
			cfg.Unlocker_handler = func() {
				defer goroutineManager.CreateBackgroundPanicCollector()()

				logger.Info().Int("index", index).Msgf("doing pre unlock")
				if err := to.SendEvent(&packets.Event{
					Type: packets.EventPreUnlock,
				}); err != nil {
					panic(errors.Join(mounter.ErrCouldNotSendPreUnlockEvent, err))
				}

				input.prev.storage.Unlock()

				logger.Info().Int("index", index).Msgf("doing post unlock")
				if err := to.SendEvent(&packets.Event{
					Type: packets.EventPostUnlock,
				}); err != nil {
					panic(errors.Join(mounter.ErrCouldNotSendPostUnlockEvent, err))
				}
			}
			cfg.Error_handler = func(b *storage.BlockInfo, err error) {
				defer goroutineManager.CreateBackgroundPanicCollector()()

				if err != nil {
					panic(errors.Join(registry.ErrCouldNotContinueWithMigration, err))
				}
			}
			cfg.Progress_handler = func(p *migrator.MigrationProgress) {
				if hook := hooks.OnDeviceInitialMigrationProgress; hook != nil {
					hook(uint32(index), input.prev.prev.prev.remote, p.Ready_blocks, p.Total_blocks)
				}
			}

			mig, err := migrator.NewMigrator(input.prev.dirtyRemote, to, input.prev.orderer, cfg)
			if err != nil {
				return errors.Join(registry.ErrCouldNotCreateMigrator, err)
			}

			logger.Info().Int("index", index).Msgf("starting .Migrate")
			if err := mig.Migrate(input.prev.totalBlocks); err != nil {
				return errors.Join(mounter.ErrCouldNotMigrateBlocks, err)
			}

			logger.Info().Int("index", index).Msgf("waiting for .Migrate to complete")
			if err := mig.WaitForCompletion(); err != nil {
				return errors.Join(registry.ErrCouldNotWaitForMigrationCompletion, err)
			}
			logger.Info().Int("index", index).Msgf("woo it completed")

			markDeviceAsReadyForAuthorityTransfer := sync.OnceFunc(func() {
				devicesLeftToTransferAuthorityFor.Add(1)
			})

			var (
				cyclesBelowDirtyBlockTreshold = 0
				totalCycles                   = 0
				ongoingMigrationsWg           sync.WaitGroup
			)
			for {
				logger.Info().Int("index", index).Msgf("loopedy loop time")

				suspendedVMLock.Lock()
				// We only need to `msync` for the memory because `msync` only affects the memory
				if !suspendedVM && input.prev.prev.prev.name == packager.MemoryName {
					logger.Info().Int("index", index).Msgf("doing msync")
					if err := migratablePeer.resumedRunner.Msync(goroutineManager.Context()); err != nil {
						suspendedVMLock.Unlock()

						return errors.Join(ErrCouldNotMsyncRunner, err)
					}
				}
				suspendedVMLock.Unlock()

				ongoingMigrationsWg.Wait()

				if hook := hooks.OnBeforeGetDirtyBlocks; hook != nil {
					hook(uint32(index), input.prev.prev.prev.remote)
				}

				blocks := mig.GetLatestDirty()
				if blocks == nil {
					mig.Unlock()

					suspendedVMLock.Lock()
					if suspendedVM {
						suspendedVMLock.Unlock()

						break
					}
					suspendedVMLock.Unlock()
				}

				if blocks != nil {
					if err := to.DirtyList(int(input.prev.prev.prev.blockSize), blocks); err != nil {
						return errors.Join(mounter.ErrCouldNotSendDirtyList, err)
					}

					ongoingMigrationsWg.Add(1)
					goroutineManager.StartForegroundGoroutine(func(_ context.Context) {
						defer ongoingMigrationsWg.Done()

						logger.Info().Int("index", index).Msgf("doing migrate dirty")
						if err := mig.MigrateDirty(blocks); err != nil {
							panic(errors.Join(mounter.ErrCouldNotMigrateDirtyBlocks, err))
						}

						suspendedVMLock.Lock()
						defer suspendedVMLock.Unlock()

						if suspendedVM {
							if hook := hooks.OnDeviceFinalMigrationProgress; hook != nil {
								hook(uint32(index), input.prev.prev.prev.remote, len(blocks))
							}
						} else {
							if hook := hooks.OnDeviceContinousMigrationProgress; hook != nil {
								hook(uint32(index), input.prev.prev.prev.remote, len(blocks))
							}
						}
					})
				}

				suspendedVMLock.Lock()
				if !suspendedVM && !(devicesLeftToTransferAuthorityFor.Load() >= int32(len(stage5Inputs))) {
					suspendedVMLock.Unlock()

					// We use the background context here instead of the internal context because we want to distinguish
					// between a context cancellation from the outside and getting a response
					cycleThrottleCtx, cancelCycleThrottleCtx := context.WithTimeout(context.Background(), input.migrateToDevice.CycleThrottle)
					defer cancelCycleThrottleCtx()

					select {
					case <-cycleThrottleCtx.Done():
						break

					case <-suspendedVMCh:
						break

					case <-goroutineManager.Context().Done(): // ctx is the goroutineManager.goroutineManager.Context() here
						if err := goroutineManager.Context().Err(); err != nil {
							return errors.Join(ErrPeerContextCancelled, err)
						}

						return nil
					}
				} else {
					suspendedVMLock.Unlock()
				}

				totalCycles++
				if len(blocks) < input.migrateToDevice.MaxDirtyBlocks {
					cyclesBelowDirtyBlockTreshold++
					if cyclesBelowDirtyBlockTreshold > input.migrateToDevice.MinCycles {
						markDeviceAsReadyForAuthorityTransfer()
					}
				} else if totalCycles > input.migrateToDevice.MaxCycles {
					markDeviceAsReadyForAuthorityTransfer()
				} else {
					cyclesBelowDirtyBlockTreshold = 0
				}

				if devicesLeftToTransferAuthorityFor.Load() >= int32(len(stage5Inputs)) {
					if err := suspendAndMsyncVM(); err != nil {
						return errors.Join(mounter.ErrCouldNotSuspendAndMsyncVM, err)
					}
				}
			}

			if err := to.SendEvent(&packets.Event{
				Type:       packets.EventCustom,
				CustomType: byte(registry.EventCustomTransferAuthority),
			}); err != nil {
				panic(errors.Join(mounter.ErrCouldNotSendTransferAuthorityEvent, err))
			}

			if hook := hooks.OnDeviceAuthoritySent; hook != nil {
				hook(uint32(index), input.prev.prev.prev.remote)
			}

			if err := mig.WaitForCompletion(); err != nil {
				return errors.Join(registry.ErrCouldNotWaitForMigrationCompletion, err)
			}

			if err := to.SendEvent(&packets.Event{
				Type: packets.EventCompleted,
			}); err != nil {
				return errors.Join(mounter.ErrCouldNotSendCompletedEvent, err)
			}

			if hook := hooks.OnDeviceMigrationCompleted; hook != nil {
				hook(uint32(index), input.prev.prev.prev.remote)
			}

			return nil
		},
	)

	if err != nil {
		panic(errors.Join(mounter.ErrCouldNotMigrateToDevice, err))
	}

	for _, deferFuncs := range deferFuncs {
		for _, deferFunc := range deferFuncs {
			defer deferFunc() // We can safely ignore errors here since we never call `addDefer` with a function that could return an error
		}
	}

	if hook := hooks.OnAllMigrationsCompleted; hook != nil {
		hook()
	}

	return
}
