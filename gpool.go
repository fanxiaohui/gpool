// MIT License
//
// Copyright (c) 2019 jiang
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

// Package gpool Implementing a goroutine pool
package gpool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// default config parameter
const (
	DefaultCapacity     = 100000
	DefaultSurvivalTime = 1 * time.Second
	DefaultCleanupTime  = 10 * time.Second
	miniCleanupTime     = 100 * time.Millisecond
)

// define pool state
const (
	onWork = iota
	closed
)

var (
	// ErrClosed indicate the pool has closed
	ErrClosed = errors.New("pool has closed")
	// ErrInvalidTaskFunc indicate the task function is invalid
	ErrInvalidTaskFunc = errors.New("invalid function, must be not nil")
	// ErrOverload indicate the goroutine overload
	ErrOverload = errors.New("pool overload")
	// ErrInvalidTask indicate the task is invalid
	ErrInvalidTask = errors.New("invalid task, must be not nil")
)

// Pool the goroutine pool
type Pool struct {
	ctx    context.Context
	cancel context.CancelFunc

	capacity        int32 // goroutines capacity
	running         int32 // goroutines running count
	survivalTime    time.Duration
	miniCleanupTime time.Duration // mini cleanup time

	closeDone uint32

	mux            sync.Mutex
	cond           *sync.Cond
	idleGoRoutines *list // idle go routine list
	cache          *sync.Pool
	wg             sync.WaitGroup

	panicFunc func()
}

// New new a pool with the config if there is ,other use default config
func New(opt ...Option) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		ctx:    ctx,
		cancel: cancel,

		capacity:        DefaultCapacity,
		survivalTime:    DefaultSurvivalTime,
		miniCleanupTime: DefaultCleanupTime,

		idleGoRoutines: newList(),
	}
	p.cond = sync.NewCond(&p.mux)
	p.cache = &sync.Pool{
		New: func() interface{} { return &work{task: make(chan Task, 1), pool: p} },
	}

	for _, f := range opt {
		f(p)
	}

	if p.capacity < 0 {
		p.capacity = DefaultCapacity
	}
	if p.miniCleanupTime < miniCleanupTime {
		p.miniCleanupTime = miniCleanupTime
	}

	go p.cleanUp()
	return p
}

func (sf *Pool) cleanUp() {
	tick := time.NewTimer(sf.survivalTime)
	defer tick.Stop()

	for {
		select {
		case <-tick.C:
			nearTimeout := sf.survivalTime
			now := time.Now()
			sf.mux.Lock()
			var next *work
			for e := sf.idleGoRoutines.Front(); e != nil; e = next {
				if nearTimeout = now.Sub(e.markTime); nearTimeout < sf.survivalTime {
					break
				}
				next = e.Next() // save before delete
				sf.idleGoRoutines.remove(e).task <- nil
			}
			sf.mux.Unlock()
			if nearTimeout < sf.miniCleanupTime {
				nearTimeout = sf.miniCleanupTime
			}
			tick.Reset(nearTimeout)
		case <-sf.ctx.Done():
			sf.mux.Lock()
			for e := sf.idleGoRoutines.Front(); e != nil; e = e.Next() {
				e.task <- nil // give a nil function, make all goroutine exit
			}
			sf.idleGoRoutines = nil
			sf.mux.Unlock()
			return
		}
	}
}

// SetPanicHandler set panic handler
func (sf *Pool) SetPanicHandler(f func()) {
	sf.panicFunc = f
}

// Len returns the currently running goroutines
func (sf *Pool) Len() int {
	return int(atomic.LoadInt32(&sf.running))
}

// Cap tha capacity of goroutines the pool can create
func (sf *Pool) Cap() int {
	return int(atomic.LoadInt32(&sf.capacity))
}

// Adjust adjust the capacity of the pools goroutines
func (sf *Pool) Adjust(size int) {
	if size < 0 || sf.Cap() == size {
		return
	}
	atomic.StoreInt32(&sf.capacity, int32(size))
}

// Free return the available goroutines can create
func (sf *Pool) Free() int {
	return sf.Cap() - sf.Len()
}

// Idle return the goroutines has running but in idle(no task work)
func (sf *Pool) Idle() int {
	var cnt int
	sf.mux.Lock()
	if sf.idleGoRoutines != nil {
		cnt = sf.idleGoRoutines.Len()
	}
	sf.mux.Unlock()
	return cnt
}

// Close the pool,if grace enable util all goroutine close
func (sf *Pool) close(grace bool) error {
	if atomic.LoadUint32(&sf.closeDone) == closed {
		return nil
	}

	sf.mux.Lock()
	if sf.closeDone == onWork { // check again,make sure
		sf.cancel()
		atomic.StoreUint32(&sf.closeDone, closed)
	}
	sf.mux.Unlock()
	if grace {
		sf.wg.Wait()
	}
	return nil
}

// Close the pool,but not wait all goroutine close
func (sf *Pool) Close() error {
	return sf.close(false)
}

// CloseGrace the pool,wait util all goroutine close
func (sf *Pool) CloseGrace() error {
	return sf.close(true)
}

// SubmitFunc submits a task function
func (sf *Pool) SubmitFunc(f TaskFunc) error {
	if f == nil {
		return ErrInvalidTaskFunc
	}
	return sf.Submit(f)
}

// Submit submit a task
func (sf *Pool) Submit(job Task) error {
	var w *work

	if job == nil {
		return ErrInvalidTask
	}

	if atomic.LoadUint32(&sf.closeDone) == closed {
		return ErrClosed
	}

	sf.mux.Lock()
	if sf.closeDone == closed || sf.idleGoRoutines == nil { // check again,make sure
		sf.mux.Unlock()
		return ErrClosed
	}

	if w = sf.idleGoRoutines.Front(); w != nil {
		sf.idleGoRoutines.Remove(w)
		sf.mux.Unlock()
		w.task <- job
		return nil
	}

	// actual goroutines maybe greater than cap, when race, but it will overload and return to normal in goroutine
	if sf.Free() > 0 {
		sf.mux.Unlock()
		w = sf.cache.Get().(*work)
		w.task <- job
		w.run()
		return nil
	}

	for {
		sf.cond.Wait()
		if w = sf.idleGoRoutines.Front(); w != nil {
			sf.idleGoRoutines.Remove(w)
			break
		}
	}
	sf.mux.Unlock()
	w.task <- job
	return nil
}

// push the running goroutine to idle pool
func (sf *Pool) push(w *work) error {
	if atomic.LoadUint32(&sf.closeDone) == closed { // quick check
		return ErrClosed
	}

	if sf.Free() < 0 {
		return ErrOverload
	}

	w.markTime = time.Now()
	sf.mux.Lock()
	if sf.closeDone == closed { // check again,make sure
		sf.mux.Unlock()
		return ErrClosed
	}
	sf.idleGoRoutines.PushBack(w)
	sf.cond.Signal()
	sf.mux.Unlock()
	return nil
}

func (sf *work) run() {
	sf.pool.wg.Add(1)
	atomic.AddInt32(&sf.pool.running, 1)
	go func() {
		defer func() {
			sf.pool.wg.Done()
			atomic.AddInt32(&sf.pool.running, -1)
			sf.pool.cache.Put(sf)
			if r := recover(); r != nil && sf.pool.panicFunc != nil {
				sf.pool.panicFunc()
			}
		}()

		for f := range sf.task {
			if f == nil {
				return
			}
			f.Run()
			if sf.pool.push(sf) != nil {
				return
			}
		}
	}()
}
