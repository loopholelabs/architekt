package runner

import (
	"context"
	"errors"
	"time"
	"unsafe"

	"github.com/loopholelabs/drafter/pkg/ipc"
	"github.com/loopholelabs/drafter/pkg/snapshotter"
)

func (resumedRunner *ResumedRunner[L, R, G]) SuspendAndCloseAgentServer(ctx context.Context, suspendTimeout time.Duration) error {
	suspendCtx, cancelSuspendCtx := context.WithTimeout(ctx, suspendTimeout)
	defer cancelSuspendCtx()

	// This `nil` check is required since `SuspendAndCloseAgentServer` can be run even if a runner resume failed
	if resumedRunner.Remote != nil {
		// This is a safe type cast because R is constrained by ipc.AgentServerRemote, so this specific BeforeSuspend field
		// must be defined or there will be a compile-time error.
		// The Go Generics system can't catch this here however, it can only catch it once the type is concrete, so we need to manually cast.
		remote := *(*ipc.AgentServerRemote[G])(unsafe.Pointer(&resumedRunner.acceptingAgent.Remote))
		if err := remote.BeforeSuspend(suspendCtx); err != nil {
			return errors.Join(ErrCouldNotCallBeforeSuspendRPC, err)
		}
	}

	// Connections need to be closed before creating the snapshot
	// This `nil` check is required since `SuspendAndCloseAgentServer` can be run even if a runner resume failed
	if resumedRunner.acceptingAgent != nil {
		if err := resumedRunner.acceptingAgent.Close(); err != nil {
			return errors.Join(snapshotter.ErrCouldNotCloseAcceptingAgent, err)
		}
	}

	// This `nil` check is required since `SuspendAndCloseAgentServer` can be run even if a runner resume failed
	if resumedRunner.agent != nil {
		resumedRunner.agent.Close()
	}

	if err := resumedRunner.createSnapshot(suspendCtx); err != nil {
		return errors.Join(snapshotter.ErrCouldNotCreateSnapshot, err)
	}

	return nil
}
