// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build darwin dragonfly freebsd linux nacl netbsd openbsd solaris windows

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// 集成的networker poller(平台无关的部分)
// 具体的实现(epoll/kqueue)必须定义下列函数:
// func netpollinit()
// Integrated network poller (platform-independent part).
// A particular implementation (epoll/kqueue) must define the following functions:
// func netpollinit()			// to initialize the poller
// func netpollopen(fd uintptr, pd *pollDesc) int32	// to arm edge-triggered notifications
// and associate fd with pd.
// An implementation must call the following function to denote that the pd is ready.
// func netpollready(gpp **g, pd *pollDesc, mode int32)

// pollDesc contains 2 binary semaphores, rg and wg, to park reader and writer
// goroutines respectively. The semaphore can be in the following states:
// pdReady - io readiness notification is pending; 已经产生了IO通知，但是还没有处理，处理之后将这个状态变为nil
//           a goroutine consumes the notification by changing the state to nil.
// pdWait - a goroutine prepares to park on the semaphore, but not yet parked; goroutine等待阻塞在该信号量上，但是还没阻塞
//          the goroutine commits to park by changing the state to G pointer,
//          or, alternatively, concurrent io notification changes the state to READY,
//          or, alternatively, concurrent timeout/close changes the state to nil.
// G pointer - the goroutine is blocked on the semaphore;
//             io notification or timeout/close changes the state to READY or nil respectively
//             and unparks the goroutine.
// nil - nothing of the above.
const (
	pdReady uintptr = 1
	pdWait  uintptr = 2
)

const pollBlockSize = 4 * 1024 // 设置poll块的大小4K

// Network poller descriptor.
type pollDesc struct { // 网络poller描述结构，如果是epoll的话会保存在event.data中
	link *pollDesc // in pollcache, protected by pollcache.lock 在pollCache队列中的指针

	// The lock protects pollOpen, pollSetDeadline, pollUnblock and deadlineimpl operations.
	// This fully covers seq, rt and wt variables. fd is constant throughout the PollDesc lifetime.
	// pollReset, pollWait, pollWaitCanceled and runtime·netpollready (IO readiness notification)
	// proceed w/o taking the lock. So closing, rg, rd, wg and wd are manipulated
	// in a lock-free way by all operations.
	// NOTE(dvyukov): the following code uses uintptr to store *g (rg/wg),
	// that will blow up when GC starts moving objects.
	lock    mutex   // protects the following fields
	fd      uintptr // 对应打开句柄
	closing bool    // 该句柄是否关闭
	seq     uintptr // protects from stale timers and ready notifications
	rg      uintptr // pdReady, pdWait, G waiting for read or nil 保存等待读的goroutine或者为nil
	rt      timer   // read deadline timer (set if rt.f != nil) 读deadline定时器
	rd      int64   // read deadline 读deadline
	wg      uintptr // pdReady, pdWait, G waiting for write or nil 保存等待写的goroutine或者为nil
	wt      timer   // write deadline timer 写deadline定时器
	wd      int64   // write deadline 写deadline
	user    uint32  // user settable cookie
}

type pollCache struct { // pollCache结构用作pollDesc结构的缓存
	lock  mutex
	first *pollDesc
	// PollDesc objects must be type-stable,
	// because we can get ready notification from epoll/kqueue
	// after the descriptor is closed/reused.
	// Stale notifications are detected using seq variable,
	// seq is incremented when deadlines are changed or descriptor is reused.
}

var (
	netpollInited uint32
	pollcache     pollCache
)

//go:linkname net_runtime_pollServerInit net.runtime_pollServerInit
func net_runtime_pollServerInit() {
	netpollinit()
	atomic.Store(&netpollInited, 1)
}

func netpollinited() bool {
	return atomic.Load(&netpollInited) != 0
}

