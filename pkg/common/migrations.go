package common

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/loopholelabs/logging/types"
	"github.com/loopholelabs/silo/pkg/storage/config"
	"github.com/loopholelabs/silo/pkg/storage/devicegroup"
	"github.com/loopholelabs/silo/pkg/storage/metrics"
	"github.com/loopholelabs/silo/pkg/storage/migrator"
	"github.com/loopholelabs/silo/pkg/storage/protocol"
	"github.com/loopholelabs/silo/pkg/storage/protocol/packets"
	"golang.org/x/sys/unix"
)

var (
	ErrCouldNotCreateDeviceDirectory  = errors.New("could not create device directory")
	ErrCouldNotGetBaseDeviceStat      = errors.New("could not get base device statistics")
	ErrCouldNotCreateOverlayDirectory = errors.New("could not create overlay directory")
	ErrCouldNotCreateStateDirectory   = errors.New("could not create state directory")
)

type MigrateToDevice struct {
	Name string `json:"name"`

	MaxDirtyBlocks int `json:"maxDirtyBlocks"`
	MinCycles      int `json:"minCycles"`
	MaxCycles      int `json:"maxCycles"`

	CycleThrottle time.Duration `json:"cycleThrottle"`
}

type MigrateFromDevice struct {
	Name      string `json:"name"`
	Base      string `json:"base"`
	Overlay   string `json:"overlay"`
	State     string `json:"state"`
	BlockSize uint32 `json:"blockSize"`
	Shared    bool   `json:"shared"`

	SharedBase bool `json:"sharedbase"`

	S3Sync      bool   `json:"s3sync"`
	S3AccessKey string `json:"s3accesskey"`
	S3SecretKey string `json:"s3secretkey"`
	S3Endpoint  string `json:"s3endpoint"`
	S3Bucket    string `json:"s3bucket"`
}

var (
	ErrCouldNotGetNBDDeviceStat = errors.New("could not get NBD device stat")
	ErrCouldNotCreateDeviceNode = errors.New("could not create device node")
)

