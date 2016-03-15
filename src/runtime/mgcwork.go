// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

const (
	_Debugwbufs  = false // if true check wbufs consistency 如果是true的话，检查wbufs的一致性
	_WorkbufSize = 2048  // in bytes; larger values result in less contention workbuf的大小2K
)

// GC的work pool的抽象
// Garbage collector work pool abstraction.
//
// 实现了生产者消费者模型，主要为指向灰对象的指针。灰对象被mark并且
// 在工作队列上。黑对象被mark但没有在工作队列上。
// This implements a producer/consumer model for pointers to grey
// objects.  A grey object is one that is marked and on a work
// queue.  A black object is marked and not on a work queue.
//
// 写屏障,查找root,栈scan和对象scan产生指向灰对象的指针。scan消耗
// 这种指针，将它们置黑，然后scan，隐含的可能将产生新的对灰对象的指针。
// Write barriers, root discovery, stack scanning, and object scanning
// produce pointers to grey objects.  Scanning consumes pointers to
// grey objects, thus blackening them, and then scans them,
// potentially producing new pointers to grey objects.

// A wbufptr holds a workbuf*, but protects it from write barriers.
// workbufs never live on the heap, so write barriers are unnecessary.
// Write barriers on workbuf pointers may also be dangerous in the GC.
type wbufptr uintptr // 指向workbuf的指针

func wbufptrOf(w *workbuf) wbufptr { // 获得指向workbuf的指针
	return wbufptr(unsafe.Pointer(w))
}

func (wp wbufptr) ptr() *workbuf { // 根据指向workbuf的指针转换为workbuf结构
	return (*workbuf)(unsafe.Pointer(wp))
}

// gcWork为垃圾收集器提供了产生及消费work的接口。
// A gcWork provides the interface to produce and consume work for the
// garbage collector.
//
// gcWork可以在栈上如下使用
// A gcWork can be used on the stack as follows:
//
//     var gcw gcWork
//     disable preemption
//     .. call gcw.put() to produce and gcw.get() to consume ..
//     gcw.dispose()
//     enable preemption
//
// Or from the per-P gcWork cache:
//
//     (preemption must be disabled)
//     gcw := &getg().m.p.ptr().gcw
//     .. call gcw.put() to produce and gcw.get() to consume ..
//     if gcBlackenPromptly {
//         gcw.dispose()
//     }
//
// It's important that any use of gcWork during the mark phase prevent
// the garbage collector from transitioning to mark termination since
// gcWork may locally hold GC work buffers. This can be done by
// disabling preemption (systemstack or acquirem).
type gcWork struct {
	// wbuf1 and wbuf2 are the primary and secondary work buffers. wbuf1和wbuf2是主要的和次要的work buffer
	//
	// This can be thought of as a stack of both work buffers'
	// pointers concatenated. When we pop the last pointer, we
	// shift the stack up by one work buffer by bringing in a new
	// full buffer and discarding an empty one. When we fill both
	// buffers, we shift the stack down by one work buffer by
	// bringing in a new empty buffer and discarding a full one.
	// This way we have one buffer's worth of hysteresis, which
	// amortizes the cost of getting or putting a work buffer over
	// at least one buffer of work and reduces contention on the
	// global work lists.
	//
	// wbuf1 is always the buffer we're currently pushing to and
	// popping from and wbuf2 is the buffer that will be discarded
	// next.
	//
	// Invariant: Both wbuf1 and wbuf2 are nil or neither are.
	wbuf1, wbuf2 wbufptr

	// Bytes marked (blackened) on this gcWork. This is aggregated
	// into work.bytesMarked by dispose.
	bytesMarked uint64 // 该gcWork mark的字节数

	// 在该gcWork上执行的Scan工作
	// Scan work performed on this gcWork. This is aggregated into
	// gcController by dispose and may also be flushed by callers.
	scanWork int64
}

func (w *gcWork) init() { // 初始化gcWork
	w.wbuf1 = wbufptrOf(getempty(101)) // 从empty中获得workbuf赋值给wbuf1
	wbuf2 := trygetfull(102)
	if wbuf2 == nil {
		wbuf2 = getempty(103)
	}
	w.wbuf2 = wbufptrOf(wbuf2) // 尝试从full中获得workbuf若没有则从empty中获得workbuf赋值给wbuf2
}

