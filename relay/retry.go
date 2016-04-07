package relay

import (
	"errors"
	"sync"
	"time"
)

type Operation func() error

// Buffers and retries operations, if the buffer is full operations are dropped.
// Only tries one operation at a time, the next operation is not attempted
// until success or timeout of the previous operation.
// There is no delay between attempts of different operations.
type retryBuffer struct {
	buf chan retryOperation

	initialInterval time.Duration
	multiplier      time.Duration
	maxInterval     time.Duration

	wg sync.WaitGroup
}

func newRetryBuffer(size int, initial, multiplier, max time.Duration) *retryBuffer {
	r := &retryBuffer{
		buf:             make(chan retryOperation, size),
		initialInterval: initial,
		multiplier:      multiplier,
		maxInterval:     max,
	}
	r.wg.Add(1)
	go r.run()
	return r
}

// Stops the retryBuffer.
// Subsequent calls to Rety will panic.
// Actions currently in the buffer will still be tried.
// Blocks until buffer is empty.
func (r *retryBuffer) Stop() {
	close(r.buf)
	r.wg.Wait()
}

type retryOperation struct {
	op   Operation
	errC chan error
}

// Retry an operation, it is expected that one attempt has already
// been made as this sleeps before trying again.
func (r *retryBuffer) Retry(op Operation) error {
	retry := retryOperation{
		op:   op,
		errC: make(chan error),
	}

	select {
	case r.buf <- retry:
		return <-retry.errC
	default:
		return errors.New("buffer full cannot retry")
	}
}

func (r *retryBuffer) run() {
	defer r.wg.Done()
	for retry := range r.buf {
		interval := r.initialInterval
		for {
			if err := retry.op(); err == nil {
				retry.errC <- nil
				break
			}
			interval *= r.multiplier
			if interval > r.maxInterval {
				interval = r.maxInterval
			}
			time.Sleep(interval)
		}
	}
}
