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
	"github.com/loopholelabs/drafter/pkg/config"
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
	ErrCouldNotGetNBDDeviceStat = errors.New("could not get NBD device stat")
)

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

		resumeTimeout time.Duration,
	) (
		resumedPeer *ResumedPeer,

		errs error,
	)
}

type ResumedPeer struct {
	Wait  func() error
	Close func() error

	SuspendAndCloseAgentServer func(ctx context.Context, resumeTimeout time.Duration) error

	MakeMigratable func(ctx context.Context) (migratablePeer *MigratablePeer, errs error)
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

		stateMaxDirtyBlocks,
		memoryMaxDirtyBlocks,
		initramfsMaxDirtyBlocks,
		kernelMaxDirtyBlocks,
		diskMaxDirtyBlocks,
		configMaxDirtyBlocks,

		stateMinCycles,
		memoryMinCycles,
		initramfsMinCycles,
		kernelMinCycles,
		diskMinCycles,
		configMinCycles,

		stateMaxCycles,
		memoryMaxCycles,
		initramfsMaxCycles,
		kernelMaxCycles,
		diskMaxCycles,
		configMaxCycles int,

		suspendTimeout time.Duration,
		concurrency int,

		readers []io.Reader,
		writers []io.Writer,

		hooks MigrateToHooks,
	) (errs error)
}

type peerStage1 struct {
	name string

	base    string
	overlay string
	state   string

	blockSize uint32
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

	storage     *modules.Lockable
	orderer     *blocks.PriorityBlockOrder
	totalBlocks int
	dirtyRemote *dirtytracker.DirtyTrackerRemote
}