//go:linkname net_runtime_pollOpen net.runtime_pollOpen
func net_runtime_pollOpen(fd uintptr) (*pollDesc, int) {
	pd := pollcache.alloc()
	lock(&pd.lock)
	if pd.wg != 0 && pd.wg != pdReady {
		throw("netpollOpen: blocked write on free descriptor")
	}
	if pd.rg != 0 && pd.rg != pdReady {
		throw("netpollOpen: blocked read on free descriptor")
	}
	pd.fd = fd         // 设置fd
	pd.closing = false // 设置当前未被关闭
	pd.seq++           // 设置打开次数
	pd.rg = 0          // 重新打开了,设置rg标志位0
	pd.rd = 0          // 读超时为0
	pd.wg = 0          // 重新打开了,设置wg标志位0
	pd.wd = 0          // 写超时为0
	unlock(&pd.lock)   // 对pollcache解锁

	var errno int32
	errno = netpollopen(fd, pd)
	return pd, int(errno)
}

//go:linkname net_runtime_pollClose net.runtime_pollClose
func net_runtime_pollClose(pd *pollDesc) {
	if !pd.closing {
		throw("netpollClose: close w/o unblock")
	}
	if pd.wg != 0 && pd.wg != pdReady {
		throw("netpollClose: blocked write on closing descriptor")
	}
	if pd.rg != 0 && pd.rg != pdReady {
		throw("netpollClose: blocked read on closing descriptor")
	}
	netpollclose(uintptr(pd.fd))
	pollcache.free(pd)
}

func (c *pollCache) free(pd *pollDesc) { // 将pd结构归还到pollCache中
	lock(&c.lock)
	pd.link = c.first
	c.first = pd
	unlock(&c.lock)
}

//go:linkname net_runtime_pollReset net.runtime_pollReset
func net_runtime_pollReset(pd *pollDesc, mode int) int {
	err := netpollcheckerr(pd, int32(mode))
	if err != 0 {
		return err
	}
	if mode == 'r' { // 如果是读模式，设置读模式为0
		pd.rg = 0
	} else if mode == 'w' { // 如果是写模式，设置写模式为0
		pd.wg = 0
	}
	return 0
}

//go:linkname net_runtime_pollWait net.runtime_pollWait
func net_runtime_pollWait(pd *pollDesc, mode int) int {
	err := netpollcheckerr(pd, int32(mode))
	if err != 0 {
		return err
	}
	// As for now only Solaris uses level-triggered IO.
	if GOOS == "solaris" { // 当前只有solaris使用水平触发IO
		netpollarm(pd, mode)
	}
	for !netpollblock(pd, int32(mode), false) { // 阻塞在这里等待事件发生
		err = netpollcheckerr(pd, int32(mode))
		if err != 0 {
			return err
		}
		// Can happen if timeout has fired and unblocked us,
		// but before we had a chance to run, timeout has been reset.
		// Pretend it has not happened and retry.
	}
	return 0
}

//go:linkname net_runtime_pollWaitCanceled net.runtime_pollWaitCanceled
func net_runtime_pollWaitCanceled(pd *pollDesc, mode int) {
	// This function is used only on windows after a failed attempt to cancel
	// a pending async IO operation. Wait for ioready, ignore closing or timeouts.
	for !netpollblock(pd, int32(mode), true) {
	}
}

//go:linkname net_runtime_pollSetDeadline net.runtime_pollSetDeadline
func net_runtime_pollSetDeadline(pd *pollDesc, d int64, mode int) {
	lock(&pd.lock)
	if pd.closing {
		unlock(&pd.lock)
		return
	}
	pd.seq++ // invalidate current timers
	// Reset current timers.
	if pd.rt.f != nil {
		deltimer(&pd.rt)
		pd.rt.f = nil
	}
	if pd.wt.f != nil {
		deltimer(&pd.wt)
		pd.wt.f = nil
	}
	// Setup new timers.
	if d != 0 && d <= nanotime() {
		d = -1
	}
	if mode == 'r' || mode == 'r'+'w' {
		pd.rd = d
	}
	if mode == 'w' || mode == 'r'+'w' {
		pd.wd = d
	}
	if pd.rd > 0 && pd.rd == pd.wd {
		pd.rt.f = netpollDeadline
		pd.rt.when = pd.rd
		// Copy current seq into the timer arg.
		// Timer func will check the seq against current descriptor seq,
		// if they differ the descriptor was reused or timers were reset.
		pd.rt.arg = pd
		pd.rt.seq = pd.seq
		addtimer(&pd.rt)
	} else {
		if pd.rd > 0 {
			pd.rt.f = netpollReadDeadline
			pd.rt.when = pd.rd
			pd.rt.arg = pd
			pd.rt.seq = pd.seq
			addtimer(&pd.rt)
		}
		if pd.wd > 0 {
			pd.wt.f = netpollWriteDeadline
			pd.wt.when = pd.wd
			pd.wt.arg = pd
			pd.wt.seq = pd.seq
			addtimer(&pd.wt)
		}
	}
	// If we set the new deadline in the past, unblock currently pending IO if any.
	var rg, wg *g
	atomicstorep(unsafe.Pointer(&wg), nil) // full memory barrier between stores to rd/wd and load of rg/wg in netpollunblock
	if pd.rd < 0 {
		rg = netpollunblock(pd, 'r', false)
	}
	if pd.wd < 0 {
		wg = netpollunblock(pd, 'w', false)
	}
	unlock(&pd.lock)
	if rg != nil {
		goready(rg, 3)
	}
	if wg != nil {
		goready(wg, 3)
	}
}

