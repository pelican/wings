package system

import (
	"context"
	"sync"

	"emperror.dev/errors"
)

var ErrLockerLocked = errors.Sentinel("locker: cannot acquire lock, already locked")

type Locker struct {
	mu sync.RWMutex
	ch chan bool
	// done is closed when the locker is destroyed, waking anything waiting in
	// TryAcquire. The lock channel itself is never closed, as a send on a closed
	// channel panics, and acquiring can race with destroying the locker when a
	// server is deleted.
	done      chan struct{}
	destroyed bool
}

// NewLocker returns a new Locker instance.
func NewLocker() *Locker {
	return &Locker{
		ch:   make(chan bool, 1),
		done: make(chan struct{}),
	}
}

// IsLocked returns the current state of the locker channel. If there is
// currently a value in the channel, it is assumed to be locked.
func (l *Locker) IsLocked() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.ch) == 1
}

// Acquire will acquire the power lock if it is not currently locked. If it is
// already locked, acquire will fail to acquire the lock, and will return false.
// A destroyed locker can never be acquired.
func (l *Locker) Acquire() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.destroyed {
		return ErrLockerLocked
	}
	select {
	case l.ch <- true:
	default:
		return ErrLockerLocked
	}
	return nil
}

// TryAcquire will attempt to acquire a power-lock until the context provided
// is canceled or the locker is destroyed.
func (l *Locker) TryAcquire(ctx context.Context) error {
	select {
	case l.ch <- true:
		// The locker may have been destroyed while we were waiting, a destroyed
		// locker must never be handed out.
		l.mu.RLock()
		destroyed := l.destroyed
		l.mu.RUnlock()
		if destroyed {
			l.Release()
			return ErrLockerLocked
		}
		return nil
	case <-l.done:
		return ErrLockerLocked
	case <-ctx.Done():
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return ErrLockerLocked
			}
		}
		return nil
	}
}

// Release will drain the locker channel so that we can properly re-acquire it
// at a later time. If the channel is not currently locked this function is a
// no-op and will immediately return.
func (l *Locker) Release() {
	l.mu.Lock()
	select {
	case <-l.ch:
	default:
	}
	l.mu.Unlock()
}

// Destroy releases the power locker and stops it from being acquired again.
// Anything waiting in TryAcquire returns ErrLockerLocked. It is safe to call
// Destroy more than once, and to call it while other goroutines are acquiring
// or releasing the lock.
func (l *Locker) Destroy() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.destroyed {
		return
	}
	l.destroyed = true
	select {
	case <-l.ch:
	default:
	}
	close(l.done)
}
