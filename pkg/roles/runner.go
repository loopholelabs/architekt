package roles

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lithammer/shortuuid/v4"
	"github.com/loopholelabs/drafter/internal/firecracker"
	"github.com/loopholelabs/drafter/pkg/ipc"
	"github.com/loopholelabs/drafter/pkg/utils"
)

const (
	VSockName = "vsock.sock"
)

type HypervisorConfiguration struct {
	FirecrackerBin string
	JailerBin      string

	ChrootBaseDir string

	UID int
	GID int

	NetNS         string
	NumaNode      int
	CgroupVersion int

	EnableOutput bool
	EnableInput  bool
}

type NetworkConfiguration struct {
	Interface string
	MAC       string
}

type VMConfiguration struct {
	CPUCount    int
	MemorySize  int
	CPUTemplate string

	BootArgs string
}

type PackageConfiguration struct {
	AgentVSockPort uint32 `json:"agentVSockPort"`
}

type Runner struct {
	VMPath string

	Wait  func() error
	Close func() error

	Resume func(
		ctx context.Context,

		resumeTimeout time.Duration,
		rescueTimeout time.Duration,
		agentVSockPort uint32,

		mapShared bool,
	) (
		resumedRunner *ResumedRunner,

		errs error,
	)
}

type ResumedRunner struct {
	Wait  func() error
	Close func() error

	Msync                      func(ctx context.Context) error
	SuspendAndCloseAgentServer func(ctx context.Context, suspendTimeout time.Duration) error
}

var (
	ErrCouldNotCreateChrootBaseDirectory = errors.New("could not create chroot base directory")
	ErrCouldNotStartFirecrackerServer    = errors.New("could not start firecracker server")
	ErrCouldNotWaitForFirecracker        = errors.New("could not wait for firecracker")
	ErrCouldNotCloseServer               = errors.New("could not close server")
	ErrCouldNotRemoveVMDir               = errors.New("could not remove VM directory")
	ErrCouldNotStartAgentServer          = errors.New("could not start agent server")
	ErrCouldNotCloseAgent                = errors.New("could not close agent")
	ErrCouldNotChownVSockPath            = errors.New("could not change ownership of vsock path")
	ErrCouldNotResumeSnapshot            = errors.New("could not resume snapshot")
	ErrCouldNotAcceptAgent               = errors.New("could not accept agent")
	ErrCouldNotWaitForAcceptingAgent     = errors.New("could not wait for accepting agent")
	ErrCouldNotCloseAcceptingAgent       = errors.New("could not close accepting agent")
	ErrCouldNotCallAfterResumeRPC        = errors.New("could not call AfterResume RPC")
	ErrCouldNotCallBeforeSuspendRPC      = errors.New("could not call BeforeSuspend RPC")
	ErrCouldNotCreateSnapshot            = errors.New("could not create snapshot")
)

