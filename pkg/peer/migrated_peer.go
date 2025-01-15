package peer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/loopholelabs/drafter/pkg/ipc"
	"github.com/loopholelabs/drafter/pkg/packager"
	"github.com/loopholelabs/drafter/pkg/runner"
	"github.com/loopholelabs/drafter/pkg/snapshotter"
	"github.com/loopholelabs/silo/pkg/storage/devicegroup"
)

type MigratedPeer[L ipc.AgentServerLocal, R ipc.AgentServerRemote[G], G any] struct {
	Wait  func() error
	Close func() error

	DgLock sync.Mutex
	Dg     *devicegroup.DeviceGroup

	devices []MigrateFromDevice
	runner  *runner.Runner[L, R, G]
}

func (migratedPeer *MigratedPeer[L, R, G]) Resume(
	ctx context.Context,

	resumeTimeout,
	rescueTimeout time.Duration,

	agentServerLocal L,
	agentServerHooks ipc.AgentServerAcceptHooks[R, G],

	snapshotLoadConfiguration runner.SnapshotLoadConfiguration,
) (resumedPeer *ResumedPeer[L, R, G], errs error) {
	resumedPeer = &ResumedPeer[L, R, G]{
		Dg: migratedPeer.Dg,
		Wait: func() error {
			return nil
		},
		Close: func() error {
			return nil
		},
	}

	migratedPeer.DgLock.Lock()
	packageConfigDevice := migratedPeer.Dg.GetExposedDeviceByName(packager.ConfigName)
	if packageConfigDevice == nil || packageConfigDevice.Device() == "" {
		migratedPeer.DgLock.Unlock()

		return nil, ErrConfigFileNotFound
	}
	packageConfigPath := filepath.Join("/dev", packageConfigDevice.Device())
	migratedPeer.DgLock.Unlock()

	packageConfigFile, err := os.Open(packageConfigPath)
	if err != nil {
		return nil, errors.Join(ErrCouldNotOpenConfigFile, err)
	}
	defer packageConfigFile.Close()

	var packageConfig snapshotter.PackageConfiguration
	if err := json.NewDecoder(packageConfigFile).Decode(&packageConfig); err != nil {
		return nil, errors.Join(ErrCouldNotDecodeConfigFile, err)
	}

	resumedPeer.resumedRunner, err = migratedPeer.runner.Resume(
		ctx,

		resumeTimeout,
		rescueTimeout,
		packageConfig.AgentVSockPort,

		agentServerLocal,
		agentServerHooks,

		snapshotLoadConfiguration,
	)
	if err != nil {
		return nil, errors.Join(ErrCouldNotResumeRunner, err)
	}
	resumedPeer.Remote = resumedPeer.resumedRunner.Remote

	resumedPeer.Wait = resumedPeer.resumedRunner.Wait
	resumedPeer.Close = resumedPeer.resumedRunner.Close

	return resumedPeer, nil
}