// put enqueues a pointer for the garbage collector to trace.
// obj must point to the beginning of a heap object.
//go:nowritebarrier
func (ww *gcWork) put(obj uintptr) { // 将obj指针进行排队
	w := (*gcWork)(noescape(unsafe.Pointer(ww))) // TODO: remove when escape analysis is fixed

	wbuf := w.wbuf1.ptr() // 获得wbuf1指向的workbuf结构
	if wbuf == nil {      // 如果指针为空，表面还没有初始化
		w.init()             // 执行gcWork的初始化
		wbuf = w.wbuf1.ptr() // 获取gcWork中的wbuf1
		// wbuf is empty at this point.
	} else if wbuf.nobj == len(wbuf.obj) { // 如果workbuf已经满了
		w.wbuf1, w.wbuf2 = w.wbuf2, w.wbuf1 // 交换wbuf1和wbuf2
		wbuf = w.wbuf1.ptr()                // 获得交换后的workbuf结构
		if wbuf.nobj == len(wbuf.obj) {     // 如果wbuf2也是满的
			putfull(wbuf, 132)        // 将wbuf加入到work.full队列中
			wbuf = getempty(133)      // 获得一个新的workbuf结构
			w.wbuf1 = wbufptrOf(wbuf) // 为wbuf1赋值
		}
	}

	wbuf.obj[wbuf.nobj] = obj // 将obj压入到wbuf中
	wbuf.nobj++               // obj的数量增加
}

// tryGet dequeues a pointer for the garbage collector to trace.
//
// If there are no pointers remaining in this gcWork or in the global
// queue, tryGet returns 0.  Note that there may still be pointers in
// other gcWork instances or other caches.
//go:nowritebarrier
func (ww *gcWork) tryGet() uintptr { // 从gcWork中获得一个对象的指针
	w := (*gcWork)(noescape(unsafe.Pointer(ww))) // TODO: remove when escape analysis is fixed

	wbuf := w.wbuf1.ptr()
	if wbuf == nil {
		w.init()
		wbuf = w.wbuf1.ptr()
		// wbuf is empty at this point.
	}
	if wbuf.nobj == 0 {
		w.wbuf1, w.wbuf2 = w.wbuf2, w.wbuf1
		wbuf = w.wbuf1.ptr()
		if wbuf.nobj == 0 {
			owbuf := wbuf
			wbuf = trygetfull(167)
			if wbuf == nil {
				return 0
			}
			putempty(owbuf, 166)
			w.wbuf1 = wbufptrOf(wbuf)
		}
	}

	wbuf.nobj--
	return wbuf.obj[wbuf.nobj]
}

// get dequeues a pointer for the garbage collector to trace, blocking
// if necessary to ensure all pointers from all queues and caches have
// been retrieved.  get returns 0 if there are no pointers remaining.
//go:nowritebarrier
func (ww *gcWork) get() uintptr {
	w := (*gcWork)(noescape(unsafe.Pointer(ww))) // TODO: remove when escape analysis is fixed

	wbuf := w.wbuf1.ptr() // 获取wbuf1,赋值为wbuf
	if wbuf == nil {      // 如果wbuf为空，初始化
		w.init()
		wbuf = w.wbuf1.ptr()
		// wbuf is empty at this point.
	}
	if wbuf.nobj == 0 { //wbuf1中没有对象
		w.wbuf1, w.wbuf2 = w.wbuf2, w.wbuf1 // 交换wbuf1和wbuf2
		wbuf = w.wbuf1.ptr()
		if wbuf.nobj == 0 { // 仍然没有obj
			owbuf := wbuf
			wbuf = getfull(185) // 从work.full中取一个workbuf
			if wbuf == nil {
				return 0
			}
			putempty(owbuf, 184)
			w.wbuf1 = wbufptrOf(wbuf)
		}
	}

	// TODO: This might be a good place to add prefetch code

	wbuf.nobj--
	return wbuf.obj[wbuf.nobj] // 取出来一个obj
}

// dispose returns any cached pointers to the global queue.
// The buffers are being put on the full queue so that the
// write barriers will not simply reacquire them before the
// GC can inspect them. This helps reduce the mutator's
// ability to hide pointers during the concurrent mark phase.
//
//go:nowritebarrier
func (w *gcWork) dispose() { // 释放gcWork将wbuf1和wbuf2放回队列
	if wbuf := w.wbuf1.ptr(); wbuf != nil { // 取回wbuf1的指针，如果wbuf1非空
		if wbuf.nobj == 0 { // wbuf中无对象
			putempty(wbuf, 212) // 放入work.empty队列
		} else {
			putfull(wbuf, 214) // 放入work.full队列
		}
		w.wbuf1 = 0

		wbuf = w.wbuf2.ptr()
		if wbuf.nobj == 0 {
			putempty(wbuf, 218)
		} else {
			putfull(wbuf, 220)
		}
		w.wbuf2 = 0
	}
	if w.bytesMarked != 0 { // 如果该gcwork被mark的字节数非0
		// dispose happens relatively infrequently. If this
		// atomic becomes a problem, we should first try to
		// dispose less and if necessary aggregate in a per-P
		// counter.
		atomic.Xadd64(&work.bytesMarked, int64(w.bytesMarked)) // 增加work的mark的字节数
		w.bytesMarked = 0
	}
	if w.scanWork != 0 {
		atomic.Xaddint64(&gcController.scanWork, w.scanWork)
		w.scanWork = 0
	}
}