func StartRunner(
	hypervisorCtx context.Context,
	rescueCtx context.Context,

	hypervisorConfiguration HypervisorConfiguration,

	stateName string,
	memoryName string,
) (
	runner *Runner,

	errs error,
) {
	runner = &Runner{}

	_, handlePanics, handleGoroutinePanics, cancel, wait, _ := utils.GetPanicHandler(
		hypervisorCtx,
		&errs,
		utils.GetPanicHandlerHooks{},
	)
	defer wait()
	defer cancel()
	defer handlePanics(false)()

	if err := os.MkdirAll(hypervisorConfiguration.ChrootBaseDir, os.ModePerm); err != nil {
		panic(errors.Join(ErrCouldNotCreateChrootBaseDirectory, err))
	}

	var ongoingResumeWg sync.WaitGroup

	firecrackerCtx, cancelFirecrackerCtx := context.WithCancel(rescueCtx) // We use `rescueContext` here since this simply intercepts `hypervisorCtx`
	// and then waits for `rescueCtx` or the rescue operation to complete
	go func() {
		<-hypervisorCtx.Done() // We use hypervisorCtx, not internalCtx here since this resource outlives the function call

		ongoingResumeWg.Wait()

		cancelFirecrackerCtx()
	}()

	server, err := firecracker.StartFirecrackerServer(
		firecrackerCtx, // We use firecrackerCtx (which depends on hypervisorCtx, not internalCtx) here since this resource outlives the function call

		hypervisorConfiguration.FirecrackerBin,
		hypervisorConfiguration.JailerBin,

		hypervisorConfiguration.ChrootBaseDir,

		hypervisorConfiguration.UID,
		hypervisorConfiguration.GID,

		hypervisorConfiguration.NetNS,
		hypervisorConfiguration.NumaNode,
		hypervisorConfiguration.CgroupVersion,

		hypervisorConfiguration.EnableOutput,
		hypervisorConfiguration.EnableInput,
	)
	if err != nil {
		panic(errors.Join(ErrCouldNotStartFirecrackerServer, err))
	}

	runner.VMPath = server.VMPath

	// We intentionally don't call `wg.Add` and `wg.Done` here since we return the process's wait method
	// We still need to `defer handleGoroutinePanic()()` here however so that we catch any errors during this call
	handleGoroutinePanics(false, func() {
		if err := server.Wait(); err != nil {
			panic(errors.Join(ErrCouldNotWaitForFirecracker, err))
		}
	})

	runner.Wait = server.Wait
	runner.Close = func() error {
		if err := server.Close(); err != nil {
			return errors.Join(ErrCouldNotCloseServer, err)
		}

		if err := runner.Wait(); err != nil {
			return errors.Join(ErrCouldNotWaitForFirecracker, err)
		}

		if err := os.RemoveAll(filepath.Dir(runner.VMPath)); err != nil {
			return errors.Join(ErrCouldNotRemoveVMDir, err)
		}

		return nil
	}

	firecrackerClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(runner.VMPath, firecracker.FirecrackerSocketName))
			},
		},
	}

	runner.Resume = func(
		ctx context.Context,

		resumeTimeout,
		rescueTimeout time.Duration,
		agentVSockPort uint32,

		mapShared bool,
	) (
		resumedRunner *ResumedRunner,

		errs error,
	) {
		resumedRunner = &ResumedRunner{}

		ongoingResumeWg.Add(1)
		defer ongoingResumeWg.Done()

		var (
			agent                   *ipc.AgentServer
			acceptingAgent          *ipc.AcceptingAgentServer
			suspendOnPanicWithError = false
		)

		internalCtx, handlePanics, handleGoroutinePanics, cancel, wait, _ := utils.GetPanicHandler(
			ctx,
			&errs,
			utils.GetPanicHandlerHooks{
				OnAfterRecover: func() {
					if suspendOnPanicWithError {
						suspendCtx, cancelSuspendCtx := context.WithTimeout(rescueCtx, rescueTimeout)
						defer cancelSuspendCtx()

						// Connections need to be closed before creating the snapshot
						if acceptingAgent != nil && acceptingAgent.Close != nil {
							if e := acceptingAgent.Close(); e != nil {
								errs = errors.Join(errs, ErrCouldNotCloseAcceptingAgent, e)
							}
						}
						if agent != nil && agent.Close != nil {
							agent.Close()
						}

						// If a resume failed, flush the snapshot so that we can re-try
						if mapShared {
							if e := firecracker.CreateSnapshot(
								suspendCtx, // We use a separate context here so that we can
								// cancel the snapshot create action independently of the resume
								// ctx, which is typically cancelled already

								firecrackerClient,

								stateName,
								memoryName,

								firecracker.SnapshotTypeFull,
							); e != nil {
								errs = errors.Join(errs, ErrCouldNotCreateSnapshot, e)

								return
							}

							return
						}

						memoryCopyName := shortuuid.New()

						if err := firecracker.CreateSnapshot(
							suspendCtx,

							firecrackerClient,

							stateName,
							memoryCopyName, // We need to write the memory to a separate file since we can't truncate an `mmap`ed file

							firecracker.SnapshotTypeFull,
						); err != nil {
							errs = errors.Join(errs, ErrCouldNotCreateSnapshot, err)

							return
						}

						inputFile, err := os.Open(filepath.Join(server.VMPath, memoryCopyName))
						if err != nil {
							errs = errors.Join(errs, ErrCouldNotOpenInputFile, err)

							return
						}
						defer inputFile.Close()

						outputFile, err := os.OpenFile(filepath.Join(server.VMPath, memoryName), os.O_TRUNC|os.O_WRONLY, os.ModePerm)
						if err != nil {
							errs = errors.Join(errs, ErrCouldNotOpenOutputFile, err)

							return
						}
						defer outputFile.Close()

						if _, err := io.Copy(outputFile, inputFile); err != nil {
							errs = errors.Join(errs, ErrCouldNotCopyFile, err)

							return
						}
					}
				},
			},
		)
		defer wait()
		defer cancel()
		defer handlePanics(false)()

		// We intentionally don't call `wg.Add` and `wg.Done` here since we return the process's wait method
		// We still need to `defer handleGoroutinePanic()()` here however so that we catch any errors during this call
		handleGoroutinePanics(false, func() {
			if err := server.Wait(); err != nil {
				panic(errors.Join(ErrCouldNotWaitForFirecracker, err))
			}
		})

		agent, err = ipc.StartAgentServer(
			filepath.Join(server.VMPath, VSockName),
			uint32(agentVSockPort),
		)
		if err != nil {
			panic(errors.Join(ErrCouldNotStartAgentServer, err))
		}

		resumedRunner.Close = func() error {
			agent.Close()

			return nil
		}

		if err := os.Chown(agent.VSockPath, hypervisorConfiguration.UID, hypervisorConfiguration.GID); err != nil {
			panic(errors.Join(ErrCouldNotChownVSockPath, err))
		}

		{
			resumeSnapshotAndAcceptCtx, cancelResumeSnapshotAndAcceptCtx := context.WithTimeout(internalCtx, resumeTimeout)
			defer cancelResumeSnapshotAndAcceptCtx()

			if err := firecracker.ResumeSnapshot(
				resumeSnapshotAndAcceptCtx,

				firecrackerClient,

				stateName,
				memoryName,
			); err != nil {
				panic(errors.Join(ErrCouldNotResumeSnapshot, err))
			}

			suspendOnPanicWithError = true

			acceptingAgent, err = agent.Accept(resumeSnapshotAndAcceptCtx, ctx)
			if err != nil {
				panic(errors.Join(ErrCouldNotAcceptAgent, err))
			}
		}

		// We intentionally don't call `wg.Add` and `wg.Done` here since we return the process's wait method
		// We still need to `defer handleGoroutinePanic()()` here however so that we catch any errors during this call
		handleGoroutinePanics(false, func() {
			if err := acceptingAgent.Wait(); err != nil {
				panic(errors.Join(ErrCouldNotWaitForAcceptingAgent, err))
			}
		})

		resumedRunner.Wait = acceptingAgent.Wait
		resumedRunner.Close = func() error {
			if err := acceptingAgent.Close(); err != nil {
				return errors.Join(ErrCouldNotCloseAcceptingAgent, err)
			}

			agent.Close()

			if err := resumedRunner.Wait(); err != nil {
				return errors.Join(ErrCouldNotWaitForAcceptingAgent, err)
			}

			return nil
		}

		{
			afterResumeCtx, cancelAfterResumeCtx := context.WithTimeout(internalCtx, resumeTimeout)
			defer cancelAfterResumeCtx()

			if err := acceptingAgent.Remote.AfterResume(afterResumeCtx); err != nil {
				panic(errors.Join(ErrCouldNotCallAfterResumeRPC, err))
			}
		}

		resumedRunner.Msync = func(ctx context.Context) error {
			if mapShared {
				if err := firecracker.CreateSnapshot(
					ctx,

					firecrackerClient,

					stateName,
					"",

					firecracker.SnapshotTypeMsync,
				); err != nil {
					return errors.Join(ErrCouldNotCreateSnapshot, err)
				}
			}

			return nil
		}

		resumedRunner.SuspendAndCloseAgentServer = func(ctx context.Context, suspendTimeout time.Duration) error {
			suspendCtx, cancelSuspendCtx := context.WithTimeout(ctx, suspendTimeout)
			defer cancelSuspendCtx()

			if err := acceptingAgent.Remote.BeforeSuspend(suspendCtx); err != nil {
				return errors.Join(ErrCouldNotCallBeforeSuspendRPC, err)
			}

			// Connections need to be closed before creating the snapshot
			if err := acceptingAgent.Close(); err != nil {
				return errors.Join(ErrCouldNotCloseAcceptingAgent, err)
			}

			agent.Close()

			if mapShared {
				if err := firecracker.CreateSnapshot(
					suspendCtx,

					firecrackerClient,

					stateName,
					"",

					firecracker.SnapshotTypeMsyncAndState,
				); err != nil {
					return errors.Join(ErrCouldNotCreateSnapshot, err)
				}

				return nil
			}

			memoryCopyName := shortuuid.New()

			if err := firecracker.CreateSnapshot(
				suspendCtx,

				firecrackerClient,

				stateName,
				memoryCopyName, // We need to write the memory to a separate file since we can't truncate an `mmap`ed file

				firecracker.SnapshotTypeFull,
			); err != nil {
				return errors.Join(ErrCouldNotCreateSnapshot, err)
			}

			inputFile, err := os.Open(filepath.Join(server.VMPath, memoryCopyName))
			if err != nil {
				return errors.Join(ErrCouldNotOpenInputFile, err)
			}
			defer inputFile.Close()

			outputFile, err := os.OpenFile(filepath.Join(server.VMPath, memoryName), os.O_TRUNC|os.O_WRONLY, os.ModePerm)
			if err != nil {
				return errors.Join(ErrCouldNotOpenOutputFile, err)
			}
			defer outputFile.Close()

			if _, err := io.Copy(outputFile, inputFile); err != nil {
				return errors.Join(ErrCouldNotCopyFile, err)
			}

			return nil
		}

		return
	}

	return
}
