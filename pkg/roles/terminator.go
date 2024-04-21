package roles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/loopholelabs/drafter/pkg/config"
	"github.com/loopholelabs/silo/pkg/storage"
	"github.com/loopholelabs/silo/pkg/storage/protocol"
	"github.com/loopholelabs/silo/pkg/storage/sources"
	"github.com/loopholelabs/silo/pkg/storage/waitingcache"
)

type CustomEventType byte

const (
	EventCustomPassAuthority  = CustomEventType(0)
	EventCustomAllDevicesSent = CustomEventType(1)
)

var (
	ErrUnknownDeviceName = errors.New("unknown device name")
)

type TerminateHooks struct {
	OnDeviceReceived           func(deviceID uint32, name string)
	OnDeviceAuthorityReceived  func(deviceID uint32)
	OnDeviceMigrationCompleted func(deviceID uint32)

	OnAllDevicesReceived     func()
	OnAllMigrationsCompleted func()
}

func Terminate(
	ctx context.Context,
	connCloser io.Closer, // TODO: Make `func (p *protocol.ProtocolRW) Handle() error` return if context is cancelled, then remove this workaround

	statePath,
	memoryPath,
	initramfsPath,
	kernelPath,
	diskPath,
	configPath string,

	readers []io.Reader,
	writers []io.Writer,

	hooks TerminateHooks,
) (errs []error) {
	var errsLock sync.Mutex

	defer func() {
		if err := recover(); err != nil {
			errsLock.Lock()
			defer errsLock.Unlock()

			errs = append(errs, fmt.Errorf("%v", err))
		}
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	handleGoroutinePanic := func() func() {
		return func() {
			if err := recover(); err != nil {
				errsLock.Lock()
				defer errsLock.Unlock()

				errs = append(errs, fmt.Errorf("%v", err))

				cancel()

				// TODO: Make `func (p *protocol.ProtocolRW) Handle() error` return if context is cancelled, then remove this workaround
				if connCloser != nil {
					if err := connCloser.Close(); err != nil {
						errs = append(errs, err)
					}
				}
			}
		}
	}

	pro := protocol.NewProtocolRW(
		ctx,
		readers,
		writers,
		func(p protocol.Protocol, u uint32) {
			var (
				from  *protocol.FromProtocol
				local *waitingcache.WaitingCacheLocal
			)
			from = protocol.NewFromProtocol(
				u,
				func(di *protocol.DevInfo) storage.StorageProvider {
					defer handleGoroutinePanic()()

					var (
						path = ""
					)
					switch di.Name {
					case config.ConfigName:
						path = configPath

					case config.DiskName:
						path = diskPath

					case config.InitramfsName:
						path = initramfsPath

					case config.KernelName:
						path = kernelPath

					case config.MemoryName:
						path = memoryPath

					case config.StateName:
						path = statePath
					}

					if strings.TrimSpace(path) == "" {
						panic(ErrUnknownDeviceName)
					}

					if hook := hooks.OnDeviceReceived; hook != nil {
						hook(u, di.Name)
					}

					if err := os.MkdirAll(filepath.Dir(path), os.ModePerm); err != nil {
						panic(err)
					}

					storage, err := sources.NewFileStorageCreate(path, int64(di.Size))
					if err != nil {
						panic(err)
					}

					var remote *waitingcache.WaitingCacheRemote
					local, remote = waitingcache.NewWaitingCache(storage, int(di.BlockSize))
					local.NeedAt = func(offset int64, length int32) {
						if err := from.NeedAt(offset, length); err != nil {
							panic(err)
						}
					}
					local.DontNeedAt = func(offset int64, length int32) {
						if err := from.DontNeedAt(offset, length); err != nil {
							panic(err)
						}
					}

					return remote
				},
				p,
			)

			wg.Add(1)
			go func() {
				defer wg.Done()
				defer handleGoroutinePanic()()

				if err := from.HandleSend(ctx); err != nil && !errors.Is(err, context.Canceled) {
					panic(err)
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				defer handleGoroutinePanic()()

				if err := from.HandleReadAt(); err != nil && !errors.Is(err, context.Canceled) {
					panic(err)
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				defer handleGoroutinePanic()()

				if err := from.HandleWriteAt(); err != nil && !errors.Is(err, context.Canceled) {
					panic(err)
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				defer handleGoroutinePanic()()

				if err := from.HandleDevInfo(); err != nil {
					panic(err)
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				defer handleGoroutinePanic()()

				if err := from.HandleEvent(func(e *protocol.Event) {
					switch e.Type {
					case protocol.EventCustom:
						switch e.CustomType {
						case byte(EventCustomPassAuthority):
							if hook := hooks.OnDeviceAuthorityReceived; hook != nil {
								hook(u)
							}

						case byte(EventCustomAllDevicesSent):
							if hook := hooks.OnAllDevicesReceived; hook != nil {
								hook()
							}
						}

					case protocol.EventCompleted:
						if hook := hooks.OnDeviceMigrationCompleted; hook != nil {
							hook(u)
						}
					}
				}); err != nil && !errors.Is(err, context.Canceled) {
					panic(err)
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				defer handleGoroutinePanic()()

				if err := from.HandleDirtyList(func(blocks []uint) {
					if local != nil {
						local.DirtyBlocks(blocks)
					}
				}); err != nil && !errors.Is(err, context.Canceled) {
					panic(err)
				}
			}()
		})

	if err := pro.Handle(); err != nil && !errors.Is(err, io.EOF) {
		panic(err)
	}

	if hook := hooks.OnAllMigrationsCompleted; hook != nil {
		hook()
	}

	return errs
}