// balance moves some work that's cached in this gcWork back on the
// global queue.
//go:nowritebarrier
func (w *gcWork) balance() {
	if w.wbuf1 == 0 {
		return
	}
	if wbuf := w.wbuf2.ptr(); wbuf.nobj != 0 {
		putfull(wbuf, 246)
		w.wbuf2 = wbufptrOf(getempty(247))
	} else if wbuf := w.wbuf1.ptr(); wbuf.nobj > 4 {
		w.wbuf1 = wbufptrOf(handoff(wbuf))
	}
}

// empty returns true if w has no mark work available.
//go:nowritebarrier
func (w *gcWork) empty() bool { // 如果没有需要进行的mark工作，返回true
	return w.wbuf1 == 0 || (w.wbuf1.ptr().nobj == 0 && w.wbuf2.ptr().nobj == 0)
}

// Internally, the GC work pool is kept in arrays in work buffers.
// The gcWork interface caches a work buffer until full (or empty) to
// avoid contending on the global work buffer lists.

type workbufhdr struct {
	node  lfnode // must be first 必须是第一项
	nobj  int    // workbuf中有多少个对象
	inuse bool   // This workbuf is in use by some gorotuine and is not on the work.empty/full queues. 是否正在被goroutine使用，没有在work.empty和work.full队列中
	log   [4]int // line numbers forming a history of ownership changes to workbuf 行号，组成了变更该workbuf的历史记录
}

type workbuf struct { // 总共2K大小的workbuf，前面是workbuf头部
	workbufhdr // 封装了一个workbufhdr结构
	// account for the above fields
	obj [(_WorkbufSize - unsafe.Sizeof(workbufhdr{})) / sys.PtrSize]uintptr
}

// workbuf factory routines. These funcs are used to manage the
// workbufs.
// If the GC asks for some work these are the only routines that
// make wbufs available to the GC.
// Each of the gets and puts also take an distinct integer that is used
// to record a brief history of changes to ownership of the workbuf.
// The convention is to use a unique line number but any encoding
// is permissible. For example if you want to pass in 2 bits of information
// you could simple add lineno1*100000+lineno2.

// logget records the past few values of entry to aid in debugging.
// logget checks the buffer b is not currently in use.
func (b *workbuf) logget(entry int) { // 记录workbuf的使用情况
	if !_Debugwbufs {
		return
	}
	if b.inuse { // 如果workbuf正在被使用，抛出异常
		println("runtime: logget fails log entry=", entry,
			"b.log[0]=", b.log[0], "b.log[1]=", b.log[1],
			"b.log[2]=", b.log[2], "b.log[3]=", b.log[3])
		throw("logget: get not legal")
	}
	b.inuse = true            // 设置正在被使用
	copy(b.log[1:], b.log[:]) // 变更使用历史记录
	b.log[0] = entry
}

// logput records the past few values of entry to aid in debugging.
// logput checks the buffer b is currently in use.
func (b *workbuf) logput(entry int) {
	if !_Debugwbufs {
		return
	}
	if !b.inuse {
		println("runtime: logput fails log entry=", entry,
			"b.log[0]=", b.log[0], "b.log[1]=", b.log[1],
			"b.log[2]=", b.log[2], "b.log[3]=", b.log[3])
		throw("logput: put not legal")
	}
	b.inuse = false
	copy(b.log[1:], b.log[:]) // 变更使用历史记录
	b.log[0] = entry
}

func (b *workbuf) checknonempty() { // 要求workbuf不能为空
	if b.nobj == 0 { // 如果workbuf中保存的对象数量为0,抛出异常
		println("runtime: nonempty check fails",
			"b.log[0]=", b.log[0], "b.log[1]=", b.log[1],
			"b.log[2]=", b.log[2], "b.log[3]=", b.log[3])
		throw("workbuf is empty")
	}
}

func (b *workbuf) checkempty() { // 要求workbuf必须为空
	if b.nobj != 0 {
		println("runtime: empty check fails",
			"b.log[0]=", b.log[0], "b.log[1]=", b.log[1],
			"b.log[2]=", b.log[2], "b.log[3]=", b.log[3])
		throw("workbuf is not empty")
	}
}

