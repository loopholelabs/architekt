package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/loopholelabs/goroutine-manager/pkg/manager"
	"github.com/pojntfx/panrpc/go/pkg/rpc"
)

var (
	ErrNoRemoteFound                     = errors.New("no remote found")
	ErrAgentClientDisconnected           = errors.New("agent client disconnected")
	ErrCouldNotAcceptAgentClient         = errors.New("could not accept agent client")
	ErrCouldNotListenInAgentServer       = errors.New("could not listen in agent server")
	ErrCouldNotCloseAcceptingAgentServer = errors.New("could not close accepting agent server")
	ErrCouldNotLinkRegistry              = errors.New("could not link registry")
)

// The RPCs the agent client can call on this server
// See https://github.com/pojntfx/panrpc/tree/main?tab=readme-ov-file#5-calling-the-clients-rpcs-from-the-server
type AgentServerLocal any

// The RPCs this server can call on the agent client
// See https://github.com/pojntfx/panrpc/tree/main?tab=readme-ov-file#4-calling-the-servers-rpcs-from-the-client
type AgentServerRemote[G any] struct {
	GuestService G

	BeforeSuspend func(ctx context.Context) error
	AfterResume   func(ctx context.Context) error
}

type AgentServer[L AgentServerLocal, R AgentServerRemote[G], G any] struct {
	VSockPath string

	Close func()

	lis net.Listener

	closed    bool
	closeLock sync.Mutex

	agentServerLocal L
}

func StartAgentServer[L AgentServerLocal, R AgentServerRemote[G], G any](
	vsockPath string,
	vsockPort uint32,

	agentServerLocal L,
) (
	agentServer *AgentServer[L, R, G],

	err error,
) {
	agentServer = &AgentServer[L, R, G]{
		Close: func() {},

		agentServerLocal: agentServerLocal,
	}

	agentServer.VSockPath = fmt.Sprintf("%s_%d", vsockPath, vsockPort)

	agentServer.lis, err = net.Listen("unix", agentServer.VSockPath)
	if err != nil {
		return nil, errors.Join(ErrCouldNotListenInAgentServer, err)
	}

	agentServer.Close = func() {
		agentServer.closeLock.Lock()
		defer agentServer.closeLock.Unlock()

		agentServer.closed = true

		// We need to remove this file first so that the client can't try to reconnect
		_ = os.Remove(agentServer.VSockPath) // We ignore errors here since the file might already have been removed, but we don't want to use `RemoveAll` cause it could remove a directory

		_ = agentServer.lis.Close() // We ignore errors here since we might interrupt a network connection
	}

	return
}

type AgentServerAcceptHooks[R AgentServerRemote[G], G any] struct {
	OnAfterRegistrySetup func(forRemotes func(cb func(remoteID string, remote R) error) error) error
}

type AcceptingAgentServer[L AgentServerLocal, R AgentServerRemote[G], G any] struct {
	Remote R

	Wait  func() error
	Close func() error
}

