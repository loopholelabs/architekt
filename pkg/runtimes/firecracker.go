package runtimes

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"time"

	"github.com/loopholelabs/drafter/pkg/common"
	"github.com/loopholelabs/drafter/pkg/ipc"
	"github.com/loopholelabs/drafter/pkg/runner"
	"github.com/loopholelabs/drafter/pkg/snapshotter"
	"github.com/loopholelabs/logging/types"
)

var ErrConfigFileNotFound = errors.New("config file not found")
var ErrCouldNotOpenConfigFile = errors.New("could not open config file")
var ErrCouldNotDecodeConfigFile = errors.New("could not decode config file")
var ErrCouldNotResumeRunner = errors.New("could not resume runner")

type FirecrackerRuntimeProvider[L ipc.AgentServerLocal, R ipc.AgentServerRemote[G], G any] struct {
	Log                     types.Logger
	Remote                  R
	Runner                  *runner.Runner[L, R, G]
	ResumedRunner           *runner.ResumedRunner[L, R, G]
	HypervisorConfiguration snapshotter.HypervisorConfiguration
	StateName               string
	MemoryName              string

	hypervisorCtx    context.Context
	hypervisorCancel context.CancelFunc
	resumeCtx        context.Context
	resumeCancel     context.CancelFunc

	AgentServerLocal L
	AgentServerHooks ipc.AgentServerAcceptHooks[R, G]

	SnapshotLoadConfiguration runner.SnapshotLoadConfiguration
}

func (rp *FirecrackerRuntimeProvider[L, R, G]) Resume(resumeTimeout time.Duration, rescueTimeout time.Duration, errChan chan error) error {
	resumeCtx, resumeCancel := context.WithCancel(context.TODO())
	rp.resumeCtx = resumeCtx
	rp.resumeCancel = resumeCancel

	// Read from the config device
	configFileData, err := os.ReadFile(path.Join(rp.DevicePath(), common.DeviceConfigName))
	if err != nil {
		return errors.Join(ErrCouldNotOpenConfigFile, err)
	}

	// Find the first 0 byte...
	firstZero := 0
	for i := 0; i < len(configFileData); i++ {
		if configFileData[i] == 0 {
			firstZero = i
			break
		}
	}
	configFileData = configFileData[:firstZero]

	var packageConfig snapshotter.PackageConfiguration
	if err := json.Unmarshal(configFileData, &packageConfig); err != nil {
		return errors.Join(ErrCouldNotDecodeConfigFile, err)
	}

	rp.ResumedRunner, err = rp.Runner.Resume(
		resumeCtx,
		resumeTimeout,
		rescueTimeout,
		packageConfig.AgentVSockPort,
		rp.AgentServerLocal,
		rp.AgentServerHooks,
		rp.SnapshotLoadConfiguration,
	)
	if err != nil {
		return err
	}

	rp.Remote = *rp.ResumedRunner.Remote

	go func() {
		err := rp.ResumedRunner.Wait()
		if err != nil {
			select {
			case errChan <- err:
			default:
			}
		}
	}()

	return nil
}

func (rp *FirecrackerRuntimeProvider[L, R, G]) Start(ctx context.Context, rescueCtx context.Context, errChan chan error) error {
	hypervisorCtx, hypervisorCancel := context.WithCancel(context.TODO())

	rp.hypervisorCtx = hypervisorCtx
	rp.hypervisorCancel = hypervisorCancel

	run, err := runner.StartRunner[L, R](
		hypervisorCtx,
		rescueCtx,
		rp.HypervisorConfiguration,
		rp.StateName,
		rp.MemoryName,
	)
	if err == nil {
		rp.Runner = run
		go func() {
			err := run.Wait()
			if err != nil {
				select {
				case errChan <- err:
				default:
				}
			}
		}()
	}
	return err
}

func (rp *FirecrackerRuntimeProvider[L, R, G]) FlushData(ctx context.Context) error {
	return rp.ResumedRunner.Msync(ctx)
}

func (rp *FirecrackerRuntimeProvider[L, R, G]) DevicePath() string {
	return rp.Runner.VMPath
}

func (rp *FirecrackerRuntimeProvider[L, R, G]) GetVMPid() int {
	return rp.Runner.VMPid
}

func (rp *FirecrackerRuntimeProvider[L, R, G]) Close() error {
	// TODO: Correct?
	if rp.ResumedRunner != nil {
		if rp.Log != nil {
			rp.Log.Debug().Msg("Closing resumed runner")
		}

		err := rp.Suspend(context.TODO(), time.Minute) // TODO

		rp.resumeCancel() // We can cancel this context now
		rp.hypervisorCancel()

		err = rp.ResumedRunner.Close()
		if err != nil {
			return err
		}
		err = rp.ResumedRunner.Wait()
		if err != nil {
			return err
		}
	} else if rp.Runner != nil {
		if rp.Log != nil {
			rp.Log.Debug().Msg("Closing runner")
		}
		err := rp.Runner.Close()
		if err != nil {
			return err
		}
		err = rp.Runner.Wait()
		if err != nil {
			return err
		}
	}
	return nil
}

func (rp *FirecrackerRuntimeProvider[L, R, G]) Suspend(ctx context.Context, resumeTimeout time.Duration) error {
	if rp.Log != nil {
		rp.Log.Debug().Msg("resumedPeer.SuspendAndCloseAgentServer")
	}
	err := rp.ResumedRunner.SuspendAndCloseAgentServer(
		ctx,
		resumeTimeout,
	)
	if err != nil {
		if rp.Log != nil {
			rp.Log.Warn().Err(err).Msg("error from resumedPeer.SuspendAndCloseAgentServer")
		}
	}
	return err
}
