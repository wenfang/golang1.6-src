// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Central free lists.
//
// See malloc.go for an overview.
// mcentral不真实包含空闲对象的列表，而是由mspan包含，每个mcentral包含两个mspan列表，一个包含空闲对象，另一个已完全分配完
// The MCentral doesn't actually contain the list of free objects; the MSpan does.
// Each MCentral is two lists of MSpans: those with free objects (c->nonempty)
// and those that are completely allocated (c->empty).

package runtime

import "runtime/internal/atomic"

// Central list of free objects of a given size.
type mcentral struct { // 给定大小的空闲对象
	lock      mutex     // mcentral的锁
	sizeclass int32     // 该mcentral用于分配sizeclass大小的空间
	nonempty  mSpanList // list of spans with a free object
	empty     mSpanList // list of spans with no free objects (or cached in an mcache)
}

// Initialize a single central free list.
func (c *mcentral) init(sizeclass int32) { // 初始化单个mcentral空闲列表
	c.sizeclass = sizeclass
	c.nonempty.init()
	c.empty.init()
}

// Allocate a span to use in an MCache.
func (c *mcentral) cacheSpan() *mspan {
	// Deduct credit for this span allocation and sweep if necessary.
	spanBytes := uintptr(class_to_allocnpages[c.sizeclass]) * _PageSize
	deductSweepCredit(spanBytes, 0)

	lock(&c.lock)
	sg := mheap_.sweepgen // 获得当前mheap的sweep代数
retry:
	var s *mspan
	for s = c.nonempty.first; s != nil; s = s.next {
		if s.sweepgen == sg-2 && atomic.Cas(&s.sweepgen, sg-2, sg-1) {
			c.nonempty.remove(s)
			c.empty.insertBack(s)
			unlock(&c.lock)
			s.sweep(true)
			goto havespan
		}
		if s.sweepgen == sg-1 {
			// the span is being swept by background sweeper, skip
			continue
		}
		// we have a nonempty span that does not require sweeping, allocate from it
		c.nonempty.remove(s)
		c.empty.insertBack(s)
		unlock(&c.lock)
		goto havespan
	}

	for s = c.empty.first; s != nil; s = s.next {
		if s.sweepgen == sg-2 && atomic.Cas(&s.sweepgen, sg-2, sg-1) {
			// we have an empty span that requires sweeping,
			// sweep it and see if we can free some space in it
			c.empty.remove(s)
			// swept spans are at the end of the list
			c.empty.insertBack(s)
			unlock(&c.lock)
			s.sweep(true)
			if s.freelist.ptr() != nil {
				goto havespan
			}
			lock(&c.lock)
			// the span is still empty after sweep
			// it is already in the empty list, so just retry
			goto retry
		}
		if s.sweepgen == sg-1 {
			// the span is being swept by background sweeper, skip
			continue
		}
		// already swept empty span,
		// all subsequent ones must also be either swept or in process of sweeping
		break
	}
	unlock(&c.lock)

	// 如果到这里还没有发现mspan，需要重新进行mcentral_grow了
	// Replenish central list if empty.
	s = c.grow()
	if s == nil {
		return nil
	}
	lock(&c.lock)
	c.empty.insertBack(s)
	unlock(&c.lock)

	// 到这里时，s是一个非空的mspan
	// At this point s is a non-empty span, queued at the end of the empty list,
	// c is unlocked.
havespan:
	cap := int32((s.npages << _PageShift) / s.elemsize) // 获得该mspan可保持多少对象
	n := cap - int32(s.ref)
	if n == 0 { // 保存的对象为0，这是一个空的mspan，抛出异常
		throw("empty span")
	}
	usedBytes := uintptr(s.ref) * s.elemsize
	if usedBytes > 0 {
		reimburseSweepCredit(usedBytes)
	}
	atomic.Xadd64(&memstats.heap_live, int64(spanBytes)-int64(usedBytes))
	if trace.enabled {
		// heap_live changed.
		traceHeapAlloc()
	}
	if gcBlackenEnabled != 0 {
		// heap_live changed.
		gcController.revise()
	}
	if s.freelist.ptr() == nil {
		throw("freelist empty")
	}
	s.incache = true // 从mcentral中获取了一个mspan，将要被加到mcache中，设置incache为true
	return s         // 返回找到的mspan
}

// Return span from an MCache.
func (c *mcentral) uncacheSpan(s *mspan) {
	lock(&c.lock)

	s.incache = false // mspan已经不在mcache中了

	if s.ref == 0 {
		throw("uncaching full span")
	}

	cap := int32((s.npages << _PageShift) / s.elemsize)
	n := cap - int32(s.ref)
	if n > 0 {
		c.empty.remove(s)
		c.nonempty.insert(s)
		// mCentral_CacheSpan conservatively counted
		// unallocated slots in heap_live. Undo this.
		atomic.Xadd64(&memstats.heap_live, -int64(n)*int64(s.elemsize))
	}
	unlock(&c.lock)
}

// Free n objects from a span s back into the central free list c.
// Called during sweep.
// Returns true if the span was returned to heap.  Sets sweepgen to
// the latest generation.
// If preserve=true, don't return the span to heap nor relink in MCentral lists;
// caller takes care of it.
func (c *mcentral) freeSpan(s *mspan, n int32, start gclinkptr, end gclinkptr, preserve bool) bool {
	if s.incache {
		throw("freespan into cached span")
	}

	// Add the objects back to s's free list.
	wasempty := s.freelist.ptr() == nil
	end.ptr().next = s.freelist
	s.freelist = start
	s.ref -= uint16(n)

	if preserve {
		// preserve is set only when called from MCentral_CacheSpan above,
		// the span must be in the empty list.
		if !s.inList() {
			throw("can't preserve unlinked span")
		}
		atomic.Store(&s.sweepgen, mheap_.sweepgen)
		return false
	}

	lock(&c.lock)

	// Move to nonempty if necessary.
	if wasempty {
		c.empty.remove(s)
		c.nonempty.insert(s)
	}

	// delay updating sweepgen until here.  This is the signal that
	// the span may be used in an MCache, so it must come after the
	// linked list operations above (actually, just after the
	// lock of c above.)
	atomic.Store(&s.sweepgen, mheap_.sweepgen)

	if s.ref != 0 {
		unlock(&c.lock)
		return false
	}

	// s is completely freed, return it to the heap.
	c.nonempty.remove(s)
	s.needzero = 1
	s.freelist = 0
	unlock(&c.lock)
	heapBitsForSpan(s.base()).initSpan(s.layout())
	mheap_.freeSpan(s, 0)
	return true
}

// Fetch a new span from the heap and carve into objects for the free list.
func (c *mcentral) grow() *mspan {
	npages := uintptr(class_to_allocnpages[c.sizeclass])
	size := uintptr(class_to_size[c.sizeclass])
	n := (npages << _PageShift) / size

	s := mheap_.alloc(npages, c.sizeclass, false, true)
	if s == nil {
		return nil
	}

	p := uintptr(s.start << _PageShift) // 获得mspan的起始页面号
	s.limit = p + size*n                // 获得mspan结束位置
	head := gclinkptr(p)
	tail := gclinkptr(p)
	// i==0 iteration already done
	for i := uintptr(1); i < n; i++ { // 作为gclinkptr串起来
		p += size
		tail.ptr().next = gclinkptr(p)
		tail = gclinkptr(p)
	}
	if s.freelist.ptr() != nil {
		throw("freelist not empty")
	}
	tail.ptr().next = 0
	s.freelist = head // 加入到freelist列表中
	heapBitsForSpan(s.base()).initSpan(s.layout())
	return s
}
