package roles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	iutils "github.com/loopholelabs/drafter/internal/utils"
	"github.com/loopholelabs/drafter/pkg/utils"
	"github.com/loopholelabs/silo/pkg/storage"
	"github.com/loopholelabs/silo/pkg/storage/blocks"
	sconfig "github.com/loopholelabs/silo/pkg/storage/config"
	sdevice "github.com/loopholelabs/silo/pkg/storage/device"
	"github.com/loopholelabs/silo/pkg/storage/dirtytracker"
	"github.com/loopholelabs/silo/pkg/storage/migrator"
	"github.com/loopholelabs/silo/pkg/storage/modules"
	"github.com/loopholelabs/silo/pkg/storage/protocol"
	"github.com/loopholelabs/silo/pkg/storage/protocol/packets"
	"github.com/loopholelabs/silo/pkg/storage/volatilitymonitor"
	"github.com/loopholelabs/silo/pkg/storage/waitingcache"
	"golang.org/x/sys/unix"
)

var (
	ErrConfigFileNotFound                 = errors.New("config file not found")
	ErrCouldNotGetNBDDeviceStat           = errors.New("could not get NBD device stat")
	ErrCouldNotStartRunner                = errors.New("could not start runner")
	ErrCouldNotCreateDeviceDirectory      = errors.New("could not create device directory")
	ErrCouldNotCreateDevice               = errors.New("could not create device")
	ErrCouldNotRequestBlock               = errors.New("could not request block")
	ErrCouldNotReleaseBlock               = errors.New("could not release block")
	ErrPeerContextCancelled               = errors.New("peer context cancelled")
	ErrCouldNotCreateDeviceNode           = errors.New("could not create device node")
	ErrCouldNotHandleReadAt               = errors.New("could not handle ReadAt")
	ErrCouldNotHandleWriteAt              = errors.New("could not handle WriteAt")
	ErrCouldNotHandleDevInfo              = errors.New("could not handle device info")
	ErrCouldNotHandleEvent                = errors.New("could not handle event")
	ErrCouldNotHandleDirtyList            = errors.New("could not handle dirty list")
	ErrCouldNotWaitForMigrationCompletion = errors.New("could not wait for migration completion")
	ErrCouldNotCloseMigratedPeer          = errors.New("could not close migrated peer")
	ErrCouldNotGetBaseDeviceStat          = errors.New("could not get base device statistics")
	ErrCouldNotCreateOverlayDirectory     = errors.New("could not create overlay directory")
	ErrCouldNotCreateStateDirectory       = errors.New("could not create state directory")
	ErrCouldNotCreateLocalDevice          = errors.New("could not create local device")
	ErrCouldNotSetupDevices               = errors.New("could not setup devices")
	ErrCouldNotOpenConfigFile             = errors.New("could not open config file")
	ErrCouldNotDecodeConfigFile           = errors.New("could not decode config file")
	ErrCouldNotResumeRunner               = errors.New("could not resume runner")
	ErrCouldNotCreateMigratablePeer       = errors.New("could not create migratable peer")
	ErrCouldNotHandleProtocol             = errors.New("could not handle protocol")
	ErrCouldNotSuspendAndCloseAgentServer = errors.New("could not suspend and close agent server")
	ErrCouldNotMsyncRunner                = errors.New("could not msync runner")
	ErrCouldNotSendDevInfo                = errors.New("could not send device info")
	ErrCouldNotSendAllDevicesSentEvent    = errors.New("could not send all devices sent event")
	ErrCouldNotHandleNeedAt               = errors.New("could not handle NeedAt")
	ErrCouldNotHandleDontNeedAt           = errors.New("could not handle DontNeedAt")
	ErrCouldNotSendPreLockEvent           = errors.New("could not send pre-lock event")
	ErrCouldNotSendPostLockEvent          = errors.New("could not send post-lock event")
	ErrCouldNotSendPreUnlockEvent         = errors.New("could not send pre-unlock event")
	ErrCouldNotSendPostUnlockEvent        = errors.New("could not send post-unlock event")
	ErrCouldNotCreateMigrator             = errors.New("could not create migrator")
	ErrCouldNotMigrateBlocks              = errors.New("could not migrate blocks")
	ErrCouldNotSendDirtyList              = errors.New("could not send dirty list")
	ErrCouldNotMigrateDirtyBlocks         = errors.New("could not migrate dirty blocks")
	ErrCouldNotSuspendAndMsyncVM          = errors.New("could not suspend and msync VM")
	ErrCouldNotSendTransferAuthorityEvent = errors.New("could not send transfer authority event")
	ErrCouldNotSendCompletedEvent         = errors.New("could not send completed event")
	ErrCouldNotMigrateToDevice            = errors.New("could not migrate to device")
)

