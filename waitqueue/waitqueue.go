package waitqueue

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

type WaitQueue struct {
	timer           *time.Timer
	intervalTicker  *time.Ticker
	intervalCounter atomic.Int32
	sendLock        *sync.Mutex
	cancelTicker    context.CancelFunc
	done            chan struct{}
}

func New(ctx context.Context) *WaitQueue {
	ctx, cancel := context.WithCancel(ctx)
	wq := &WaitQueue{
		timer:           time.NewTimer(0),
		done:            make(chan struct{}),
		intervalTicker:  time.NewTicker(66 * time.Second),
		intervalCounter: atomic.Int32{},
		sendLock:        &sync.Mutex{},
		cancelTicker:    cancel,
	}

	go wq.runTicker(ctx)
	return wq
}

func (w *WaitQueue) runTicker(ctx context.Context) {
	defer func() { w.done <- struct{}{} }()
	for {
		select {
		case <-w.intervalTicker.C:
			w.intervalCounter.Store(0)
		case <-ctx.Done():
			return
		}
	}
}

func (w *WaitQueue) Close() {
	w.cancelTicker()
	<-w.done
}

func (w *WaitQueue) SendSingle(ctx context.Context, fn func() error) error {
	return w.SendMany(ctx, 1, fn)
}

func (w *WaitQueue) SendMany(ctx context.Context, n int32, fn func() error) error {
	<-w.timer.C
	defer w.timer.Reset(4 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := w.trySend(fn, n); nil != err {
				if errors.Is(err, errIntervalCapReached) {
					continue
				}
				return err
			}
			return nil
		}
	}
}

var errIntervalCapReached = errors.New("wait queue interval capacity has reached, waiting for next interval")

func (w *WaitQueue) trySend(fn func() error, n int32) error {
	w.sendLock.Lock()
	defer w.sendLock.Unlock()

	if c := w.intervalCounter.Load(); 20-c >= n {
		if err := fn(); nil != err {
			return err
		}
		w.intervalCounter.Add(n)
		return nil
	}
	return errIntervalCapReached
}