// getempty从work.empty队列中弹出一个空的workbuf
// getempty pops an empty work buffer off the work.empty list,
// allocating new buffers if none are available.
// entry is used to record a brief history of ownership.
//go:nowritebarrier
func getempty(entry int) *workbuf {
	var b *workbuf
	if work.empty != 0 { // 如果work.empty非空
		b = (*workbuf)(lfstackpop(&work.empty)) // 返回一个workbuf结构
		if b != nil {                           // 取出来了workbuf结构
			b.checkempty() // 要求取出的workbuf必须为空
		}
	}
	if b == nil { // 如果没有取到，重新分配一个workbuf
		b = (*workbuf)(persistentalloc(unsafe.Sizeof(*b), sys.CacheLineSize, &memstats.gc_sys))
	}
	b.logget(entry)
	return b
}

// putempty puts a workbuf onto the work.empty list.
// Upon entry this go routine owns b. The lfstackpush relinquishes ownership.
//go:nowritebarrier
func putempty(b *workbuf, entry int) { // 将workbuf加入到work.empty队列中
	b.checkempty()
	b.logput(entry)
	lfstackpush(&work.empty, &b.node)
}

// putfull puts the workbuf on the work.full list for the GC.
// putfull accepts partially full buffers so the GC can avoid competing
// with the mutators for ownership of partially full buffers.
//go:nowritebarrier
func putfull(b *workbuf, entry int) {
	b.checknonempty()                // 检查workbuf不能为空
	b.logput(entry)                  // 记录workbuf的使用日志
	lfstackpush(&work.full, &b.node) // 将workbuf加入到work.full队列中

	// We just made more work available. Let the GC controller
	// know so it can encourage more workers to run.
	if gcphase == _GCmark {
		gcController.enlistWorker()
	}
}

// trygetfull tries to get a full or partially empty workbuffer.
// If one is not immediately available return nil
//go:nowritebarrier
func trygetfull(entry int) *workbuf { // 尝试获得一个满的或者部分满的workbuffer
	b := (*workbuf)(lfstackpop(&work.full))
	if b != nil {
		b.logget(entry)
		b.checknonempty()
		return b
	}
	return b
}

// Get a full work buffer off the work.full list.
// If nothing is available wait until all the other gc helpers have
// finished and then return nil.
// getfull acts as a barrier for work.nproc helpers. As long as one
// gchelper is actively marking objects it
// may create a workbuffer that the other helpers can work on.
// The for loop either exits when a work buffer is found
// or when _all_ of the work.nproc GC helpers are in the loop
// looking for work and thus not capable of creating new work.
// This is in fact the termination condition for the STW mark
// phase.
//go:nowritebarrier
func getfull(entry int) *workbuf {
	b := (*workbuf)(lfstackpop(&work.full))
	if b != nil {
		b.logget(entry)
		b.checknonempty()
		return b
	}

	incnwait := atomic.Xadd(&work.nwait, +1)
	if incnwait > work.nproc {
		println("runtime: work.nwait=", incnwait, "work.nproc=", work.nproc)
		throw("work.nwait > work.nproc")
	}
	for i := 0; ; i++ {
		if work.full != 0 {
			decnwait := atomic.Xadd(&work.nwait, -1)
			if decnwait == work.nproc {
				println("runtime: work.nwait=", decnwait, "work.nproc=", work.nproc)
				throw("work.nwait > work.nproc")
			}
			b = (*workbuf)(lfstackpop(&work.full))
			if b != nil {
				b.logget(entry)
				b.checknonempty()
				return b
			}
			incnwait := atomic.Xadd(&work.nwait, +1)
			if incnwait > work.nproc {
				println("runtime: work.nwait=", incnwait, "work.nproc=", work.nproc)
				throw("work.nwait > work.nproc")
			}
		}
		if work.nwait == work.nproc && work.markrootNext >= work.markrootJobs {
			return nil
		}
		_g_ := getg()
		if i < 10 {
			_g_.m.gcstats.nprocyield++
			procyield(20)
		} else if i < 20 {
			_g_.m.gcstats.nosyield++
			osyield()
		} else {
			_g_.m.gcstats.nsleep++
			usleep(100)
		}
	}
}

//go:nowritebarrier
func handoff(b *workbuf) *workbuf { // 返回一半的workbuf
	// Make new buffer with half of b's pointers.
	b1 := getempty(915) // 获得一个空的workbuf
	n := b.nobj / 2     // 将b这个workbuf的对象数量除以2
	b.nobj -= n
	b1.nobj = n // 平分对象数量
	memmove(unsafe.Pointer(&b1.obj[0]), unsafe.Pointer(&b.obj[b.nobj]), uintptr(n)*unsafe.Sizeof(b1.obj[0]))
	_g_ := getg()                          // 获得当前的goroutine
	_g_.m.gcstats.nhandoff++               // handoff的次数增加
	_g_.m.gcstats.nhandoffcnt += uint64(n) // handoff的总数量增加

	// Put b on full list - let first half of b get stolen.
	putfull(b, 942) // 将b加入work.full队列
	return b1
}