type MigrateFromDevice struct {
	Name string `json:"name"`

	Base    string `json:"base"`
	Overlay string `json:"overlay"`
	State   string `json:"state"`

	BlockSize uint32 `json:"blockSize"`
}

type MigrateFromHooks struct {
	OnRemoteDeviceReceived           func(remoteDeviceID uint32, name string)
	OnRemoteDeviceExposed            func(remoteDeviceID uint32, path string)
	OnRemoteDeviceAuthorityReceived  func(remoteDeviceID uint32)
	OnRemoteDeviceMigrationCompleted func(remoteDeviceID uint32)

	OnRemoteAllDevicesReceived     func()
	OnRemoteAllMigrationsCompleted func()

	OnLocalDeviceRequested func(localDeviceID uint32, name string)
	OnLocalDeviceExposed   func(localDeviceID uint32, path string)

	OnLocalAllDevicesRequested func()
}

type MigratedPeer struct {
	Wait  func() error
	Close func() error

	Resume func(
		ctx context.Context,

		resumeTimeout,
		rescueTimeout time.Duration,
	) (
		resumedPeer *ResumedPeer,

		errs error,
	)
}

type MakeMigratableDevice struct {
	Name string `json:"name"`

	Expiry time.Duration `json:"expiry"`
}

type ResumedPeer struct {
	Wait  func() error
	Close func() error

	SuspendAndCloseAgentServer func(ctx context.Context, resumeTimeout time.Duration) error

	MakeMigratable func(
		ctx context.Context,

		devices []MakeMigratableDevice,
	) (migratablePeer *MigratablePeer, errs error)
}

type MigrateToDevice struct {
	Name string `json:"name"`

	MaxDirtyBlocks int `json:"maxDirtyBlocks"`
	MinCycles      int `json:"minCycles"`
	MaxCycles      int `json:"maxCycles"`

	CycleThrottle time.Duration `json:"cycleThrottle"`
}