//go:linkname net_runtime_pollUnblock net.runtime_pollUnblock
func net_runtime_pollUnblock(pd *pollDesc) {
	lock(&pd.lock)
	if pd.closing {
		throw("netpollUnblock: already closing")
	}
	pd.closing = true
	pd.seq++
	var rg, wg *g
	atomicstorep(unsafe.Pointer(&rg), nil) // full memory barrier between store to closing and read of rg/wg in netpollunblock
	rg = netpollunblock(pd, 'r', false)
	wg = netpollunblock(pd, 'w', false)
	if pd.rt.f != nil {
		deltimer(&pd.rt)
		pd.rt.f = nil
	}
	if pd.wt.f != nil {
		deltimer(&pd.wt)
		pd.wt.f = nil
	}
	unlock(&pd.lock)
	if rg != nil {
		goready(rg, 3)
	}
	if wg != nil {
		goready(wg, 3)
	}
}

// make pd ready, newly runnable goroutines (if any) are returned in rg/wg
// May run during STW, so write barriers are not allowed.
//go:nowritebarrier
func netpollready(gpp *guintptr, pd *pollDesc, mode int32) {
	var rg, wg guintptr
	if mode == 'r' || mode == 'r'+'w' {
		rg.set(netpollunblock(pd, 'r', true))
	}
	if mode == 'w' || mode == 'r'+'w' {
		wg.set(netpollunblock(pd, 'w', true))
	}
	if rg != 0 {
		rg.ptr().schedlink = *gpp
		*gpp = rg
	}
	if wg != 0 {
		wg.ptr().schedlink = *gpp
		*gpp = wg
	}
}

func netpollcheckerr(pd *pollDesc, mode int32) int {
	if pd.closing {
		return 1 // errClosing
	}
	if (mode == 'r' && pd.rd < 0) || (mode == 'w' && pd.wd < 0) {
		return 2 // errTimeout
	}
	return 0
}

func netpollblockcommit(gp *g, gpp unsafe.Pointer) bool { // 将gpp的值设置为goroutine
	return atomic.Casuintptr((*uintptr)(gpp), pdWait, uintptr(unsafe.Pointer(gp)))
}

// 如果IO就绪，返回true，如果超时或者关闭返回false
// returns true if IO is ready, or false if timedout or closed
// waitio - wait only for completed IO, ignore errors 等待pd发生mode事件,waitio设置为true时只等待完整的IO，忽略错误
func netpollblock(pd *pollDesc, mode int32, waitio bool) bool { // io等待成功时返回true，超时或者关闭返回false
	gpp := &pd.rg    // 默认选择等待读的goroutine
	if mode == 'w' { // 根据模式选择不同的指针值
		gpp = &pd.wg
	}

	// set the gpp semaphore to WAIT
	for {
		old := *gpp         // 先取出来原有的值
		if old == pdReady { // 如果老的标识为pdReady，表明已经有事件发生，将标识置为0，返回
			*gpp = 0
			return true // 已经有事件发生了，不用阻塞，直接返回
		}
		if old != 0 { // 如果当前标识不为0，重复设置了
			throw("netpollblock: double wait")
		}
		if atomic.Casuintptr(gpp, 0, pdWait) { // 将gpp的值设置为pdWait
			break
		}
	}

	// need to recheck error states after setting gpp to WAIT
	// this is necessary because runtime_pollUnblock/runtime_pollSetDeadline/deadlineimpl
	// do the opposite: store to closing/rd/wd, membarrier, load of rg/wg
	if waitio || netpollcheckerr(pd, mode) == 0 {
		gopark(netpollblockcommit, unsafe.Pointer(gpp), "IO wait", traceEvGoBlockNet, 5)
	}
	// be careful to not lose concurrent READY notification
	old := atomic.Xchguintptr(gpp, 0)
	if old > pdWait {
		throw("netpollblock: corrupted state")
	}
	return old == pdReady
}