// expose a Silo Device as a file within the vm directory
func ExposeSiloDeviceAsFile(vmpath string, name string, devicePath string) error {
	if vmpath == "" {
		return nil
	}
	deviceInfo, err := os.Stat(devicePath)
	if err != nil {
		return errors.Join(ErrCouldNotGetBaseDeviceStat, err)
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
func CreateSiloDevSchema(i *MigrateFromDevice) (*config.DeviceSchema, error) {
	stat, err := os.Stat(i.Base)
	if err != nil {
		return nil, errors.Join(ErrCouldNotGetBaseDeviceStat, err)
	}

	ds := &config.DeviceSchema{
		Name:      i.Name,
		BlockSize: fmt.Sprintf("%v", i.BlockSize),
		Expose:    true,
		Size:      fmt.Sprintf("%v", stat.Size()),
	}
	if strings.TrimSpace(i.Overlay) == "" || strings.TrimSpace(i.State) == "" {
		err := os.MkdirAll(filepath.Dir(i.Base), os.ModePerm)
		if err != nil {
			return nil, errors.Join(ErrCouldNotCreateDeviceDirectory, err)
		}
		ds.System = "file"
		ds.Location = i.Base
	} else {
		err := os.MkdirAll(filepath.Dir(i.Overlay), os.ModePerm)
		if err != nil {
			return nil, errors.Join(ErrCouldNotCreateOverlayDirectory, err)
		}

		err = os.MkdirAll(filepath.Dir(i.State), os.ModePerm)
		if err != nil {
			return nil, errors.Join(ErrCouldNotCreateStateDirectory, err)
		}

		ds.System = "sparsefile"
		ds.Location = i.Overlay

		ds.ROSourceShared = i.SharedBase

		ds.ROSource = &config.DeviceSchema{
			Name:     i.State,
			System:   "file",
			Location: i.Base,
			Size:     fmt.Sprintf("%v", stat.Size()),
		}
	}

	if i.S3Sync {
		ds.Sync = &config.SyncS3Schema{
			Secure:    true,
			AccessKey: i.S3AccessKey,
			SecretKey: i.S3SecretKey,
			Endpoint:  i.S3Endpoint,
			Bucket:    i.S3Bucket,
			AutoStart: true,
			Config: &config.SyncConfigSchema{
				OnlyDirty:   false,
				BlockShift:  2,
				MaxAge:      "100ms",
				MinChanged:  4,
				Limit:       256,
				CheckPeriod: "100ms",
			},
		}
		if ds.ROSourceShared {
			ds.Sync.Config.OnlyDirty = true
		}
	}
	return ds, nil
}

func CreateIncomingSiloDevSchema(i *MigrateFromDevice, schema *config.DeviceSchema) (*config.DeviceSchema, error) {
	ds := &config.DeviceSchema{
		Name:      i.Name,
		BlockSize: fmt.Sprintf("%v", i.BlockSize),
		Expose:    true,
		Size:      schema.Size,
		Sync:      schema.Sync,
	}
	if schema.Sync != nil {
		ds.Sync.AutoStart = false
	}
	if strings.TrimSpace(i.Overlay) == "" || strings.TrimSpace(i.State) == "" {
		err := os.MkdirAll(filepath.Dir(i.Base), os.ModePerm)
		if err != nil {
			return nil, errors.Join(ErrCouldNotCreateDeviceDirectory, err)
		}
		ds.System = "file"
		ds.Location = i.Base
	} else {
		err := os.MkdirAll(filepath.Dir(i.Overlay), os.ModePerm)
		if err != nil {
			return nil, errors.Join(ErrCouldNotCreateOverlayDirectory, err)
		}

		err = os.MkdirAll(filepath.Dir(i.State), os.ModePerm)
		if err != nil {
			return nil, errors.Join(ErrCouldNotCreateStateDirectory, err)
		}

		ds.System = "sparsefile"
		ds.Location = i.Overlay

		// If it was shared with us, assume it's going to be shared with future migrations
		ds.ROSourceShared = i.SharedBase

		ds.ROSource = &config.DeviceSchema{
			Name:     i.State,
			System:   "file",
			Location: i.Base,
			Size:     schema.Size,
		}
	}
	return ds, nil
}

/**
 * 'migrate' from the local filesystem.
 *
 */
func MigrateFromFS(log types.Logger, met metrics.SiloMetrics, vmpath string,
	devices []MigrateFromDevice, tweak func(index int, name string, schema *config.DeviceSchema) *config.DeviceSchema) (*devicegroup.DeviceGroup, error) {
	siloDeviceSchemas := make([]*config.DeviceSchema, 0)
	for i, input := range devices {
		if input.Shared {
			// Deal with shared devices here...
			err := ExposeSiloDeviceAsFile(vmpath, input.Name, input.Base)
			if err != nil {
				return nil, err
			}
		} else {
			ds, err := CreateSiloDevSchema(&input)
			if err != nil {
				return nil, err
			}

			if tweak != nil {
				ds = tweak(i, input.Name, ds)
			}
			siloDeviceSchemas = append(siloDeviceSchemas, ds)
		}
	}

	var slog types.Logger
	if log != nil {
		slog = log.SubLogger("silo")
	}

	// Create a silo deviceGroup from all the schemas
	dg, err := devicegroup.NewFromSchema(siloDeviceSchemas, false, slog, met)
	if err != nil {
		return nil, err
	}

	for _, input := range siloDeviceSchemas {
		dev := dg.GetExposedDeviceByName(input.Name)
		err = ExposeSiloDeviceAsFile(vmpath, input.Name, filepath.Join("/dev", dev.Device()))
		if err != nil {
			return nil, err
		}
		if log != nil {
			log.Info().
				Str("name", input.Name).
				Str("vmpath", vmpath).
				Str("device", dev.Device()).
				Msg("silo device exposed")
		}
	}
	return dg, nil
}

/**
 * Migrate FROM a pipe
 * NB: You should call dg.WaitForCompletion() later to ensure migrations are finished
 */
func MigrateFromPipe(log types.Logger, met metrics.SiloMetrics, vmpath string,
	ctx context.Context, readers []io.Reader, writers []io.Writer, schemaTweak func(index int, name string, schema *config.DeviceSchema) *config.DeviceSchema,
	cdh func([]byte)) (*devicegroup.DeviceGroup, error) {
	ready := make(chan bool)

	protocolCtx, protocolCancel := context.WithCancel(ctx)

	pro := protocol.NewRW(protocolCtx, readers, writers, nil)

	// Start a goroutine to do the protocol Handle()
	go func() {
		err := pro.Handle()
		if err != nil && !errors.Is(err, io.EOF) {
			// This deserves a warning log message, but will result in other errors being returned
			// so can be ignored here.
			if log != nil {
				log.Warn().Err(err).Msg("protocol handle error")
			}

			protocolCancel()
		}
	}()

	events := func(e *packets.Event) {}
	icdh := func(data []byte) {
		if cdh != nil {
			cdh(data)
		}
		close(ready)
	}

	var slog types.Logger
	if log != nil {
		slog = log.SubLogger("silo")
	}

	dg, err := devicegroup.NewFromProtocol(protocolCtx, pro, schemaTweak, events, icdh, slog, met)
	if err != nil {
		return nil, err
	}

	// Expose all devices
	for _, n := range dg.GetAllNames() {
		dev := dg.GetExposedDeviceByName(n)
		if dev != nil {
			err := ExposeSiloDeviceAsFile(vmpath, n, filepath.Join("/dev", dev.Device()))
			if err != nil {
				return nil, err
			}
			if log != nil {
				log.Info().
					Str("name", n).
					Str("vmpath", vmpath).
					Str("device", dev.Device()).
					Msg("silo device exposed")
			}
		}
	}

	// Wait either until EventCustomTransferAuthority or context cancelled.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ready:
	}

	return dg, nil
}

/**
 * Migrate TO a pipe
 *
 */
func MigrateToPipe(ctx context.Context, readers []io.Reader, writers []io.Writer,
	dg *devicegroup.DeviceGroup, concurrency int, onProgress func(p map[string]*migrator.MigrationProgress),
	vmState *VMStateMgr, devices []MigrateToDevice, getCustomPayload func() []byte) error {

	protocolCtx, protocolCancel := context.WithCancel(ctx)

	// Create a protocol for use by Silo
	pro := protocol.NewRW(protocolCtx, readers, writers, nil)

	// Start a goroutine to do the protocol Handle()
	go func() {
		err := pro.Handle()
		if err != nil && !errors.Is(err, io.EOF) {
			// Deserves a log, but not critical, as it'll get returned in other errors
			//			fmt.Printf("ERROR in protocol %v\n", err)
			protocolCancel()
		}
	}()

	// Start a migration to the protocol. This will send all schema info etc
	err := dg.StartMigrationTo(pro, true)
	if err != nil {
		return err
	}

	// Do the main migration of the data...
	err = dg.MigrateAll(concurrency, onProgress)
	if err != nil {
		return err
	}

	dirtyDevices := make(map[string]*DeviceStatus, 0)
	for _, d := range devices {
		dirtyDevices[d.Name] = &DeviceStatus{
			CycleThrottle:  d.CycleThrottle,
			MinCycles:      d.MinCycles,
			MaxCycles:      d.MaxCycles,
			MaxDirtyBlocks: d.MaxDirtyBlocks,
		}
	}

	// When we are ready to transfer authority, we send a single Custom Event here.
	authTransfer := func() error {
		if getCustomPayload != nil {
			cdata := getCustomPayload()
			if cdata != nil {
				return dg.SendCustomData(cdata)
			}
		}
		return dg.SendCustomData([]byte{})
	}

	dm := NewDirtyManager(vmState, dirtyDevices, authTransfer)

	err = dg.MigrateDirty(&devicegroup.MigrateDirtyHooks{
		PreGetDirty:      dm.PreGetDirty,
		PostGetDirty:     dm.PostGetDirty,
		PostMigrateDirty: dm.PostMigrateDirty,
		Completed:        func(name string) {},
	})
	if err != nil {
		return err
	}

	// Send Silo completion events for the devices. This will trigger any S3 sync behaviour etc.
	err = dg.Completed()
	if err != nil {
		return err
	}

	return nil
}
