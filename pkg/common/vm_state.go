package common

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrCouldNotSuspendAndCloseAgentServer = errors.New("could not suspend and close agent server")
	ErrCouldNotMsyncRunner                = errors.New("could not msync runner")
)

type VMStateMgr struct {
	ctx             context.Context
	suspendedLock   sync.Mutex
	suspended       bool
	suspendedCh     chan struct{}
	onBeforeSuspend func()
	onAfterSuspend  func()
	suspendFunc     func(ctx context.Context, timeout time.Duration) error
	msyncFunc       func(ctx context.Context) error
	suspendTimeout  time.Duration
}

func NewVMStateMgr(ctx context.Context,
	suspendFunc func(ctx context.Context, timeout time.Duration) error,
	suspendTimeout time.Duration,
	msyncFunc func(ctx context.Context) error,
	onBeforeSuspend func(),
	onAfterSuspend func()) *VMStateMgr {
	return &VMStateMgr{
		ctx:             ctx,
		suspendFunc:     suspendFunc,
		suspendTimeout:  suspendTimeout,
		msyncFunc:       msyncFunc,
		onBeforeSuspend: onBeforeSuspend,
		onAfterSuspend:  onAfterSuspend,
		suspendedCh:     make(chan struct{}),
	}
}

func NewDummyVMStateMgr(ctx context.Context) *VMStateMgr {
	return &VMStateMgr{
		ctx:             ctx,
		suspendFunc:     func(context.Context, time.Duration) error { return nil },
		suspendTimeout:  10 * time.Second,
		msyncFunc:       func(context.Context) error { return nil },
		onBeforeSuspend: func() {},
		onAfterSuspend:  func() {},
		suspendedCh:     make(chan struct{}),
	}
}

func (sm *VMStateMgr) GetSuspsnededVMCh() chan struct{} {
	return sm.suspendedCh
}

func (sm *VMStateMgr) CheckSuspendedVM() bool {
	sm.suspendedLock.Lock()
	defer sm.suspendedLock.Unlock()
	return sm.suspended
}

func (sm *VMStateMgr) SuspendAndMsync() error {
	sm.suspendedLock.Lock()
	defer sm.suspendedLock.Unlock()
	if sm.suspended {
		return nil
	}

	if sm.onBeforeSuspend != nil {
		sm.onBeforeSuspend()
	}

	err := sm.suspendFunc(sm.ctx, sm.suspendTimeout)
	if err != nil {
		return errors.Join(ErrCouldNotSuspendAndCloseAgentServer, err)
	}

	err = sm.msyncFunc(sm.ctx)
	if err != nil {
		return errors.Join(ErrCouldNotMsyncRunner, err)
	}

	if sm.onAfterSuspend != nil {
		sm.onAfterSuspend()
	}

	sm.suspended = true
	close(sm.suspendedCh)
	return nil
}

func (sm *VMStateMgr) Msync() error {
	return sm.msyncFunc(sm.ctx)
}