func netpollunblock(pd *pollDesc, mode int32, ioready bool) *g { // 解锁等待该事件的goroutine，如果ioready设置为true
	gpp := &pd.rg    // 默认取读列表
	if mode == 'w' { // 如果是写模式，取写列表
		gpp = &pd.wg
	}

	for {
		old := *gpp         // 获得原始列表中的值，根据读写模式，对应读列表或者写列表
		if old == pdReady { // 如果老的状态是Ready表明出现了事件，但是没有goroutine等待
			return nil
		}
		if old == 0 && !ioready { // 如果old状态为0，并且没有设置ioready，返回空
			// Only set READY for ioready. runtime_pollWait
			// will check for timeout/cancel before waiting.
			return nil
		}
		var new uintptr
		if ioready { // 如果设置了ioready为true
			new = pdReady // 将状态置为pdReady
		}
		if atomic.Casuintptr(gpp, old, new) {
			if old == pdReady || old == pdWait { // 如果以前的状态为pdReady或者pgWait，设置为0
				old = 0
			}
			return (*g)(unsafe.Pointer(old)) // 返回old值
		}
	}
}

func netpolldeadlineimpl(pd *pollDesc, seq uintptr, read, write bool) {
	lock(&pd.lock)
	// Seq arg is seq when the timer was set.
	// If it's stale, ignore the timer event.
	if seq != pd.seq {
		// The descriptor was reused or timers were reset.
		unlock(&pd.lock)
		return
	}
	var rg *g
	if read {
		if pd.rd <= 0 || pd.rt.f == nil {
			throw("netpolldeadlineimpl: inconsistent read deadline")
		}
		pd.rd = -1
		atomicstorep(unsafe.Pointer(&pd.rt.f), nil) // full memory barrier between store to rd and load of rg in netpollunblock
		rg = netpollunblock(pd, 'r', false)
	}
	var wg *g
	if write {
		if pd.wd <= 0 || pd.wt.f == nil && !read {
			throw("netpolldeadlineimpl: inconsistent write deadline")
		}
		pd.wd = -1
		atomicstorep(unsafe.Pointer(&pd.wt.f), nil) // full memory barrier between store to wd and load of wg in netpollunblock
		wg = netpollunblock(pd, 'w', false)
	}
	unlock(&pd.lock)
	if rg != nil {
		goready(rg, 0)
	}
	if wg != nil {
		goready(wg, 0)
	}
}

func netpollDeadline(arg interface{}, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, true, true)
}

func netpollReadDeadline(arg interface{}, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, true, false)
}

func netpollWriteDeadline(arg interface{}, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, false, true)
}

func (c *pollCache) alloc() *pollDesc { // 从pollCache中分配一个pollDesc结构
	lock(&c.lock)
	if c.first == nil { // 如果cache中为空
		const pdSize = unsafe.Sizeof(pollDesc{}) // 获取pollDesc单个的大小
		n := pollBlockSize / pdSize
		if n == 0 {
			n = 1
		}
		// Must be in non-GC memory because can be referenced
		// only from epoll/kqueue internals.
		mem := persistentalloc(n*pdSize, 0, &memstats.other_sys) // 分配空间，不可以被gc的内存
		for i := uintptr(0); i < n; i++ {
			pd := (*pollDesc)(add(mem, i*pdSize))
			pd.link = c.first
			c.first = pd
		}
	}
	pd := c.first
	c.first = pd.link
	unlock(&c.lock)
	return pd
}
