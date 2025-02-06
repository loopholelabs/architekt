package mounter

import (
	"context"
	"fmt"
	"io"
	"runtime/debug"
	"sync"

	"github.com/loopholelabs/drafter/pkg/common"
	"github.com/loopholelabs/silo/pkg/storage/devicegroup"
	"github.com/loopholelabs/silo/pkg/storage/migrator"
)

type MigratableMounter struct {
	Close func()

	Dg     *devicegroup.DeviceGroup
	DgLock sync.Mutex
}

type MounterMigrateToHooks struct {
	OnBeforeGetDirtyBlocks func(deviceID uint32, remote bool)

	OnDeviceSent                       func(deviceID uint32, remote bool)
	OnDeviceAuthoritySent              func(deviceID uint32, remote bool)
	OnDeviceInitialMigrationProgress   func(deviceID uint32, remote bool, ready int, total int)
	OnDeviceContinousMigrationProgress func(deviceID uint32, remote bool, delta int)
	OnDeviceFinalMigrationProgress     func(deviceID uint32, remote bool, delta int)
	OnDeviceMigrationCompleted         func(deviceID uint32, remote bool)

	OnProgress func(p map[string]*migrator.MigrationProgress)

	OnAllDevicesSent func()
}

func (migratableMounter *MigratableMounter) MigrateTo(
	ctx context.Context,

	devices []common.MigrateToDevice,

	concurrency int,

	readers []io.Reader,
	writers []io.Writer,

	hooks MounterMigrateToHooks,
) (errs error) {
	defer func() {
		err := recover()
		if err != nil {
			fmt.Printf("ERROR in MigrateTo %v\n", err)
			debug.PrintStack()
		}
	}()

	vmStateMgr := common.NewDummyVMStateMgr(ctx)
	return common.MigrateToPipe(ctx, readers, writers, migratableMounter.Dg, concurrency, hooks.OnProgress, vmStateMgr, devices, nil)
}