func (agentServer *AgentServer[L, R, G]) Accept(
	acceptCtx context.Context,
	remoteCtx context.Context,

	hooks AgentServerAcceptHooks[R, G],
) (acceptingAgentServer *AcceptingAgentServer[L, R, G], errs error) {
	acceptingAgentServer = &AcceptingAgentServer[L, R, G]{
		Wait: func() error {
			return nil
		},
		Close: func() error {
			return nil
		},
	}

	goroutineManager := manager.NewGoroutineManager(
		acceptCtx,
		&errs,
		manager.GoroutineManagerHooks{},
	)
	defer goroutineManager.Wait()
	defer goroutineManager.StopAllGoroutines()
	defer goroutineManager.CreateBackgroundPanicCollector()()

	var (
		ready       = make(chan struct{})
		signalReady = sync.OnceFunc(func() {
			close(ready) // We can safely close() this channel since the caller only runs once/is `sync.OnceFunc`d
		})
	)
	// This goroutine will not leak on function return because it selects on `goroutineManager.Context().Done()` internally
	goroutineManager.StartBackgroundGoroutine(func(ctx context.Context) {
		select {
		// Failure case; something failed and the goroutineManager.Context() was cancelled before we got a connection
		case <-ctx.Done():
			agentServer.Close() // We ignore errors here since we might interrupt a network connection

		// Happy case; we've got a connection and we want to wait with closing the agent's connections until the context, not the internal context is cancelled
		// We don't do anything here because the `acceptingAgent` context handler must close in order
		case <-ready:
			break
		}
	})

	conn, err := agentServer.lis.Accept()
	if err != nil {
		agentServer.closeLock.Lock()
		defer agentServer.closeLock.Unlock()

		if agentServer.closed && errors.Is(err, net.ErrClosed) { // Don't treat closed errors as errors if we closed the connection
			if err := goroutineManager.Context().Err(); err != nil {
				panic(err)
			}

			return
		}

		panic(errors.Join(ErrCouldNotAcceptAgentClient, err))
	}

	linkCtx, cancelLinkCtx := context.WithCancelCause(remoteCtx) // This resource outlives the current scope, so we use the external context

	acceptingAgentServer.Close = func() error {
		agentServer.closeLock.Lock()

		agentServer.closed = true

		cancelLinkCtx(goroutineManager.GetErrGoroutineStopped())

		agentServer.closeLock.Unlock()

		return acceptingAgentServer.Wait()
	}

	// It is safe to start a background goroutine here since we return a wait function
	// Despite returning a wait function, we still need to start this goroutine however so that any errors
	// we get as we're waiting for a connection are caught
	// It's important that we start this _after_ calling `cmd.Start`, otherwise our process would be nil
	goroutineManager.StartBackgroundGoroutine(func(ctx context.Context) {
		select {
		// Failure case; something failed and the goroutineManager.Context() was cancelled before we got a connection
		case <-ctx.Done():

			// Happy case; we've got a connection and we want to wait with closing the agent's connections until the context, not the internal context is cancelled
			select {
			case <-ready:
				<-remoteCtx.Done() // Wait here...
			default:
			}

			if err := acceptingAgentServer.Close(); err != nil {
				panic(errors.Join(ErrCouldNotCloseAcceptingAgentServer, err))
			}
			agentServer.Close() // We ignore errors here since we might interrupt a network connection
		}
	})

	registry := rpc.NewRegistry[R, json.RawMessage](
		agentServer.agentServerLocal,

		&rpc.RegistryHooks{
			OnClientConnect: func(remoteID string) {
				signalReady()
			},
		},
	)

	if hook := hooks.OnAfterRegistrySetup; hook != nil {
		hook(registry.ForRemotes)
	}

	acceptingAgentServer.Wait = sync.OnceValue(func() error {
		// We don't `defer conn.Close` here since Firecracker handles resetting active VSock connections for us
		defer cancelLinkCtx(nil)

		encoder := json.NewEncoder(conn)
		decoder := json.NewDecoder(conn)

		if err := registry.LinkStream(
			linkCtx,

			func(v rpc.Message[json.RawMessage]) error {
				return encoder.Encode(v)
			},
			func(v *rpc.Message[json.RawMessage]) error {
				return decoder.Decode(v)
			},

			func(v any) (json.RawMessage, error) {
				b, err := json.Marshal(v)
				if err != nil {
					return nil, err
				}

				return json.RawMessage(b), nil
			},
			func(data json.RawMessage, v any) error {
				return json.Unmarshal([]byte(data), v)
			},

			nil,
		); err != nil {
			agentServer.closeLock.Lock()
			defer agentServer.closeLock.Unlock()

			// Don't treat closed errors as errors if we closed the connection
			if !(agentServer.closed && errors.Is(err, context.Canceled) && errors.Is(context.Cause(goroutineManager.Context()), goroutineManager.GetErrGoroutineStopped())) {
				return errors.Join(ErrAgentClientDisconnected, ErrCouldNotLinkRegistry, err)
			}

			return remoteCtx.Err()
		}

		return nil
	})

	// It is safe to start a background goroutine here since we return a wait function
	// Despite returning a wait function, we still need to start this goroutine however so that any errors
	// we get as we're waiting for a connection are caught
	goroutineManager.StartBackgroundGoroutine(func(_ context.Context) {
		if err := acceptingAgentServer.Wait(); err != nil {
			panic(err)
		}
	})

	select {
	case <-goroutineManager.Context().Done():
		if err := goroutineManager.Context().Err(); err != nil {
			panic(err)
		}

		return
	case <-ready:
		break
	}

	found := false
	if err := registry.ForRemotes(func(remoteID string, r R) error {
		acceptingAgentServer.Remote = r
		found = true

		return nil
	}); err != nil {
		panic(err)
	}

	if !found {
		panic(ErrNoRemoteFound)
	}

	return
}
