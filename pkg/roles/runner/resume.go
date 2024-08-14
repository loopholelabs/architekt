package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/lithammer/shortuuid/v4"
	"github.com/loopholelabs/drafter/internal/firecracker"
	"github.com/loopholelabs/drafter/pkg/ipc"
	"github.com/loopholelabs/drafter/pkg/roles/packager"
	"github.com/loopholelabs/drafter/pkg/roles/snapshotter"
	"github.com/loopholelabs/drafter/pkg/utils"
	"github.com/loopholelabs/goroutine-manager/pkg/manager"
)

type ResumedRunner struct {
	Wait  func() error
	Close func() error

	snapshotLoadConfiguration SnapshotLoadConfiguration

	runner *Runner

	agent          *ipc.AgentServer
	acceptingAgent *ipc.AcceptingAgentServer

	createSnapshot func(ctx context.Context) error
}

func (runner *Runner) Resume(
	ctx context.Context,

	resumeTimeout time.Duration,
	rescueTimeout time.Duration,
	agentVSockPort uint32,

	snapshotLoadConfiguration SnapshotLoadConfiguration,
) (
	resumedRunner *ResumedRunner,

	errs error,
) {
	resumedRunner = &ResumedRunner{
		Wait:  func() error { return nil },
		Close: func() error { return nil },

		snapshotLoadConfiguration: snapshotLoadConfiguration,

		runner: runner,
	}

	runner.ongoingResumeWg.Add(1)
	defer runner.ongoingResumeWg.Done()

	var (
		suspendOnPanicWithError = false
	)

	resumedRunner.createSnapshot = func(ctx context.Context) error {
		var (
			stateCopyName  = shortuuid.New()
			memoryCopyName = shortuuid.New()
		)
		if snapshotLoadConfiguration.ExperimentalMapPrivate {
			if err := firecracker.CreateSnapshot(
				ctx,

				runner.firecrackerClient,

				// We need to write the state and memory to a separate file since we can't truncate an `mmap`ed file
				stateCopyName,
				memoryCopyName,

				firecracker.SnapshotTypeFull,
			); err != nil {
				return errors.Join(snapshotter.ErrCouldNotCreateSnapshot, err)
			}
		} else {
			if err := firecracker.CreateSnapshot(
				ctx,

				runner.firecrackerClient,

				runner.stateName,
				"",

				firecracker.SnapshotTypeMsyncAndState,
			); err != nil {
				return errors.Join(snapshotter.ErrCouldNotCreateSnapshot, err)
			}
		}

		if snapshotLoadConfiguration.ExperimentalMapPrivate {
			if err := runner.server.Close(); err != nil {
				return errors.Join(ErrCouldNotCloseServer, err)
			}

			if err := runner.Wait(); err != nil {
				return errors.Join(ErrCouldNotWaitForFirecracker, err)
			}

			for _, device := range [][3]string{
				{runner.stateName, stateCopyName, snapshotLoadConfiguration.ExperimentalMapPrivateStateOutput},
				{runner.memoryName, memoryCopyName, snapshotLoadConfiguration.ExperimentalMapPrivateMemoryOutput},
			} {
				inputFile, err := os.Open(filepath.Join(runner.server.VMPath, device[1]))
				if err != nil {
					return errors.Join(snapshotter.ErrCouldNotOpenInputFile, err)
				}
				defer inputFile.Close()

				var (
					outputPath = device[2]
					addPadding = true
				)
				if outputPath == "" {
					outputPath = filepath.Join(runner.server.VMPath, device[0])
					addPadding = false
				}

				if err := os.MkdirAll(filepath.Dir(outputPath), os.ModePerm); err != nil {
					panic(errors.Join(packager.ErrCouldNotCreateOutputDir, err))
				}

				outputFile, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.ModePerm)
				if err != nil {
					return errors.Join(packager.ErrCouldNotOpenOutputFile, err)
				}
				defer outputFile.Close()

				deviceSize, err := io.Copy(outputFile, inputFile)
				if err != nil {
					return errors.Join(snapshotter.ErrCouldNotCopyFile, err)
				}

				// We need to add a padding like the snapshotter if we're writing to a file instead of a block device
				if addPadding {
					if paddingLength := utils.GetBlockDevicePadding(deviceSize); paddingLength > 0 {
						if _, err := outputFile.Write(make([]byte, paddingLength)); err != nil {
							return errors.Join(snapshotter.ErrCouldNotWritePadding, err)
						}
					}
				}
			}
		}

		return nil
	}

	goroutineManager := manager.NewGoroutineManager(
		ctx,
		&errs,
		manager.GoroutineManagerHooks{
			OnAfterRecover: func() {
				if suspendOnPanicWithError {
					suspendCtx, cancelSuspendCtx := context.WithTimeout(runner.rescueCtx, rescueTimeout)
					defer cancelSuspendCtx()

					// Connections need to be closed before creating the snapshot
					if resumedRunner.acceptingAgent != nil {
						if e := resumedRunner.acceptingAgent.Close(); e != nil {
							errs = errors.Join(errs, snapshotter.ErrCouldNotCloseAcceptingAgent, e)
						}
					}
					if resumedRunner.agent != nil {
						resumedRunner.agent.Close()
					}

					// If a resume failed, flush the snapshot so that we can re-try
					if err := resumedRunner.createSnapshot(suspendCtx); err != nil {
						errs = errors.Join(errs, ErrCouldNotCreateRecoverySnapshot, err)
					}
				}
			},
		},
	)
	defer goroutineManager.Wait()
	defer goroutineManager.StopAllGoroutines()
	defer goroutineManager.CreateBackgroundPanicCollector()()

	// We intentionally don't call `wg.Add` and `wg.Done` here since we return the process's wait method
	// We still need to `defer handleGoroutinePanic()()` here however so that we catch any errors during this call
	goroutineManager.StartBackgroundGoroutine(func(_ context.Context) {
		if err := runner.server.Wait(); err != nil {
			panic(errors.Join(ErrCouldNotWaitForFirecracker, err))
		}
	})

	var err error
	resumedRunner.agent, err = ipc.StartAgentServer(
		filepath.Join(runner.server.VMPath, snapshotter.VSockName),
		uint32(agentVSockPort),
	)
	if err != nil {
		panic(errors.Join(snapshotter.ErrCouldNotStartAgentServer, err))
	}

	resumedRunner.Close = func() error {
		resumedRunner.agent.Close()

		return nil
	}

	if err := os.Chown(resumedRunner.agent.VSockPath, runner.hypervisorConfiguration.UID, runner.hypervisorConfiguration.GID); err != nil {
		panic(errors.Join(ErrCouldNotChownVSockPath, err))
	}

	{
		resumeSnapshotAndAcceptCtx, cancelResumeSnapshotAndAcceptCtx := context.WithTimeout(goroutineManager.Context(), resumeTimeout)
		defer cancelResumeSnapshotAndAcceptCtx()

		if err := firecracker.ResumeSnapshot(
			resumeSnapshotAndAcceptCtx,

			runner.firecrackerClient,

			runner.stateName,
			runner.memoryName,

			!snapshotLoadConfiguration.ExperimentalMapPrivate,
		); err != nil {
			panic(errors.Join(ErrCouldNotResumeSnapshot, err))
		}

		suspendOnPanicWithError = true

		resumedRunner.acceptingAgent, err = resumedRunner.agent.Accept(resumeSnapshotAndAcceptCtx, ctx)
		if err != nil {
			panic(errors.Join(ErrCouldNotAcceptAgent, err))
		}
	}

	// We intentionally don't call `wg.Add` and `wg.Done` here since we return the process's wait method
	// We still need to `defer handleGoroutinePanic()()` here however so that we catch any errors during this call
	goroutineManager.StartBackgroundGoroutine(func(_ context.Context) {
		if err := resumedRunner.acceptingAgent.Wait(); err != nil {
			panic(errors.Join(snapshotter.ErrCouldNotWaitForAcceptingAgent, err))
		}
	})

	resumedRunner.Wait = resumedRunner.acceptingAgent.Wait
	resumedRunner.Close = func() error {
		if err := resumedRunner.acceptingAgent.Close(); err != nil {
			return errors.Join(snapshotter.ErrCouldNotCloseAcceptingAgent, err)
		}

		resumedRunner.agent.Close()

		if err := resumedRunner.Wait(); err != nil {
			return errors.Join(snapshotter.ErrCouldNotWaitForAcceptingAgent, err)
		}

		return nil
	}

	{
		afterResumeCtx, cancelAfterResumeCtx := context.WithTimeout(goroutineManager.Context(), resumeTimeout)
		defer cancelAfterResumeCtx()

		if err := resumedRunner.acceptingAgent.Remote.AfterResume(afterResumeCtx); err != nil {
			panic(errors.Join(ErrCouldNotCallAfterResumeRPC, err))
		}
	}

	return
}