type Peer struct {
	VMPath string

	Wait  func() error
	Close func() error

	MigrateFrom func(
		ctx context.Context,

		stateBasePath,
		memoryBasePath,
		initramfsBasePath,
		kernelBasePath,
		diskBasePath,
		configBasePath,

		stateOverlayPath,
		memoryOverlayPath,
		initramfsOverlayPath,
		kernelOverlayPath,
		diskOverlayPath,
		configOverlayPath,

		stateStatePath,
		memoryStatePath,
		initramfsStatePath,
		kernelStatePath,
		diskStatePath,
		configStatePath string,

		stateBlockSize,
		memoryBlockSize,
		initramfsBlockSize,
		kernelBlockSize,
		diskBlockSize,
		configBlockSize uint32,

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
	hypervisorConfiguration config.HypervisorConfiguration,

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
		panic(err)
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

		stateBasePath,
		memoryBasePath,
		initramfsBasePath,
		kernelBasePath,
		diskBasePath,
		configBasePath,

		stateOverlayPath,
		memoryOverlayPath,
		initramfsOverlayPath,
		kernelOverlayPath,
		diskOverlayPath,
		configOverlayPath,

		stateStatePath,
		memoryStatePath,
		initramfsStatePath,
		kernelStatePath,
		diskStatePath,
		configStatePath string,

		stateBlockSize,
		memoryBlockSize,
		initramfsBlockSize,
		kernelBlockSize,
		diskBlockSize,
		configBlockSize uint32,

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
				func(p protocol.Protocol, index uint32) {
					var (
						from  *protocol.FromProtocol
						local *waitingcache.WaitingCacheLocal
					)
					from = protocol.NewFromProtocol(
						index,
						func(di *packets.DevInfo) storage.StorageProvider {
							defer handlePanics(false)()

							var (
								base = ""
							)
							switch di.Name {
							case config.ConfigName:
								base = configBasePath

							case config.DiskName:
								base = diskBasePath

							case config.InitramfsName:
								base = initramfsBasePath

							case config.KernelName:
								base = kernelBasePath

							case config.MemoryName:
								base = memoryBasePath

							case config.StateName:
								base = stateBasePath
							}

							if strings.TrimSpace(base) == "" {
								panic(ErrUnknownDeviceName)
							}

							receivedButNotReadyRemoteDevices.Add(1)

							if hook := hooks.OnRemoteDeviceReceived; hook != nil {
								hook(index, di.Name)
							}

							if err := os.MkdirAll(filepath.Dir(base), os.ModePerm); err != nil {
								panic(err)
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
								panic(err)
							}
							deviceCloseFuncsLock.Lock()
							deviceCloseFuncs = append(deviceCloseFuncs, src.Close)       // defer src.Close()
							deviceCloseFuncs = append(deviceCloseFuncs, device.Shutdown) // defer device.Shutdown()
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
									panic(err)
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
									panic(err)
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
								panic(err)
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
									panic(internalCtx.Err())
								}

								return nil

							default:
								if err := unix.Mknod(filepath.Join(runner.VMPath, di.Name), unix.S_IFBLK|0666, deviceID); err != nil {
									panic(err)
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
							panic(err)
						}
					})

					handleGoroutinePanics(true, func() {
						if err := from.HandleWriteAt(); err != nil {
							panic(err)
						}
					})

					handleGoroutinePanics(true, func() {
						if err := from.HandleDevInfo(); err != nil {
							panic(err)
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
							panic(err)
						}
					})

					handleGoroutinePanics(true, func() {
						if err := from.HandleDirtyList(func(blocks []uint) {
							if local != nil {
								local.DirtyBlocks(blocks)
							}
						}); err != nil {
							panic(err)
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
				panic(err)
			}
		})

		// We don't track this because we return the close function
		handleGoroutinePanics(false, func() {
			select {
			// Failure case; we cancelled the internal context before all devices are ready
			case <-internalCtx.Done():
				if err := migratedPeer.Close(); err != nil {
					panic(err)
				}

			// Happy case; all devices are ready and we want to wait with closing the devices until we stop the Firecracker process
			case <-allRemoteDevicesReadyCtx.Done():
				<-hypervisorCtx.Done()

				if err := migratedPeer.Close(); err != nil {
					panic(err)
				}

				break
			}
		})

		select {
		case <-internalCtx.Done():
			if err := internalCtx.Err(); err != nil {
				panic(internalCtx.Err())
			}

			return
		case <-allRemoteDevicesReceivedCtx.Done():
			break
		}

		allStage1Inputs := []peerStage1{
			{
				name: config.StateName,

				base:    stateBasePath,
				overlay: stateOverlayPath,
				state:   stateStatePath,

				blockSize: stateBlockSize,
			},
			{
				name: config.MemoryName,

				base:    memoryBasePath,
				overlay: memoryOverlayPath,
				state:   memoryStatePath,

				blockSize: memoryBlockSize,
			},
			{
				name: config.InitramfsName,

				base:    initramfsBasePath,
				overlay: initramfsOverlayPath,
				state:   initramfsStatePath,

				blockSize: initramfsBlockSize,
			},
			{
				name: config.KernelName,

				base:    kernelBasePath,
				overlay: kernelOverlayPath,
				state:   kernelStatePath,

				blockSize: kernelBlockSize,
			},
			{
				name: config.DiskName,

				base:    diskBasePath,
				overlay: diskOverlayPath,
				state:   diskStatePath,

				blockSize: diskBlockSize,
			},
			{
				name: config.ConfigName,

				base:    configBasePath,
				overlay: configOverlayPath,
				state:   configStatePath,

				blockSize: configBlockSize,
			},
		}

		stage1Inputs := []peerStage1{}
		for _, input := range allStage1Inputs {
			if slices.ContainsFunc(
				stage2Inputs,
				func(r peerStage2) bool {
					return input.name == r.name
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
			func(index int, input peerStage1, _ *struct{}, addDefer func(deferFunc func() error)) error {
				if hook := hooks.OnLocalDeviceRequested; hook != nil {
					hook(uint32(index), input.name)
				}

				if remainingRequestedLocalDevices.Add(-1) <= 0 {
					if hook := hooks.OnLocalAllDevicesRequested; hook != nil {
						hook()
					}
				}

				stat, err := os.Stat(input.base)
				if err != nil {
					return err
				}

				var (
					local  storage.StorageProvider
					device storage.ExposedStorage
				)
				if strings.TrimSpace(input.overlay) == "" || strings.TrimSpace(input.state) == "" {
					local, device, err = sdevice.NewDevice(&sconfig.DeviceSchema{
						Name:      input.name,
						System:    "file",
						Location:  input.base,
						Size:      fmt.Sprintf("%v", stat.Size()),
						BlockSize: fmt.Sprintf("%v", input.blockSize),
						Expose:    true,
					})
				} else {
					if err := os.MkdirAll(filepath.Dir(input.overlay), os.ModePerm); err != nil {
						return err
					}

					if err := os.MkdirAll(filepath.Dir(input.state), os.ModePerm); err != nil {
						return err
					}

					local, device, err = sdevice.NewDevice(&sconfig.DeviceSchema{
						Name:      input.name,
						System:    "sparsefile",
						Location:  input.overlay,
						Size:      fmt.Sprintf("%v", stat.Size()),
						BlockSize: fmt.Sprintf("%v", input.blockSize),
						Expose:    true,
						ROSource: &sconfig.DeviceSchema{
							Name:     input.state,
							System:   "file",
							Location: input.base,
							Size:     fmt.Sprintf("%v", stat.Size()),
						},
					})
				}
				if err != nil {
					return err
				}
				addDefer(local.Close)
				addDefer(device.Shutdown)

				device.SetProvider(local)

				stage2InputsLock.Lock()
				stage2Inputs = append(stage2Inputs, peerStage2{
					name: input.name,

					blockSize: input.blockSize,

					id:     uint32(index),
					remote: false,

					storage: local,
					device:  device,
				})
				stage2InputsLock.Unlock()

				devicePath := filepath.Join("/dev", device.Device())

				deviceInfo, err := os.Stat(devicePath)
				if err != nil {
					return err
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
						return internalCtx.Err()
					}

					return nil

				default:
					if err := unix.Mknod(filepath.Join(runner.VMPath, input.name), unix.S_IFBLK|0666, deviceID); err != nil {
						return err
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
			panic(err)
		}

		select {
		case <-internalCtx.Done():
			if err := internalCtx.Err(); err != nil {
				panic(internalCtx.Err())
			}

			return
		case <-allRemoteDevicesReadyCtx.Done():
			break
		}

		migratedPeer.Resume = func(ctx context.Context, resumeTimeout time.Duration) (resumedPeer *ResumedPeer, errs error) {
			packageConfigFile, err := os.Open(configBasePath)
			if err != nil {
				return nil, err
			}
			defer packageConfigFile.Close()

			var packageConfig config.PackageConfiguration
			if err := json.NewDecoder(packageConfigFile).Decode(&packageConfig); err != nil {
				return nil, err
			}

			resumedRunner, err := runner.Resume(ctx, resumeTimeout, packageConfig.AgentVSockPort)
			if err != nil {
				return nil, err
			}

			return &ResumedPeer{
				Wait:  resumedRunner.Wait,
				Close: resumedRunner.Close,

				SuspendAndCloseAgentServer: resumedRunner.SuspendAndCloseAgentServer,

				MakeMigratable: func(ctx context.Context) (migratablePeer *MigratablePeer, errs error) {
					migratablePeer = &MigratablePeer{}

					stage3Inputs, deferFuncs, err := iutils.ConcurrentMap(
						stage2Inputs,
						func(index int, input peerStage2, output *peerStage3, addDefer func(deferFunc func() error)) error {
							output.prev = input

							metrics := modules.NewMetrics(input.storage)
							dirtyLocal, dirtyRemote := dirtytracker.NewDirtyTracker(metrics, int(input.blockSize))
							output.dirtyRemote = dirtyRemote
							monitor := volatilitymonitor.NewVolatilityMonitor(dirtyLocal, int(input.blockSize), 10*time.Second)

							local := modules.NewLockable(monitor)
							output.storage = local
							addDefer(func() error {
								local.Unlock()

								return nil
							})

							input.device.SetProvider(local)

							totalBlocks := (int(local.Size()) + int(input.blockSize) - 1) / int(input.blockSize)
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

						panic(err)
					}

					migratablePeer.MigrateTo = func(
						ctx context.Context,

						stateMaxDirtyBlocks,
						memoryMaxDirtyBlocks,
						initramfsMaxDirtyBlocks,
						kernelMaxDirtyBlocks,
						diskMaxDirtyBlocks,
						configMaxDirtyBlocks,

						stateMinCycles,
						memoryMinCycles,
						initramfsMinCycles,
						kernelMinCycles,
						diskMinCycles,
						configMinCycles,

						stateMaxCycles,
						memoryMaxCycles,
						initramfsMaxCycles,
						kernelMaxCycles,
						diskMaxCycles,
						configMaxCycles int,

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
								panic(err)
							}
						})

						var (
							devicesLeftToSend                 atomic.Int32
							devicesLeftToTransferAuthorityFor atomic.Int32

							suspendedVMLock sync.Mutex
							suspendedVM     bool
						)

						suspendVM := sync.OnceValue(func() error {
							if hook := hooks.OnBeforeSuspend; hook != nil {
								hook()
							}

							if err := resumedPeer.SuspendAndCloseAgentServer(ctx, suspendTimeout); err != nil {
								return err
							}

							if hook := hooks.OnAfterSuspend; hook != nil {
								hook()
							}

							suspendedVMLock.Lock()
							suspendedVM = true
							suspendedVMLock.Unlock()

							return nil
						})

						_, deferFuncs, err := iutils.ConcurrentMap(
							stage3Inputs,
							func(index int, input peerStage3, _ *struct{}, _ func(deferFunc func() error)) error {
								to := protocol.NewToProtocol(input.storage.Size(), uint32(index), pro)

								if err := to.SendDevInfo(input.prev.name, input.prev.blockSize); err != nil {
									return err
								}

								if hook := hooks.OnDeviceSent; hook != nil {
									hook(uint32(index), input.prev.remote)
								}

								devicesLeftToSend.Add(1)
								if devicesLeftToSend.Load() >= int32(len(stage3Inputs)) {
									handleGoroutinePanics(true, func() {
										if err := to.SendEvent(&packets.Event{
											Type:       packets.EventCustom,
											CustomType: byte(EventCustomAllDevicesSent),
										}); err != nil {
											panic(err)
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
										if endOffset > uint64(input.storage.Size()) {
											endOffset = uint64(input.storage.Size())
										}

										startBlock := int(offset / int64(input.prev.blockSize))
										endBlock := int((endOffset-1)/uint64(input.prev.blockSize)) + 1
										for b := startBlock; b < endBlock; b++ {
											input.orderer.PrioritiseBlock(b)
										}
									}); err != nil {
										panic(err)
									}
								})

								handleGoroutinePanics(true, func() {
									if err := to.HandleDontNeedAt(func(offset int64, length int32) {
										// Deprioritize blocks
										endOffset := uint64(offset + int64(length))
										if endOffset > uint64(input.storage.Size()) {
											endOffset = uint64(input.storage.Size())
										}

										startBlock := int(offset / int64(input.storage.Size()))
										endBlock := int((endOffset-1)/uint64(input.storage.Size())) + 1
										for b := startBlock; b < endBlock; b++ {
											input.orderer.Remove(b)
										}
									}); err != nil {
										panic(err)
									}
								})

								cfg := migrator.NewMigratorConfig().WithBlockSize(int(input.prev.blockSize))
								cfg.Concurrency = map[int]int{
									storage.BlockTypeAny:      concurrency,
									storage.BlockTypeStandard: concurrency,
									storage.BlockTypeDirty:    concurrency,
									storage.BlockTypePriority: concurrency,
								}
								cfg.Progress_handler = func(p *migrator.MigrationProgress) {
									if hook := hooks.OnDeviceInitialMigrationProgress; hook != nil {
										hook(uint32(index), input.prev.remote, p.Ready_blocks, p.Total_blocks)
									}
								}

								mig, err := migrator.NewMigrator(input.dirtyRemote, to, input.orderer, cfg)
								if err != nil {
									return err
								}

								if err := mig.Migrate(input.totalBlocks); err != nil {
									return err
								}

								if err := mig.WaitForCompletion(); err != nil {
									return err
								}

								markDeviceAsReadyForAuthorityTransfer := sync.OnceFunc(func() {
									devicesLeftToTransferAuthorityFor.Add(1)
								})

								var (
									maxDirtyBlocks int
									minCycles      int
									maxCycles      int
								)
								switch input.prev.name {
								case config.ConfigName:
									maxDirtyBlocks = configMaxDirtyBlocks
									minCycles = configMinCycles
									maxCycles = configMaxCycles

								case config.DiskName:
									maxDirtyBlocks = diskMaxDirtyBlocks
									minCycles = diskMinCycles
									maxCycles = diskMaxCycles

								case config.InitramfsName:
									maxDirtyBlocks = initramfsMaxDirtyBlocks
									minCycles = initramfsMinCycles
									maxCycles = initramfsMaxCycles

								case config.KernelName:
									maxDirtyBlocks = kernelMaxDirtyBlocks
									minCycles = kernelMinCycles
									maxCycles = kernelMaxCycles

								case config.MemoryName:
									maxDirtyBlocks = memoryMaxDirtyBlocks
									minCycles = memoryMinCycles
									maxCycles = memoryMaxCycles

								case config.StateName:
									maxDirtyBlocks = stateMaxDirtyBlocks
									minCycles = stateMinCycles
									maxCycles = stateMaxCycles

									// No need for a default case/check here - we validate that all resources have valid names earlier
								}

								var (
									finalDirtyBlocks              []uint
									cyclesBelowDirtyBlockTreshold = 0
									totalCycles                   = 0
								)
								for {
									if err := resumedRunner.Msync(ctx); err != nil {
										return err
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

									if err := to.DirtyList(blocks); err != nil {
										return err
									}

									if hook := hooks.OnDeviceContinousMigrationProgress; hook != nil {
										hook(uint32(index), input.prev.remote, len(blocks))
									}

									totalCycles++
									if len(blocks) < maxDirtyBlocks {
										cyclesBelowDirtyBlockTreshold++
										if cyclesBelowDirtyBlockTreshold > minCycles {
											markDeviceAsReadyForAuthorityTransfer()
										}
									} else if totalCycles > maxCycles {
										markDeviceAsReadyForAuthorityTransfer()
									} else {
										cyclesBelowDirtyBlockTreshold = 0
									}

									if devicesLeftToTransferAuthorityFor.Load() >= int32(len(stage3Inputs)) {
										if err := suspendVM(); err != nil {
											return err
										}
									}

									suspendedVMLock.Lock()
									if suspendedVM {
										finalDirtyBlocks = append(finalDirtyBlocks, blocks...)
									} else {
										if err := mig.MigrateDirty(blocks); err != nil {
											suspendedVMLock.Unlock()

											return err
										}
									}
									suspendedVMLock.Unlock()
								}

								if err := to.SendEvent(&packets.Event{
									Type:       packets.EventCustom,
									CustomType: byte(EventCustomTransferAuthority),
								}); err != nil {
									panic(err)
								}

								if hook := hooks.OnDeviceAuthoritySent; hook != nil {
									hook(uint32(index), input.prev.remote)
								}

								if err := mig.MigrateDirty(finalDirtyBlocks); err != nil {
									return err
								}

								if hook := hooks.OnDeviceFinalMigrationProgress; hook != nil {
									hook(uint32(index), input.prev.remote, len(finalDirtyBlocks))
								}

								if err := mig.WaitForCompletion(); err != nil {
									return err
								}

								if err := to.SendEvent(&packets.Event{
									Type: packets.EventCompleted,
								}); err != nil {
									return err
								}

								if hook := hooks.OnDeviceMigrationCompleted; hook != nil {
									hook(uint32(index), input.prev.remote)
								}

								return nil
							},
						)

						if err != nil {
							panic(err)
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
