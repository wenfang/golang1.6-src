// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build dragonfly freebsd linux

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// This implementation depends on OS-specific implementations of
//
//	runtime·futexsleep(uint32 *addr, uint32 val, int64 ns)
//		Atomically,
//			if(*addr == val) sleep
//		Might be woken up spuriously; that's allowed.
//		Don't sleep longer than ns; ns < 0 means forever.
//
//	runtime·futexwakeup(uint32 *addr, uint32 cnt)
//		If any procs are sleeping on addr, wake up at most cnt.

const (
	mutex_unlocked = 0
	mutex_locked   = 1
	mutex_sleeping = 2

	active_spin     = 4
	active_spin_cnt = 30
	passive_spin    = 1
)

// Possible lock states are mutex_unlocked, mutex_locked and mutex_sleeping.
// mutex_sleeping means that there is presumably at least one sleeping thread.
// Note that there can be spinning threads during all states - they do not
// affect mutex's state.

// We use the uintptr mutex.key and note.key as a uint32.
func key32(p *uintptr) *uint32 {
	return (*uint32)(unsafe.Pointer(p))
}

func lock(l *mutex) {
	gp := getg() // 获得当前的goroutine

	if gp.m.locks < 0 { // 如果lock的count小于0，抛出异常
		throw("runtime·lock: lock count")
	}
	gp.m.locks++ // 增加锁的count

	// Speculative grab for lock.
	v := atomic.Xchg(key32(&l.key), mutex_locked)
	if v == mutex_unlocked { // 如果处于未加锁状态，直接返回
		return
	}

	// wait is either MUTEX_LOCKED or MUTEX_SLEEPING
	// depending on whether there is a thread sleeping
	// on this mutex.  If we ever change l->key from
	// MUTEX_SLEEPING to some other value, we must be
	// careful to change it back to MUTEX_SLEEPING before
	// returning, to ensure that the sleeping thread gets
	// its wakeup call.
	wait := v

	// On uniprocessors, no point spinning.
	// On multiprocessors, spin for ACTIVE_SPIN attempts.
	spin := 0
	if ncpu > 1 {
		spin = active_spin
	}
	for {
		// Try for lock, spinning.
		for i := 0; i < spin; i++ {
			for l.key == mutex_unlocked {
				if atomic.Cas(key32(&l.key), mutex_unlocked, wait) {
					return
				}
			}
			procyield(active_spin_cnt)
		}

		// Try for lock, rescheduling.
		for i := 0; i < passive_spin; i++ {
			for l.key == mutex_unlocked {
				if atomic.Cas(key32(&l.key), mutex_unlocked, wait) {
					return
				}
			}
			osyield()
		}

		// Sleep.
		v = atomic.Xchg(key32(&l.key), mutex_sleeping)
		if v == mutex_unlocked {
			return
		}
		wait = mutex_sleeping
		futexsleep(key32(&l.key), mutex_sleeping, -1)
	}
}

func unlock(l *mutex) {
	v := atomic.Xchg(key32(&l.key), mutex_unlocked)
	if v == mutex_unlocked {
		throw("unlock of unlocked lock")
	}
	if v == mutex_sleeping {
		futexwakeup(key32(&l.key), 1)
	}

	gp := getg()
	gp.m.locks--
	if gp.m.locks < 0 {
		throw("runtime·unlock: lock count")
	}
	if gp.m.locks == 0 && gp.preempt { // restore the preemption request in case we've cleared it in newstack
		gp.stackguard0 = stackPreempt
	}
}

// One-time notifications.
func noteclear(n *note) { // 将note中保存的key值清0
	n.key = 0 // key为1表明被唤醒，key为0表明sleep
}

func notewakeup(n *note) {
	old := atomic.Xchg(key32(&n.key), 1) // 交换note中的key和1
	if old != 0 {                        // 如果已经被唤醒了，抛出异常
		print("notewakeup - double wakeup (", old, ")\n")
		throw("notewakeup - double wakeup")
	}
	futexwakeup(key32(&n.key), 1) // 唤醒一个等待在key上的线程
}

func notesleep(n *note) {
	gp := getg()       // 获取当前的goroutine
	if gp != gp.m.g0 { // 如果当前是g0，不能notesleep
		throw("notesleep not on g0")
	}
	for atomic.Load(key32(&n.key)) == 0 { // 如果还没有被唤醒，阻塞住当前线程
		gp.m.blocked = true              // 当前的m被阻塞住了
		futexsleep(key32(&n.key), 0, -1) // sleep在key上
		gp.m.blocked = false             //恢复当前m的状态
	}
}

// May run with m.p==nil if called from notetsleep, so write barriers
// are not allowed.
//
//go:nosplit
//go:nowritebarrier
func notetsleep_internal(n *note, ns int64) bool {
	gp := getg() // 获取当前的goroutine

	if ns < 0 { // 如果要等待的ns值小于0，ns小于0，类似于notesleep，一直等待
		for atomic.Load(key32(&n.key)) == 0 { // 如果要获取的n.key的值为0，一直等待
			gp.m.blocked = true // 当前的m被阻塞住了
			futexsleep(key32(&n.key), 0, -1)
			gp.m.blocked = false // 当前的m解除阻塞
		}
		return true
	}

	if atomic.Load(key32(&n.key)) != 0 { // 如果当前的note解除阻塞，直接返回
		return true
	}

	deadline := nanotime() + ns // 获取deadline的时间点
	for {
		gp.m.blocked = true              // 设置m处于阻塞状态
		futexsleep(key32(&n.key), 0, ns) // 等待ns时间
		gp.m.blocked = false
		if atomic.Load(key32(&n.key)) != 0 { // 获取key的值
			break
		}
		now := nanotime()
		if now >= deadline { // 如果当前的时间超过了deadline跳出循环
			break
		}
		ns = deadline - now
	}
	return atomic.Load(key32(&n.key)) != 0 // 返回是否唤醒成功
}

func notetsleep(n *note, ns int64) bool { // 等待一段时间，返回是否唤醒成功
	gp := getg()
	if gp != gp.m.g0 && gp.m.preemptoff != "" {
		throw("notetsleep not on g0")
	}

	return notetsleep_internal(n, ns)
}

// same as runtime·notetsleep, but called on user g (not g0)
// calls only nosplit functions between entersyscallblock/exitsyscall
func notetsleepg(n *note, ns int64) bool {
	gp := getg()
	if gp == gp.m.g0 {
		throw("notetsleepg on g0")
	}

	entersyscallblock(0)
	ok := notetsleep_internal(n, ns)
	exitsyscall(0)
	return ok
}
