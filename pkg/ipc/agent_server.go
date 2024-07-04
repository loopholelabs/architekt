package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/loopholelabs/drafter/internal/remotes"
	"github.com/loopholelabs/drafter/pkg/utils"
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

type AgentServer struct {
	VSockPath string

	Accept func(
		acceptCtx context.Context,
		remoteCtx context.Context,
	) (
		acceptingAgent *AcceptingAgentServer,

		errs error,
	)

	Close func()
}

type AcceptingAgentServer struct {
	Remote remotes.AgentRemote

	Wait  func() error
	Close func() error
}

func StartAgentServer(
	vsockPath string,
	vsockPort uint32,
) (
	agentServer *AgentServer,

	err error,
) {
	agentServer = &AgentServer{}

	agentServer.VSockPath = fmt.Sprintf("%s_%d", vsockPath, vsockPort)

	lis, err := net.Listen("unix", agentServer.VSockPath)
	if err != nil {
		return nil, errors.Join(ErrCouldNotListenInAgentServer, err)
	}

	var closeLock sync.Mutex
	closed := false

	agentServer.Close = func() {
		closeLock.Lock()
		defer closeLock.Unlock()

		closed = true

		// We need to remove this file first so that the client can't try to reconnect
		_ = os.Remove(agentServer.VSockPath) // We ignore errors here since the file might already have been removed, but we don't want to use `RemoveAll` cause it could remove a directory

		_ = lis.Close() // We ignore errors here since we might interrupt a network connection
	}

	agentServer.Accept = func(acceptCtx context.Context, remoteCtx context.Context) (acceptingAgentServer *AcceptingAgentServer, errs error) {
		acceptingAgentServer = &AcceptingAgentServer{}

		// We use the background context here instead of the internal context because we want to distinguish
		// between a context cancellation from the outside and getting a response
		readyCtx, cancelReadyCtx := context.WithCancel(context.Background())
		defer cancelReadyCtx()

		internalCtx, handlePanics, handleGoroutinePanics, cancel, wait, _ := utils.GetPanicHandler(
			acceptCtx,
			&errs,
			utils.GetPanicHandlerHooks{},
		)
		defer wait()
		defer cancel()
		defer handlePanics(false)()

		// We intentionally don't call `wg.Add` and `wg.Done` here - we are ok with leaking this
		// goroutine since we return a `Wait()` function
		go func() {
			select {
			// Failure case; we cancelled the internal context before we got a connection
			case <-internalCtx.Done():
				agentServer.Close() // We ignore errors here since we might interrupt a network connection

			// Happy case; we've got a connection and we want to wait with closing the agent's connections until the context, not the internal context is cancelled
			// We don't do anything here because the `acceptingAgent` context handler must close in order
			case <-readyCtx.Done():
				break
			}
		}()

		conn, err := lis.Accept()
		if err != nil {
			closeLock.Lock()
			defer closeLock.Unlock()

			if closed && errors.Is(err, net.ErrClosed) { // Don't treat closed errors as errors if we closed the connection
				if err := internalCtx.Err(); err != nil {
					panic(internalCtx.Err())
				}

				return
			}

			panic(errors.Join(ErrCouldNotAcceptAgentClient, err))
		}

		acceptingAgentServer.Close = func() error {
			closeLock.Lock()

			closed = true

			_ = conn.Close() // We ignore errors here since we might interrupt a network connection

			closeLock.Unlock()

			if acceptingAgentServer.Wait != nil {
				return acceptingAgentServer.Wait()
			}

			return nil
		}

		// We intentionally don't call `wg.Add` and `wg.Done` here - we are ok with leaking this
		// goroutine since we return a `Wait()` function.
		// We still need to `defer handleGoroutinePanic()()` however so that
		// if we cancel the context during this call, we still handle it appropriately
		handleGoroutinePanics(false, func() {
			select {
			// Failure case; we cancelled the internal context before we got a connection
			case <-internalCtx.Done():
				if err := acceptingAgentServer.Close(); err != nil {
					panic(errors.Join(ErrCouldNotCloseAcceptingAgentServer, err))
				}
				agentServer.Close() // We ignore errors here since we might interrupt a network connection

			// Happy case; we've got a connection and we want to wait with closing the agent's connections until the context, not the internal context is cancelled
			case <-readyCtx.Done():
				<-remoteCtx.Done()

				if err := acceptingAgentServer.Close(); err != nil {
					panic(errors.Join(ErrCouldNotCloseAcceptingAgentServer, err))
				}
				agentServer.Close() // We ignore errors here since we might interrupt a network connection

				break
			}
		})

		registry := rpc.NewRegistry[remotes.AgentRemote, json.RawMessage](
			&struct{}{},

			remoteCtx, // This resource outlives the current scope, so we use the external context

			&rpc.RegistryHooks{
				OnClientConnect: func(remoteID string) {
					cancelReadyCtx()
				},
			},
		)

		acceptingAgentServer.Wait = sync.OnceValue(func() error {
			encoder := json.NewEncoder(conn)
			decoder := json.NewDecoder(conn)

			if err := registry.LinkStream(
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
				closeLock.Lock()
				defer closeLock.Unlock()

				if closed && errors.Is(err, net.ErrClosed) { // Don't treat closed errors as errors if we closed the connection
					return remoteCtx.Err()
				}

				return errors.Join(ErrAgentClientDisconnected, ErrCouldNotLinkRegistry, err)
			}

			return nil
		})

		// We intentionally don't call `wg.Add` and `wg.Done` here - we are ok with leaking this
		// goroutine since we return the process, which allows tracking errors and stopping this goroutine
		// and waiting for it to be stopped. We still need to `defer handleGoroutinePanic()()` however so that
		// any errors we get as we're polling the socket path directory are caught
		// It's important that we start this _after_ calling `cmd.Start`, otherwise our process would be nil
		handleGoroutinePanics(false, func() {
			if err := acceptingAgentServer.Wait(); err != nil {
				panic(err)
			}
		})

		select {
		case <-internalCtx.Done():
			if err := internalCtx.Err(); err != nil {
				panic(internalCtx.Err())
			}

			return
		case <-readyCtx.Done():
			break
		}

		found := false
		if err := registry.ForRemotes(func(remoteID string, r remotes.AgentRemote) error {
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

	return
}
