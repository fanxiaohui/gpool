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

// package gpool Implementing a goroutine pool
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
	DefaultCapacity        = 100000
	DefaultSurvivalTime    = 1 * time.Second
	DefaultMiniCleanupTime = 100 * time.Millisecond
)

// define pool state
const (
	onWork = iota
	closed
)

var (
	// ErrClosed indicate the pool has closed
	ErrClosed = errors.New("pool has closed")
	// ErrInvalidFunc indicate the task function is invalid
	ErrInvalidFunc = errors.New("invalid function, must not be nil")
	// ErrOverload indicate the goroutine overload
	ErrOverload = errors.New("pool overload")
)

// TaskFunc task function define
type TaskFunc func(arg interface{})

// Config the pool config parameter
type Config struct {
	Capacity        int
	SurvivalTime    time.Duration
	MiniCleanupTime time.Duration // mini cleanup time
}

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
func New(c ...Config) *Pool {
	if len(c) == 0 {
		c = append(c, Config{
			Capacity:        DefaultCapacity,
			SurvivalTime:    DefaultSurvivalTime,
			MiniCleanupTime: DefaultMiniCleanupTime,
		})
	}
	if c[0].Capacity < 0 {
		c[0].Capacity = DefaultCapacity
	}
	if c[0].MiniCleanupTime < DefaultMiniCleanupTime {
		c[0].MiniCleanupTime = DefaultMiniCleanupTime
	}

	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		ctx:    ctx,
		cancel: cancel,

		capacity:        int32(c[0].Capacity),
		survivalTime:    c[0].SurvivalTime,
		miniCleanupTime: c[0].MiniCleanupTime,

		idleGoRoutines: newList(),
	}
	p.cond = sync.NewCond(&p.mux)
	p.cache = &sync.Pool{
		New: func() interface{} { return &work{itm: make(chan item, 1), pool: p} },
	}
	go p.cleanUp()
	return p
}

func (this *Pool) cleanUp() {
	tick := time.NewTimer(this.survivalTime)
	defer tick.Stop()

	for {
		select {
		case <-tick.C:
			nearTimeout := this.survivalTime
			now := time.Now()
			this.mux.Lock()
			var next *work
			for e := this.idleGoRoutines.Front(); e != nil; e = next {
				if nearTimeout = now.Sub(e.markTime); nearTimeout < this.survivalTime {
					break
				}
				next = e.Next() // save before delete
				this.idleGoRoutines.remove(e).itm <- item{}
			}
			this.mux.Unlock()
			if nearTimeout < this.miniCleanupTime {
				nearTimeout = this.miniCleanupTime
			}
			tick.Reset(nearTimeout)
		case <-this.ctx.Done():
			this.mux.Lock()
			for e := this.idleGoRoutines.Front(); e != nil; e = e.Next() {
				e.itm <- item{} // give a nil function, make all goroutine exit
			}
			this.idleGoRoutines = nil
			this.mux.Unlock()
			return
		}
	}
}

// SetPanicHandler set panic handler
func (this *Pool) SetPanicHandler(f func()) {
	this.panicFunc = f
}

// Len returns the currently running goroutines
func (this *Pool) Len() int {
	return int(atomic.LoadInt32(&this.running))
}

// Cap tha capacity of goroutines the pool can create
func (this *Pool) Cap() int {
	return int(atomic.LoadInt32(&this.capacity))
}

// Adjust adjust the capacity of the pools goroutines
func (this *Pool) Adjust(size int) {
	if size < 0 || this.Cap() == size {
		return
	}
	atomic.StoreInt32(&this.capacity, int32(size))
}

// Free return the available goroutines can create
func (this *Pool) Free() int {
	return this.Cap() - this.Len()
}

// Idle return the goroutines has running but in idle(no task work)
func (this *Pool) Idle() int {
	var cnt int
	this.mux.Lock()
	if this.idleGoRoutines != nil {
		cnt = this.idleGoRoutines.Len()
	}
	this.mux.Unlock()
	return cnt
}

// Close the pool,if grace enable util all goroutine close
func (this *Pool) Close(grace bool) error {
	if atomic.LoadUint32(&this.closeDone) == closed {
		return nil
	}

	this.mux.Lock()
	if this.closeDone == onWork { // check again,make sure
		this.cancel()
		atomic.StoreUint32(&this.closeDone, closed)
	}
	this.mux.Unlock()
	if grace {
		this.wg.Wait()
	}
	return nil
}

// Submit submits a task with arg
func (this *Pool) Submit(f TaskFunc, arg interface{}) error {
	var w *work

	if f == nil {
		return ErrInvalidFunc
	}

	if atomic.LoadUint32(&this.closeDone) == closed {
		return ErrClosed
	}

	this.mux.Lock()
	if this.closeDone == closed || this.idleGoRoutines == nil { // check again,make sure
		this.mux.Unlock()
		return ErrClosed
	}

	itm := item{f, arg}
	if w = this.idleGoRoutines.Front(); w != nil {
		this.idleGoRoutines.Remove(w)
		this.mux.Unlock()
		w.itm <- itm
		return nil
	}

	// actual goroutines maybe greater than cap, when race, but it will overload and return to normal in goroutine
	if this.Free() > 0 {
		this.mux.Unlock()
		w = this.cache.Get().(*work)
		w.run(itm)
		return nil
	}

	for {
		this.cond.Wait()
		if w = this.idleGoRoutines.Front(); w != nil {
			this.idleGoRoutines.Remove(w)
			break
		}
	}
	this.mux.Unlock()
	w.itm <- itm
	return nil
}

// push the running goroutine to idle pool
func (this *Pool) push(w *work) error {
	if atomic.LoadUint32(&this.closeDone) == closed { // quick check
		return ErrClosed
	}

	if this.Free() < 0 {
		return ErrOverload
	}

	w.markTime = time.Now()
	this.mux.Lock()
	if this.closeDone == closed { // check again,make sure
		this.mux.Unlock()
		return ErrClosed
	}
	this.idleGoRoutines.PushBack(w)
	this.cond.Signal()
	this.mux.Unlock()
	return nil
}

func (this *work) run(itm item) {
	this.pool.wg.Add(1)
	atomic.AddInt32(&this.pool.running, 1)
	go func() {
		defer func() {
			this.pool.wg.Done()
			atomic.AddInt32(&this.pool.running, -1)
			this.pool.cache.Put(this)
			if r := recover(); r != nil && this.pool.panicFunc != nil {
				this.pool.panicFunc()
			}
		}()

		for {
			itm.task(itm.arg)
			if this.pool.push(this) != nil {
				return
			}
			if itm = <-this.itm; itm.task == nil {
				return
			}
		}
	}()
}