type MigrateToHooks struct {
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

type MigratablePeer struct {
	Close func()

	MigrateTo func(
		ctx context.Context,

		devices []MigrateToDevice,

		suspendTimeout time.Duration,
		concurrency int,

		readers []io.Reader,
		writers []io.Writer,

		hooks MigrateToHooks,
	) (errs error)
}

type peerStage2 struct {
	name string

	blockSize uint32

	id     uint32
	remote bool

	storage storage.StorageProvider
	device  storage.ExposedStorage
}

type peerStage3 struct {
	prev peerStage2

	makeMigratableDevice MakeMigratableDevice
}

type peerStage4 struct {
	prev peerStage3

	storage     *modules.Lockable
	orderer     *blocks.PriorityBlockOrder
	totalBlocks int
	dirtyRemote *dirtytracker.DirtyTrackerRemote
}

type peerStage5 struct {
	prev peerStage4

	migrateToDevice MigrateToDevice
}

type Peer struct {
	VMPath string

	Wait  func() error
	Close func() error

	MigrateFrom func(
		ctx context.Context,

		devices []MigrateFromDevice,

		readers []io.Reader,
		writers []io.Writer,

		hooks MigrateFromHooks,
	) (
		migratedPeer *MigratedPeer,

		errs error,
	)
}

func StartPeer(
	hypervisorCtx context.Context,
	rescueCtx context.Context,

	hypervisorConfiguration HypervisorConfiguration,

	stateName string,
	memoryName string,
) (
	peer *Peer,

	errs error,
) {
	peer = &Peer{}

	_, handlePanics, handleGoroutinePanics, cancel, wait, _ := utils.GetPanicHandler(
		hypervisorCtx,
		&errs,
		utils.GetPanicHandlerHooks{},
	)
	defer wait()
	defer cancel()
	defer handlePanics(false)()

	runner, err := StartRunner(
		hypervisorCtx,
		rescueCtx,

		hypervisorConfiguration,

		stateName,
		memoryName,
	)

	// We set both of these even if we return an error since we need to have a way to wait for rescue operations to complete
	peer.Wait = runner.Wait
	peer.Close = func() error {
		if runner.Close != nil {
			if err := runner.Close(); err != nil {
				return err
			}
		}

		if peer.Wait != nil {
			if err := peer.Wait(); err != nil {
				return err
			}
		}

		return nil
	}

	if err != nil {
		panic(errors.Join(ErrCouldNotStartRunner, err))
	}

	peer.VMPath = runner.VMPath

	// We don't track this because we return the wait function
	handleGoroutinePanics(false, func() {
		if err := runner.Wait(); err != nil {
			panic(err)
		}
	})

	peer.MigrateFrom = func(
		ctx context.Context,

		devices []MigrateFromDevice,

		readers []io.Reader,
		writers []io.Writer,

		hooks MigrateFromHooks,
	) (
		migratedPeer *MigratedPeer,

		errs error,
	) {
		migratedPeer = &MigratedPeer{}

		// We use the background context here instead of the internal context because we want to distinguish
		// between a context cancellation from the outside and getting a response
		allRemoteDevicesReceivedCtx, cancelAllRemoteDevicesReceivedCtx := context.WithCancel(context.Background())
		defer cancelAllRemoteDevicesReceivedCtx()

		allRemoteDevicesReadyCtx, cancelAllRemoteDevicesReadyCtx := context.WithCancel(context.Background())
		defer cancelAllRemoteDevicesReadyCtx()

		// We don't `defer cancelProtocolCtx()` this because we cancel in the wait function
		protocolCtx, cancelProtocolCtx := context.WithCancel(ctx)

		// We overwrite this further down, but this is so that we don't leak the `protocolCtx` if we `panic()` before we set `WaitForMigrationsToComplete`
		migratedPeer.Wait = func() error {
			cancelProtocolCtx()

			return nil
		}

		internalCtx, handlePanics, handleGoroutinePanics, cancel, wait, _ := utils.GetPanicHandler(
			ctx,
			&errs,
			utils.GetPanicHandlerHooks{},
		)
		defer wait()
		defer cancel()
		defer handlePanics(false)()

		// Use an atomic counter and `allDevicesReadyCtx` and instead of a WaitGroup so that we can `select {}` without leaking a goroutine
		var (
			receivedButNotReadyRemoteDevices atomic.Int32

			deviceCloseFuncsLock sync.Mutex
			deviceCloseFuncs     []func() error

			stage2InputsLock sync.Mutex
			stage2Inputs     = []peerStage2{}

			pro *protocol.ProtocolRW
		)
		if len(readers) > 0 && len(writers) > 0 { // Only open the protocol if we want passed in readers and writers
			pro = protocol.NewProtocolRW(
				protocolCtx, // We don't track this because we return the wait function
				readers,
				writers,
				func(ctx context.Context, p protocol.Protocol, index uint32) {
					var (
						from  *protocol.FromProtocol
						local *waitingcache.WaitingCacheLocal
					)
					from = protocol.NewFromProtocol(
						ctx,
						index,
						func(di *packets.DevInfo) storage.StorageProvider {
							// No need to `defer handlePanics` here - panics bubble upwards

							base := ""
							for _, device := range devices {
								if di.Name == device.Name {
									base = device.Base

									break
								}
							}

							if strings.TrimSpace(base) == "" {
								panic(ErrUnknownDeviceName)
							}

							receivedButNotReadyRemoteDevices.Add(1)

							if hook := hooks.OnRemoteDeviceReceived; hook != nil {
								hook(index, di.Name)
							}

							if err := os.MkdirAll(filepath.Dir(base), os.ModePerm); err != nil {
								panic(errors.Join(ErrCouldNotCreateDeviceDirectory, err))
							}

							src, device, err := sdevice.NewDevice(&sconfig.DeviceSchema{
								Name:      di.Name,
								System:    "file",
								Location:  base,
								Size:      fmt.Sprintf("%v", di.Size),
								BlockSize: fmt.Sprintf("%v", di.Block_size),
								Expose:    true,
							})
							if err != nil {
								panic(errors.Join(ErrCouldNotCreateDevice, err))
							}
							deviceCloseFuncsLock.Lock()
							deviceCloseFuncs = append(deviceCloseFuncs, device.Shutdown) // defer device.Shutdown()
							// We have to close the runner before we close the devices
							deviceCloseFuncs = append(deviceCloseFuncs, runner.Close) // defer runner.Close()
							deviceCloseFuncsLock.Unlock()

							var remote *waitingcache.WaitingCacheRemote
							local, remote = waitingcache.NewWaitingCache(src, int(di.Block_size))
							local.NeedAt = func(offset int64, length int32) {
								// Only access the `from` protocol if it's not already closed
								select {
								case <-protocolCtx.Done():
									return

								default:
								}

								if err := from.NeedAt(offset, length); err != nil {
									panic(errors.Join(ErrCouldNotRequestBlock, err))
								}
							}
							local.DontNeedAt = func(offset int64, length int32) {
								// Only access the `from` protocol if it's not already closed
								select {
								case <-protocolCtx.Done():
									return

								default:
								}

								if err := from.DontNeedAt(offset, length); err != nil {
									panic(errors.Join(ErrCouldNotReleaseBlock, err))
								}
							}

							device.SetProvider(local)

							stage2InputsLock.Lock()
							stage2Inputs = append(stage2Inputs, peerStage2{
								name: di.Name,

								blockSize: di.Block_size,

								id:     index,
								remote: true,

								storage: local,
								device:  device,
							})
							stage2InputsLock.Unlock()

							devicePath := filepath.Join("/dev", device.Device())

							deviceInfo, err := os.Stat(devicePath)
							if err != nil {
								panic(errors.Join(ErrCouldNotGetDeviceStat, err))
							}

							deviceStat, ok := deviceInfo.Sys().(*syscall.Stat_t)
							if !ok {
								panic(ErrCouldNotGetNBDDeviceStat)
							}

							deviceMajor := uint64(deviceStat.Rdev / 256)
							deviceMinor := uint64(deviceStat.Rdev % 256)

							deviceID := int((deviceMajor << 8) | deviceMinor)

							select {
							case <-internalCtx.Done():
								if err := internalCtx.Err(); err != nil {
									panic(errors.Join(ErrPeerContextCancelled, err))
								}

								return nil

							default:
								if err := unix.Mknod(filepath.Join(runner.VMPath, di.Name), unix.S_IFBLK|0666, deviceID); err != nil {
									panic(errors.Join(ErrCouldNotCreateDeviceNode, err))
								}
							}

							if hook := hooks.OnRemoteDeviceExposed; hook != nil {
								hook(index, devicePath)
							}

							return remote
						},
						p,
					)

					handleGoroutinePanics(true, func() {
						if err := from.HandleReadAt(); err != nil {
							panic(errors.Join(ErrCouldNotHandleReadAt, err))
						}
					})

					handleGoroutinePanics(true, func() {
						if err := from.HandleWriteAt(); err != nil {
							panic(errors.Join(ErrCouldNotHandleWriteAt, err))
						}
					})

					handleGoroutinePanics(true, func() {
						if err := from.HandleDevInfo(); err != nil {
							panic(errors.Join(ErrCouldNotHandleDevInfo, err))
						}
					})

					handleGoroutinePanics(true, func() {
						if err := from.HandleEvent(func(e *packets.Event) {
							switch e.Type {
							case packets.EventCustom:
								switch e.CustomType {
								case byte(EventCustomAllDevicesSent):
									cancelAllRemoteDevicesReceivedCtx()

									if hook := hooks.OnRemoteAllDevicesReceived; hook != nil {
										hook()
									}

								case byte(EventCustomTransferAuthority):
									if receivedButNotReadyRemoteDevices.Add(-1) <= 0 {
										cancelAllRemoteDevicesReadyCtx()
									}

									if hook := hooks.OnRemoteDeviceAuthorityReceived; hook != nil {
										hook(index)
									}
								}

							case packets.EventCompleted:
								if hook := hooks.OnRemoteDeviceMigrationCompleted; hook != nil {
									hook(index)
								}
							}
						}); err != nil {
							panic(errors.Join(ErrCouldNotHandleEvent, err))
						}
					})

					handleGoroutinePanics(true, func() {
						if err := from.HandleDirtyList(func(blocks []uint) {
							if local != nil {
								local.DirtyBlocks(blocks)
							}
						}); err != nil {
							panic(errors.Join(ErrCouldNotHandleDirtyList, err))
						}
					})
				})
		}

		migratedPeer.Wait = sync.OnceValue(func() error {
			defer cancelProtocolCtx()

			// If we haven't opened the protocol, don't wait for it
			if pro != nil {
				if err := pro.Handle(); err != nil && !errors.Is(err, io.EOF) {
					return err
				}
			}

			// If it hasn't sent any devices, the remote Silo peer doesn't send `EventCustomAllDevicesSent`
			// After the protocol has closed without errors, we can safely assume that we won't receive any
			// additional devices, so we mark all devices as received and ready
			select {
			case <-allRemoteDevicesReceivedCtx.Done():
			default:
				cancelAllRemoteDevicesReceivedCtx()

				// We need to call the hook manually too since we would otherwise only call if we received at least one device
				if hook := hooks.OnRemoteAllDevicesReceived; hook != nil {
					hook()
				}
			}

			cancelAllRemoteDevicesReadyCtx()

			if hook := hooks.OnRemoteAllMigrationsCompleted; hook != nil {
				hook()
			}

			return nil
		})
		migratedPeer.Close = func() (errs error) {
			// We have to close the runner before we close the devices
			if err := runner.Close(); err != nil {
				errs = errors.Join(errs, err)
			}

			defer func() {
				if err := migratedPeer.Wait(); err != nil {
					errs = errors.Join(errs, err)
				}
			}()

			deviceCloseFuncsLock.Lock()
			defer deviceCloseFuncsLock.Unlock()

			for _, closeFunc := range deviceCloseFuncs {
				defer func(closeFunc func() error) {
					if err := closeFunc(); err != nil {
						errs = errors.Join(errs, err)
					}
				}(closeFunc)
			}

			return
		}

		// We don't track this because we return the wait function
		handleGoroutinePanics(false, func() {
			if err := migratedPeer.Wait(); err != nil {
				panic(errors.Join(ErrCouldNotWaitForMigrationCompletion, err))
			}
		})

		// We don't track this because we return the close function
		handleGoroutinePanics(false, func() {
			select {
			// Failure case; we cancelled the internal context before all devices are ready
			case <-internalCtx.Done():
				if err := migratedPeer.Close(); err != nil {
					panic(errors.Join(ErrCouldNotCloseMigratedPeer, err))
				}

			// Happy case; all devices are ready and we want to wait with closing the devices until we stop the Firecracker process
			case <-allRemoteDevicesReadyCtx.Done():
				<-hypervisorCtx.Done()

				if err := migratedPeer.Close(); err != nil {
					panic(errors.Join(ErrCouldNotCloseMigratedPeer, err))
				}

				break
			}
		})

		select {
		case <-internalCtx.Done():
			if err := internalCtx.Err(); err != nil {
				panic(errors.Join(ErrPeerContextCancelled, err))
			}

			return
		case <-allRemoteDevicesReceivedCtx.Done():
			break
		}

		stage1Inputs := []MigrateFromDevice{}
		for _, input := range devices {
			if slices.ContainsFunc(
				stage2Inputs,
				func(r peerStage2) bool {
					return input.Name == r.name
				},
			) {
				continue
			}

			stage1Inputs = append(stage1Inputs, input)
		}

		// Use an atomic counter instead of a WaitGroup so that we can wait without leaking a goroutine
		var remainingRequestedLocalDevices atomic.Int32
		remainingRequestedLocalDevices.Add(int32(len(stage1Inputs)))

		_, deferFuncs, err := iutils.ConcurrentMap(
			stage1Inputs,
			func(index int, input MigrateFromDevice, _ *struct{}, addDefer func(deferFunc func() error)) error {
				if hook := hooks.OnLocalDeviceRequested; hook != nil {
					hook(uint32(index), input.Name)
				}

				if remainingRequestedLocalDevices.Add(-1) <= 0 {
					if hook := hooks.OnLocalAllDevicesRequested; hook != nil {
						hook()
					}
				}

				stat, err := os.Stat(input.Base)
				if err != nil {
					return errors.Join(ErrCouldNotGetBaseDeviceStat, err)
				}

				var (
					local  storage.StorageProvider
					device storage.ExposedStorage
				)
				if strings.TrimSpace(input.Overlay) == "" || strings.TrimSpace(input.State) == "" {
					local, device, err = sdevice.NewDevice(&sconfig.DeviceSchema{
						Name:      input.Name,
						System:    "file",
						Location:  input.Base,
						Size:      fmt.Sprintf("%v", stat.Size()),
						BlockSize: fmt.Sprintf("%v", input.BlockSize),
						Expose:    true,
					})
				} else {
					if err := os.MkdirAll(filepath.Dir(input.Overlay), os.ModePerm); err != nil {
						return errors.Join(ErrCouldNotCreateOverlayDirectory, err)
					}

					if err := os.MkdirAll(filepath.Dir(input.State), os.ModePerm); err != nil {
						return errors.Join(ErrCouldNotCreateStateDirectory, err)
					}

					local, device, err = sdevice.NewDevice(&sconfig.DeviceSchema{
						Name:      input.Name,
						System:    "sparsefile",
						Location:  input.Overlay,
						Size:      fmt.Sprintf("%v", stat.Size()),
						BlockSize: fmt.Sprintf("%v", input.BlockSize),
						Expose:    true,
						ROSource: &sconfig.DeviceSchema{
							Name:     input.State,
							System:   "file",
							Location: input.Base,
							Size:     fmt.Sprintf("%v", stat.Size()),
						},
					})
				}
				if err != nil {
					return errors.Join(ErrCouldNotCreateLocalDevice, err)
				}
				addDefer(local.Close)
				addDefer(device.Shutdown)

				device.SetProvider(local)

				stage2InputsLock.Lock()
				stage2Inputs = append(stage2Inputs, peerStage2{
					name: input.Name,

					blockSize: input.BlockSize,

					id:     uint32(index),
					remote: false,

					storage: local,
					device:  device,
				})
				stage2InputsLock.Unlock()

				devicePath := filepath.Join("/dev", device.Device())

				deviceInfo, err := os.Stat(devicePath)
				if err != nil {
					return errors.Join(ErrCouldNotGetDeviceStat, err)
				}

				deviceStat, ok := deviceInfo.Sys().(*syscall.Stat_t)
				if !ok {
					return ErrCouldNotGetNBDDeviceStat
				}

				deviceMajor := uint64(deviceStat.Rdev / 256)
				deviceMinor := uint64(deviceStat.Rdev % 256)

				deviceID := int((deviceMajor << 8) | deviceMinor)

				select {
				case <-internalCtx.Done():
					if err := internalCtx.Err(); err != nil {
						return errors.Join(ErrPeerContextCancelled, err)
					}

					return nil

				default:
					if err := unix.Mknod(filepath.Join(runner.VMPath, input.Name), unix.S_IFBLK|0666, deviceID); err != nil {
						return errors.Join(ErrCouldNotCreateDeviceNode, err)
					}
				}

				if hook := hooks.OnLocalDeviceExposed; hook != nil {
					hook(uint32(index), devicePath)
				}

				return nil
			},
		)

		// Make sure that we schedule the `deferFuncs` even if we get an error during device setup
		for _, deferFuncs := range deferFuncs {
			for _, deferFunc := range deferFuncs {
				deviceCloseFuncsLock.Lock()
				deviceCloseFuncs = append(deviceCloseFuncs, deferFunc) // defer deferFunc()
				deviceCloseFuncsLock.Unlock()
			}
		}

		if err != nil {
			panic(errors.Join(ErrCouldNotSetupDevices, err))
		}

		select {
		case <-internalCtx.Done():
			if err := internalCtx.Err(); err != nil {
				panic(errors.Join(ErrPeerContextCancelled, err))
			}

			return
		case <-allRemoteDevicesReadyCtx.Done():
			break
		}

		migratedPeer.Resume = func(
			ctx context.Context,

			resumeTimeout,
			rescueTimeout time.Duration,
		) (resumedPeer *ResumedPeer, errs error) {
			configBasePath := ""
			for _, device := range devices {
				if device.Name == ConfigName {
					configBasePath = device.Base

					break
				}
			}

			if strings.TrimSpace(configBasePath) == "" {
				return nil, ErrConfigFileNotFound
			}

			packageConfigFile, err := os.Open(configBasePath)
			if err != nil {
				return nil, errors.Join(ErrCouldNotOpenConfigFile, err)
			}
			defer packageConfigFile.Close()

			var packageConfig PackageConfiguration
			if err := json.NewDecoder(packageConfigFile).Decode(&packageConfig); err != nil {
				return nil, errors.Join(ErrCouldNotDecodeConfigFile, err)
			}

			resumedRunner, err := runner.Resume(ctx, resumeTimeout, rescueTimeout, packageConfig.AgentVSockPort)
			if err != nil {
				return nil, errors.Join(ErrCouldNotResumeRunner, err)
			}

			return &ResumedPeer{
				Wait:  resumedRunner.Wait,
				Close: resumedRunner.Close,

				SuspendAndCloseAgentServer: resumedRunner.SuspendAndCloseAgentServer,

				MakeMigratable: func(
					ctx context.Context,

					devices []MakeMigratableDevice,
				) (migratablePeer *MigratablePeer, errs error) {
					migratablePeer = &MigratablePeer{}

					stage3Inputs := []peerStage3{}
					for _, input := range stage2Inputs {
						var makeMigratableDevice *MakeMigratableDevice
						for _, device := range devices {
							if device.Name == input.name {
								makeMigratableDevice = &device

								break
							}
						}

						// We don't want to make this device migratable
						if makeMigratableDevice == nil {
							continue
						}

						stage3Inputs = append(stage3Inputs, peerStage3{
							prev: input,

							makeMigratableDevice: *makeMigratableDevice,
						})
					}

					stage4Inputs, deferFuncs, err := iutils.ConcurrentMap(
						stage3Inputs,
						func(index int, input peerStage3, output *peerStage4, addDefer func(deferFunc func() error)) error {
							output.prev = input

							dirtyLocal, dirtyRemote := dirtytracker.NewDirtyTracker(input.prev.storage, int(input.prev.blockSize))
							output.dirtyRemote = dirtyRemote
							monitor := volatilitymonitor.NewVolatilityMonitor(dirtyLocal, int(input.prev.blockSize), input.makeMigratableDevice.Expiry)

							local := modules.NewLockable(monitor)
							output.storage = local
							addDefer(func() error {
								local.Unlock()

								return nil
							})

							input.prev.device.SetProvider(local)

							totalBlocks := (int(local.Size()) + int(input.prev.blockSize) - 1) / int(input.prev.blockSize)
							output.totalBlocks = totalBlocks

							orderer := blocks.NewPriorityBlockOrder(totalBlocks, monitor)
							output.orderer = orderer
							orderer.AddAll()

							return nil
						},
					)

					migratablePeer.Close = func() {
						// Make sure that we schedule the `deferFuncs` even if we get an error
						for _, deferFuncs := range deferFuncs {
							for _, deferFunc := range deferFuncs {
								defer deferFunc() // We can safely ignore errors here since we never call `addDefer` with a function that could return an error
							}
						}
					}

					if err != nil {
						// Make sure that we schedule the `deferFuncs` even if we get an error
						migratablePeer.Close()

						panic(errors.Join(ErrCouldNotCreateMigratablePeer, err))
					}

					migratablePeer.MigrateTo = func(
						ctx context.Context,

						devices []MigrateToDevice,

						suspendTimeout time.Duration,
						concurrency int,

						readers []io.Reader,
						writers []io.Writer,

						hooks MigrateToHooks,
					) (errs error) {
						ctx, handlePanics, handleGoroutinePanics, cancel, wait, _ := utils.GetPanicHandler(
							ctx,
							&errs,
							utils.GetPanicHandlerHooks{},
						)
						defer wait()
						defer cancel()
						defer handlePanics(false)()

						pro := protocol.NewProtocolRW(
							ctx,
							readers,
							writers,
							nil,
						)

						handleGoroutinePanics(true, func() {
							if err := pro.Handle(); err != nil && !errors.Is(err, io.EOF) {
								panic(errors.Join(ErrCouldNotHandleProtocol, err))
							}
						})

						var (
							devicesLeftToSend                 atomic.Int32
							devicesLeftToTransferAuthorityFor atomic.Int32

							suspendedVMLock sync.Mutex
							suspendedVM     bool
						)

						// We use the background context here instead of the internal context because we want to distinguish
						// between a context cancellation from the outside and getting a response
						suspendedVMCtx, cancelSuspendedVMCtx := context.WithCancel(context.Background())
						defer cancelSuspendedVMCtx()

						suspendAndMsyncVM := sync.OnceValue(func() error {
							if hook := hooks.OnBeforeSuspend; hook != nil {
								hook()
							}

							if err := resumedPeer.SuspendAndCloseAgentServer(ctx, suspendTimeout); err != nil {
								return errors.Join(ErrCouldNotSuspendAndCloseAgentServer, err)
							}

							if err := resumedRunner.Msync(ctx); err != nil {
								return errors.Join(ErrCouldNotMsyncRunner, err)
							}

							if hook := hooks.OnAfterSuspend; hook != nil {
								hook()
							}

							suspendedVMLock.Lock()
							suspendedVM = true
							suspendedVMLock.Unlock()

							cancelSuspendedVMCtx()

							return nil
						})

						stage5Inputs := []peerStage5{}
						for _, input := range stage4Inputs {
							var migrateToDevice *MigrateToDevice
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

							stage5Inputs = append(stage5Inputs, peerStage5{
								prev: input,

								migrateToDevice: *migrateToDevice,
							})
						}

						_, deferFuncs, err := iutils.ConcurrentMap(
							stage5Inputs,
							func(index int, input peerStage5, _ *struct{}, _ func(deferFunc func() error)) error {
								to := protocol.NewToProtocol(input.prev.storage.Size(), uint32(index), pro)

								if err := to.SendDevInfo(input.prev.prev.prev.name, input.prev.prev.prev.blockSize, ""); err != nil {
									return errors.Join(ErrCouldNotSendDevInfo, err)
								}

								if hook := hooks.OnDeviceSent; hook != nil {
									hook(uint32(index), input.prev.prev.prev.remote)
								}

								devicesLeftToSend.Add(1)
								if devicesLeftToSend.Load() >= int32(len(stage5Inputs)) {
									handleGoroutinePanics(true, func() {
										if err := to.SendEvent(&packets.Event{
											Type:       packets.EventCustom,
											CustomType: byte(EventCustomAllDevicesSent),
										}); err != nil {
											panic(errors.Join(ErrCouldNotSendAllDevicesSentEvent, err))
										}

										if hook := hooks.OnAllDevicesSent; hook != nil {
											hook()
										}
									})
								}

								handleGoroutinePanics(true, func() {
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
										panic(errors.Join(ErrCouldNotHandleNeedAt, err))
									}
								})

								handleGoroutinePanics(true, func() {
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
										panic(errors.Join(ErrCouldNotHandleDontNeedAt, err))
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
									defer handlePanics(false)()

									if err := to.SendEvent(&packets.Event{
										Type: packets.EventPreLock,
									}); err != nil {
										panic(errors.Join(ErrCouldNotSendPreLockEvent, err))
									}

									input.prev.storage.Lock()

									if err := to.SendEvent(&packets.Event{
										Type: packets.EventPostLock,
									}); err != nil {
										panic(errors.Join(ErrCouldNotSendPostLockEvent, err))
									}
								}
								cfg.Unlocker_handler = func() {
									defer handlePanics(false)()

									if err := to.SendEvent(&packets.Event{
										Type: packets.EventPreUnlock,
									}); err != nil {
										panic(errors.Join(ErrCouldNotSendPreUnlockEvent, err))
									}

									input.prev.storage.Unlock()

									if err := to.SendEvent(&packets.Event{
										Type: packets.EventPostUnlock,
									}); err != nil {
										panic(errors.Join(ErrCouldNotSendPostUnlockEvent, err))
									}
								}
								cfg.Error_handler = func(b *storage.BlockInfo, err error) {
									defer handlePanics(false)()

									if err != nil {
										panic(errors.Join(ErrCouldNotContinueWithMigration, err))
									}
								}
								cfg.Progress_handler = func(p *migrator.MigrationProgress) {
									if hook := hooks.OnDeviceInitialMigrationProgress; hook != nil {
										hook(uint32(index), input.prev.prev.prev.remote, p.Ready_blocks, p.Total_blocks)
									}
								}

								mig, err := migrator.NewMigrator(input.prev.dirtyRemote, to, input.prev.orderer, cfg)
								if err != nil {
									return errors.Join(ErrCouldNotCreateMigrator, err)
								}

								if err := mig.Migrate(input.prev.totalBlocks); err != nil {
									return errors.Join(ErrCouldNotMigrateBlocks, err)
								}

								if err := mig.WaitForCompletion(); err != nil {
									return errors.Join(ErrCouldNotWaitForMigrationCompletion, err)
								}

								markDeviceAsReadyForAuthorityTransfer := sync.OnceFunc(func() {
									devicesLeftToTransferAuthorityFor.Add(1)
								})

								var (
									cyclesBelowDirtyBlockTreshold = 0
									totalCycles                   = 0
									ongoingMigrationsWg           sync.WaitGroup
								)
								for {
									suspendedVMLock.Lock()
									// We only need to `msync` for the memory because `msync` only affects the memory
									if !suspendedVM && input.prev.prev.prev.name == MemoryName {
										if err := resumedRunner.Msync(ctx); err != nil {
											suspendedVMLock.Unlock()

											return errors.Join(ErrCouldNotMsyncRunner, err)
										}
									}
									suspendedVMLock.Unlock()

									ongoingMigrationsWg.Wait()

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
											return errors.Join(ErrCouldNotSendDirtyList, err)
										}

										ongoingMigrationsWg.Add(1)
										handleGoroutinePanics(true, func() {
											defer ongoingMigrationsWg.Done()

											if err := mig.MigrateDirty(blocks); err != nil {
												panic(errors.Join(ErrCouldNotMigrateDirtyBlocks, err))
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

										case <-suspendedVMCtx.Done():
											break

										case <-ctx.Done(): // ctx is the internalCtx here
											if err := ctx.Err(); err != nil {
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
											return errors.Join(ErrCouldNotSuspendAndMsyncVM, err)
										}
									}
								}

								if err := to.SendEvent(&packets.Event{
									Type:       packets.EventCustom,
									CustomType: byte(EventCustomTransferAuthority),
								}); err != nil {
									panic(errors.Join(ErrCouldNotSendTransferAuthorityEvent, err))
								}

								if hook := hooks.OnDeviceAuthoritySent; hook != nil {
									hook(uint32(index), input.prev.prev.prev.remote)
								}

								if err := mig.WaitForCompletion(); err != nil {
									return errors.Join(ErrCouldNotWaitForMigrationCompletion, err)
								}

								if err := to.SendEvent(&packets.Event{
									Type: packets.EventCompleted,
								}); err != nil {
									return errors.Join(ErrCouldNotSendCompletedEvent, err)
								}

								if hook := hooks.OnDeviceMigrationCompleted; hook != nil {
									hook(uint32(index), input.prev.prev.prev.remote)
								}

								return nil
							},
						)

						if err != nil {
							panic(errors.Join(ErrCouldNotMigrateToDevice, err))
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

					return
				},
			}, nil
		}

		return
	}

	return
}
