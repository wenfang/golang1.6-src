// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// 每个P的cache，用来分配小对象，因为只被一个P所使用，所以不需要锁定
// Per-thread (in Go, per-P) cache for small objects.
// No locking needed because it is per-thread (per-P).
//
// mcaches are allocated from non-GC'd memory, so any heap pointers
// must be specially handled.
type mcache struct {
	// 下列成员在每个malloc调用时访问，因此成组出现
	// The following members are accessed on every malloc,
	// so they are grouped here for better caching.
	next_sample int32   // trigger heap sample after allocating this many bytes
	local_scan  uintptr // bytes of scannable heap allocated
	// 分配小对象时使用
	// Allocator cache for tiny objects w/o pointers.
	// See "Tiny allocator" comment in malloc.go.

	// tiny points to the beginning of the current tiny block, or
	// nil if there is no current tiny block.
	//
	// tiny is a heap pointer. Since mcache is in non-GC'd memory,
	// we handle it by clearing it in releaseAll during mark
	// termination.
	tiny             uintptr
	tinyoffset       uintptr
	local_tinyallocs uintptr // number of tiny allocs not counted in other stats

	// The rest is not accessed on every malloc.
	alloc [_NumSizeClasses]*mspan // spans to allocate from 对应每个67类的mspan

	stackcache [_NumStackOrders]stackfreelist // 用作分配栈空间的stackfreelist

	// Local allocator stats, flushed during GC.
	local_nlookup    uintptr                  // number of pointer lookups
	local_largefree  uintptr                  // bytes freed for large objects (>maxsmallsize)
	local_nlargefree uintptr                  // number of frees for large objects (>maxsmallsize)
	local_nsmallfree [_NumSizeClasses]uintptr // number of frees for small objects (<=maxsmallsize)
}

// gclink是在块的连接列表中的一个节点,像mlink，但是对gc不透明。
// gc工作的时候不会坚持gclinkptr指针，对gclinkptr的值编译器也不会发出写屏障
// 代码应该存储到gclinks结构的引用，而不是使用指针
// A gclink is a node in a linked list of blocks, like mlink,
// but it is opaque to the garbage collector.
// The GC does not trace the pointers during collection,
// and the compiler does not emit write barriers for assignments
// of gclinkptr values. Code should store references to gclinks
// as gclinkptr, not as *gclink.
type gclink struct {
	next gclinkptr
}

// 是到gclink结构的指针，但是对垃圾收集不透明
// A gclinkptr is a pointer to a gclink, but it is opaque
// to the garbage collector.
type gclinkptr uintptr

// ptr返回p代表的gclink的指针，返回的结果应该被用来访问域，而不是存储进其他数据结构中
// ptr returns the *gclink form of p.
// The result should be used for accessing fields, not stored
// in other data structures.
func (p gclinkptr) ptr() *gclink {
	return (*gclink)(unsafe.Pointer(p))
}

type stackfreelist struct {
	list gclinkptr // linked list of free stacks
	size uintptr   // total size of stacks in list
}

// dummy MSpan that contains no free objects. 不包含空闲对象的dummy mspan
var emptymspan mspan

func allocmcache() *mcache { // 分配出mcache结构
	lock(&mheap_.lock) // 先给mheap加锁
	c := (*mcache)(mheap_.cachealloc.alloc())
	unlock(&mheap_.lock)                         // 分配完结构后就给mheap解锁
	memclr(unsafe.Pointer(c), unsafe.Sizeof(*c)) // 清除mcache结构
	for i := 0; i < _NumSizeClasses; i++ {       // 对67个class的每个class，都设置为emptymspan
		c.alloc[i] = &emptymspan // 设置为不包含空闲对象的dummy mspan
	}
	c.next_sample = nextSample()
	return c
}

func freemcache(c *mcache) { // 释放mcache
	systemstack(func() {
		c.releaseAll()
		stackcache_clear(c)

		// NOTE(rsc,rlh): If gcworkbuffree comes back, we need to coordinate
		// with the stealing of gcworkbufs during garbage collection to avoid
		// a race where the workbuf is double-freed.
		// gcworkbuffree(c.gcworkbuf)

		lock(&mheap_.lock)
		purgecachedstats(c)
		mheap_.cachealloc.free(unsafe.Pointer(c))
		unlock(&mheap_.lock)
	})
}

// Gets a span that has a free object in it and assigns it
// to be the cached span for the given sizeclass.  Returns this span.
func (c *mcache) refill(sizeclass int32) *mspan {
	_g_ := getg()

	_g_.m.locks++
	// Return the current cached span to the central lists.
	s := c.alloc[sizeclass]      // 获得用于分配该class的mspan
	if s.freelist.ptr() != nil { // 如果是个非空mspan，抛出异常，已经有数据了，不用再fill
		throw("refill on a nonempty span")
	}
	if s != &emptymspan { // 到这里时这个mspan的空间已经分配完了，如果它不是mspan，将incache设置为false
		s.incache = false
	}

	// Get a new cached span from the central lists.
	s = mheap_.central[sizeclass].mcentral.cacheSpan()
	if s == nil { // 获得mspan失败，内存溢出了
		throw("out of memory")
	}
	if s.freelist.ptr() == nil { // 如果分配出的是一个空mspan，抛出异常
		println(s.ref, (s.npages<<_PageShift)/s.elemsize)
		throw("empty span")
	}
	c.alloc[sizeclass] = s // 将分配得到的mspan赋值给mcache
	_g_.m.locks--          // 为当前的m解锁
	return s
}

func (c *mcache) releaseAll() {
	for i := 0; i < _NumSizeClasses; i++ {
		s := c.alloc[i]
		if s != &emptymspan {
			mheap_.central[i].mcentral.uncacheSpan(s)
			c.alloc[i] = &emptymspan
		}
	}
	// Clear tinyalloc pool.
	c.tiny = 0
	c.tinyoffset = 0
}
