//go:build linux

package jobs

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/L-K-M/dl-tool/internal/store"
)

// inotifyMask is the pair of events that count a drop as complete: a
// closed writable descriptor on a file in the directory, and a rename
// into it.
const inotifyMask = syscall.IN_CLOSE_WRITE | syscall.IN_MOVED_TO

// inotifyWake is the longest the read loop stays inside one select: the
// bounded wait is what lets the loop observe Close without a wake pipe —
// an fd number closed under a blocked select never interrupts it.
const inotifyWake = 200 * time.Millisecond

// The Linux build registers folders with inotify; every registration
// failure falls back to the polling watcher instead of failing.
func init() {
	newOSWatcher = newInotifyWatcher
}

// inotifyWatcher is the Linux registration: one inotify watch on the
// folder's directory, whose events feed the same channel the poll ticks
// feed — the sweep is what keeps the one-interval bound on NFS and CIFS
// mounts, which accept the registration but never deliver remote writes.
type inotifyWatcher struct {
	fd     int
	out    chan time.Time
	done   chan struct{}
	ticker *time.Ticker
	wg     sync.WaitGroup
	once   sync.Once
}

// newInotifyWatcher registers the folder's directory for close-write and
// moved-to events. A registration failure returns the polling watcher
// with the error — the caller logs once and still sweeps.
func newInotifyWatcher(folder store.WatchFolder) (folderWatcher, error) {
	pollFallback := func(err error) (folderWatcher, error) {
		pw, perr := newPollWatcher(folder)
		return pw, errors.Join(err, perr)
	}

	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return pollFallback(fmt.Errorf("jobs: inotify init: %w", err))
	}
	if _, err := syscall.InotifyAddWatch(fd, folder.Path, inotifyMask); err != nil {
		_ = syscall.Close(fd)
		return pollFallback(fmt.Errorf("jobs: inotify watch %s: %w", folder.Path, err))
	}

	w := &inotifyWatcher{
		fd:     fd,
		out:    make(chan time.Time, 1),
		done:   make(chan struct{}),
		ticker: time.NewTicker(pollInterval(folder)),
	}
	w.wg.Add(1)
	go w.run()

	return w, nil
}

// C fires once per inotify event and once per poll tick; the loop drains
// both into the one ScanOnce call site, so scans never overlap.
func (w *inotifyWatcher) C() <-chan time.Time { return w.out }

// Close stops the loop and releases the registration; it is idempotent.
func (w *inotifyWatcher) Close() error {
	var err error
	w.once.Do(func() {
		close(w.done)
		w.ticker.Stop()
		w.wg.Wait()
		err = syscall.Close(w.fd)
	})

	return err
}

// run forwards inotify events and poll ticks onto out until Close. The fd
// is non-blocking, so readiness is polled with a bounded select: the wake
// bounds how long Close waits for the loop, and the tick delivers the
// fallback sweep.
func (w *inotifyWatcher) run() {
	defer w.wg.Done()

	// One event buffer covers a burst; a truncated read loses only the
	// coalesced signal, which the tick re-issues anyway.
	buf := make([]byte, 64*(syscall.SizeofInotifyEvent+syscall.NAME_MAX+1))
	for {
		select {
		case <-w.done:
			return
		default:
		}

		var set syscall.FdSet
		set.Bits[w.fd/64] |= 1 << (uint(w.fd) % 64)
		tv := syscall.NsecToTimeval(int64(inotifyWake / time.Nanosecond))
		n, err := syscall.Select(w.fd+1, &set, nil, nil, &tv)
		switch {
		case err != nil && errors.Is(err, syscall.EINTR):
			// A signal only interrupted the wait; poll again.
		case err != nil:
			return
		case n > 0 && set.Bits[w.fd/64]&(1<<(uint(w.fd)%64)) != 0:
			if _, rerr := syscall.Read(w.fd, buf); rerr == nil {
				w.notify()
			} else if !errors.Is(rerr, syscall.EAGAIN) {
				return
			}
		}

		select {
		case <-w.done:
			return
		case <-w.ticker.C:
			w.notify()
		default:
		}
	}
}

// notify coalesces signals: one pending delivery is enough — ScanOnce
// re-reads the whole directory.
func (w *inotifyWatcher) notify() {
	select {
	case w.out <- time.Now():
	default:
	}
}
