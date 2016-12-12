// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Page heap.
//
// See malloc.go for overview.

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// malloc分配的主堆内存
// heap本身在free和large数组
// Main malloc heap.
// The heap itself is the "free[]" and "large" arrays,
// but all the other global data is here too.
type mheap struct { // 主malloc的堆
	lock      mutex                    // 主malloc的锁
	free      [_MaxMHeapList]mSpanList // free lists of given length 已经分配的mspan，但是没人用
	freelarge mSpanList                // free lists length >= _MaxMHeapList
	busy      [_MaxMHeapList]mSpanList // busy lists of large objects of given length 有人用的mspan
	busylarge mSpanList                // busy lists of large objects length >= _MaxMHeapList
	allspans  **mspan                  // all spans out there 所有的mspan都在这个列表中
	gcspans   **mspan                  // copy of allspans referenced by gc marker or sweeper
	nspan     uint32                   // 堆中mspan的数量
	sweepgen  uint32                   // sweep generation, see comment in mspan sweep的代数
	sweepdone uint32                   // all spans are swept 所有的span已经被sweep
	// span lookup
	spans        **mspan // 用作mspan的查找，也就是h_spans
	spans_mapped uintptr

	// Proportional sweep
	pagesInUse        uint64  // pages of spans in stats _MSpanInUse; R/W with mheap.lock
	spanBytesAlloc    uint64  // bytes of spans allocated this cycle; updated atomically
	pagesSwept        uint64  // pages swept this cycle; updated atomically
	sweepPagesPerByte float64 // proportional sweep ratio; written with lock, read without
	// TODO(austin): pagesInUse should be a uintptr, but the 386
	// compiler can't 8-byte align fields.

	// Malloc stats. Malloc的状态
	largefree  uint64                  // bytes freed for large objects (>maxsmallsize)
	nlargefree uint64                  // number of frees for large objects (>maxsmallsize)
	nsmallfree [_NumSizeClasses]uint64 // number of frees for small objects (<=maxsmallsize)

	// range of addresses we might see in the heap
	bitmap         uintptr
	bitmap_mapped  uintptr
	arena_start    uintptr
	arena_used     uintptr // always mHeap_Map{Bits,Spans} before updating
	arena_end      uintptr
	arena_reserved bool

	// 对小对象的central空闲列表
	// central free lists for small size classes.
	// the padding makes sure that the MCentrals are
	// spaced CacheLineSize bytes apart, so that each MCentral.lock
	// gets its own cache line.
	central [_NumSizeClasses]struct { // 初始化67个类别的mcentral
		mcentral mcentral
		pad      [sys.CacheLineSize]byte
	}

	spanalloc             fixalloc // allocator for span* span结构的分配器
	cachealloc            fixalloc // allocator for mcache* mcache结构的分配器
	specialfinalizeralloc fixalloc // allocator for specialfinalizer* specialfinalizer结构的分配器
	specialprofilealloc   fixalloc // allocator for specialprofile* specialprofile结构的分配器
	speciallock           mutex    // lock for special record allocators.
}

var mheap_ mheap // 全局的mheap结构

// An MSpan is a run of pages. MSpan是一堆页面的集合
// 当MSpan在堆的free列表时，状态为MSpanFree
// When a MSpan is in the heap free list, state == MSpanFree
// and heapmap(s->start) == span, heapmap(s->start+s->npages-1) == span.
// 当一个MSpan被分配时，状态变为MSpanInUse或者MSpanStack
// When a MSpan is allocated, state == MSpanInUse or MSpanStack
// and heapmap(i) == span for all s->start <= i < s->start+s->npages.
// 每一个MSpan都在一个双向列表中，或者在MHeap的free列表中，或者在MCentral的span列表中，使用空MSpan结构作为列表头
// Every MSpan is in one doubly-linked list,
// either one of the MHeap's free lists or one of the
// MCentral's span lists.  We use empty MSpan structures as list heads.
// 当MSpan为_MSpanInUse状态时保存有确定的内存
// An MSpan representing actual memory has state _MSpanInUse,
// _MSpanStack, or _MSpanFree. Transitions between these states are
// constrained as follows:
// 在任何GC阶段,mspan的状态可以从free变为in-use或者stack状态
// * A span may transition from free to in-use or stack during any GC
//   phase.
// 在sweeping阶段，mspan可以从in_use状态变为free状态或者从stack状态变为free状态
// * During sweeping (gcphase == _GCoff), a span may transition from
//   in-use to free (as a result of sweeping) or stack to free (as a
//   result of stacks being freed).
// 在GC阶段，span一定不能从stack或in-use状态变为free状态，因为并发的gc可能会读一个指针并且查找其Span
// * During GC (gcphase != _GCoff), a span *must not* transition from
//   stack or in-use to free. Because concurrent GC may read a pointer
//   and then look up its span, the span state must be monotonic.
const (
	_MSpanInUse = iota // allocated for garbage collected heap 改Mspan用作可被垃圾收集的堆内存
	_MSpanStack        // allocated for use by stack allocator 该Mspan用作栈分配
	_MSpanFree
	_MSpanDead
)

// span链接列表的结构
// mSpanList heads a linked list of spans.
//
// Linked List结构基于BSD的tail queue数据结构
// Linked list structure is based on BSD's "tail queue" data structure.
type mSpanList struct {
	first *mspan  // first span in list, or nil if none 列表中的第一个mspan
	last  **mspan // last span's next field, or first if none 列表中的最后一个mspan
}

type mspan struct {
	next *mspan     // next span in list, or nil if none
	prev **mspan    // previous span's next field, or list head's first field if none
	list *mSpanList // For debugging. TODO: Remove. 所属的mspan列表

	start    pageID    // starting page number 起始页面号
	npages   uintptr   // number of pages in span 该mspan中页面的数量
	freelist gclinkptr // list of free objects 空闲对象的列表
	// sweep 的代数
	// 如果sweepgen == 堆的sweepgen-2，该span需要进行清除
	// 如果sweepgen == 堆的sweepgen-1，该span当前正在进行清除
	// 如果sweepgen == 堆的sweepgen，该span被清除了，可以被使用
	// 每次gc后堆得sweepgen值都会增2
	// sweep generation:
	// if sweepgen == h->sweepgen - 2, the span needs sweeping
	// if sweepgen == h->sweepgen - 1, the span is currently being swept
	// if sweepgen == h->sweepgen, the span is swept and ready to use
	// h->sweepgen is incremented by 2 after every GC

	sweepgen    uint32   // 该mspan的代数
	divMul      uint32   // for divide by elemsize - divMagic.mul 加速除操作的魔数
	ref         uint16   // capacity - number of objects in freelist 容量,freelist中对象的数量
	sizeclass   uint8    // size class 对应的size class的值
	incache     bool     // being used by an mcache 是否正在被一个mcache使用
	state       uint8    // mspaninuse etc mspan当前的状态
	needzero    uint8    // needs to be zeroed before allocation 在分配前需要清0
	divShift    uint8    // for divide by elemsize - divMagic.shift 加速除操作的魔数
	divShift2   uint8    // for divide by elemsize - divMagic.shift2 加速除操作的魔数
	elemsize    uintptr  // computed from sizeclass or from npages 保存的元素大小
	unusedsince int64    // first time spotted by gc in mspanfree state
	npreleased  uintptr  // number of pages released to the os
	limit       uintptr  // end of data in span 在span中数据的结束位置
	speciallock mutex    // guards specials list
	specials    *special // linked list of special records sorted by offset.
	baseMask    uintptr  // if non-0, elemsize is a power of 2, & this will get object allocation base
}

func (s *mspan) base() uintptr { // 获得mspan对应的起始地址，获得绝对地址
	return uintptr(s.start << _PageShift)
}

func (s *mspan) layout() (size, n, total uintptr) { // 获得可保存的元素大小，可保存的元素数量和总大小
	total = s.npages << _PageShift // 获得该mspan所有空间大小，页数量乘以页大小
	size = s.elemsize              // 获得该mspan可保存的元素大小
	if size > 0 {
		n = total / size // 获得可保存的元素数量
	}
	return
}

// 指向所有的mspan结构的指针，其实也就是mheap结构中的allspans
var h_allspans []*mspan // TODO: make this h.allspans once mheap can be defined in Go

// h_spans是一个查找表，将虚拟的页面ID映射到*mspan。对已分配的span，映射到span本身。
// 对空闲的span，只有最低和最高的页面映射到span自身。内部页面映射到任意span。
// 对从来没有分配的页面，h_spans项为空。
// h_spans is a lookup table to map virtual address page IDs to *mspan.
// For allocated spans, their pages map to the span itself.
// For free spans, only the lowest and highest pages map to the span itself.  Internal
// pages map to an arbitrary span.
// For pages that have never been allocated, h_spans entries are nil.
var h_spans []*mspan // TODO: make this h.spans once mheap can be defined in Go

// 将一个mspan加入到mheap中
func recordspan(vh unsafe.Pointer, p unsafe.Pointer) { // 将mspan p记录到堆vh中
	h := (*mheap)(vh)                       // 将vh转换为mheap结构
	s := (*mspan)(p)                        // 将p转换为mspan结构
	if len(h_allspans) >= cap(h_allspans) { // 如果h_allspans slice不够用了
		n := 64 * 1024 / sys.PtrSize // n保存一个64K可以保存多少个指针
		if n < cap(h_allspans)*3/2 { // 至少扩展1.5倍的spans的大小
			n = cap(h_allspans) * 3 / 2
		}
		var new []*mspan                                                 // 声明一个mspan指针数组
		sp := (*slice)(unsafe.Pointer(&new))                             // 转换为slice指针
		sp.array = sysAlloc(uintptr(n)*sys.PtrSize, &memstats.other_sys) // 分配可以保存n个指针的空间
		if sp.array == nil {                                             // 分配空间失败，抛出异常
			throw("runtime: cannot allocate memory")
		}
		sp.len = len(h_allspans) // 获得h_allspans的长度
		sp.cap = n
		if len(h_allspans) > 0 { // 将原有的span拷贝过来
			copy(new, h_allspans)
			// 如果老的数组正在被sweep引用，不要释放
			// Don't free the old array if it's referenced by sweep.
			// See the comment in mgc.go.
			if h.allspans != mheap_.gcspans {
				sysFree(unsafe.Pointer(h.allspans), uintptr(cap(h_allspans))*sys.PtrSize, &memstats.other_sys)
			}
		}
		h_allspans = new // 更换了mspan的列表
		h.allspans = (**mspan)(unsafe.Pointer(sp.array))
	}
	h_allspans = append(h_allspans, s) // 将mspan s加入到h_allspans slice中
	h.nspan = uint32(len(h_allspans))  // 更新堆中span的数量
}

// inheap指示是否b是一个指向堆对象的指针
// inheap reports whether b is a pointer into a (potentially dead) heap object.
// It returns false for pointers into stack spans.
// Non-preemptible because it is used by write barriers.
//go:nowritebarrier
//go:nosplit
func inheap(b uintptr) bool { // 传入一个地址，传出是否在堆上
	if b == 0 || b < mheap_.arena_start || b >= mheap_.arena_used { // 如果在堆空间以外，返回false
		return false
	}
	// Not a beginning of a block, consult span table to find the block beginning.
	k := b >> _PageShift // 获得指针的页偏移
	x := k
	x -= mheap_.arena_start >> _PageShift                                         // 获得指针所在页在堆内的偏移
	s := h_spans[x]                                                               // 根据偏移找到居停对应的mspan
	if s == nil || pageID(k) < s.start || b >= s.limit || s.state != mSpanInUse { // 如果该页不在有效区域返回false
		return false
	}
	return true // 其他情况表明是一个指向堆内对象的指针
}

// TODO: spanOf and spanOfUnchecked are open-coded in a lot of places.
// Use the functions instead.
// spanOf返回指针p所属的mspan，如果p不是堆得指针，或者没有span包含p，spanOf返回nil
// spanOf returns the span of p. If p does not point into the heap or
// no span contains p, spanOf returns nil.
func spanOf(p uintptr) *mspan {
	if p == 0 || p < mheap_.arena_start || p >= mheap_.arena_used {
		return nil
	}
	return spanOfUnchecked(p)
}

// spanOfUnchecked is equivalent to spanOf, but the caller must ensure
// that p points into the heap (that is, mheap_.arena_start <= p <
// mheap_.arena_used).
func spanOfUnchecked(p uintptr) *mspan { // 返回p所在的mspan,但是不做范围检查
	return h_spans[(p-mheap_.arena_start)>>_PageShift]
}

// 查找到v指针所在的mspan的对应元素的基地址base和可存放的元素的大小size，mspan本身由sp传出
func mlookup(v uintptr, base *uintptr, size *uintptr, sp **mspan) int32 {
	_g_ := getg() // 获得当前的goroutine

	_g_.m.mcache.local_nlookup++                                 // 本地查找次数增加
	if sys.PtrSize == 4 && _g_.m.mcache.local_nlookup >= 1<<30 { // 如果是32位系统，且local_nlookup次数过多purge cachedstas防止溢出
		// purge cache stats to prevent overflow purge cache状态，避免溢出
		lock(&mheap_.lock)
		purgecachedstats(_g_.m.mcache)
		unlock(&mheap_.lock)
	}

	s := mheap_.lookupMaybe(unsafe.Pointer(v)) // 查找对应的mspan
	if sp != nil {
		*sp = s // 由sp传出mspan
	}
	if s == nil { // 如果找到了mspan为空
		if base != nil {
			*base = 0
		}
		if size != nil {
			*size = 0
		}
		return 0
	}

	p := uintptr(s.start) << _PageShift // 获得mspan起始地址
	if s.sizeclass == 0 {               // 如果是大对象
		// Large object.
		if base != nil {
			*base = p // 设置基地址
		}
		if size != nil {
			*size = s.npages << _PageShift // 设置大小
		}
		return 1
	}
	// 如果是小对象，获得元素大小
	n := s.elemsize
	if base != nil {
		i := (uintptr(v) - uintptr(p)) / n
		*base = p + i*n // base为元素的起始地址
	}
	if size != nil { // 可存放的元素的大小
		*size = n
	}

	return 1
}

// Initialize the heap. 初始化heap
func (h *mheap) init(spans_size uintptr) {
	// 初始化几个结构的分配器
	h.spanalloc.init(unsafe.Sizeof(mspan{}), recordspan, unsafe.Pointer(h), &memstats.mspan_sys)
	h.cachealloc.init(unsafe.Sizeof(mcache{}), nil, nil, &memstats.mcache_sys)
	h.specialfinalizeralloc.init(unsafe.Sizeof(specialfinalizer{}), nil, nil, &memstats.other_sys)
	h.specialprofilealloc.init(unsafe.Sizeof(specialprofile{}), nil, nil, &memstats.other_sys)

	// h->mapcache needs no init
	for i := range h.free { // 初始化free和busy两个MSpanList结构
		h.free[i].init()
		h.busy[i].init()
	}
	// 初始化freelarge和busylarge两个MSpanList结构
	h.freelarge.init()
	h.busylarge.init()
	for i := range h.central { // 初始化mcentral列表
		h.central[i].mcentral.init(int32(i))
	}

	sp := (*slice)(unsafe.Pointer(&h_spans))
	sp.array = unsafe.Pointer(h.spans)
	sp.len = int(spans_size / sys.PtrSize)
	sp.cap = int(spans_size / sys.PtrSize)
}

// mHeap_mapSpans保证spans被映射到直到arena_used
// mHeap_MapSpans makes sure that the spans are mapped
// up to the new value of arena_used.
//
// It must be called with the expected new value of arena_used,
// *before* h.arena_used has been updated.
// Waiting to update arena_used until after the memory has been mapped
// avoids faults when other threads try access the bitmap immediately
// after observing the change to arena_used.
func (h *mheap) mapSpans(arena_used uintptr) {
	// Map spans array, PageSize at a time.
	n := arena_used
	n -= h.arena_start
	n = n / _PageSize * sys.PtrSize
	n = round(n, sys.PhysPageSize) // 取得已经映射了多少页面
	if h.spans_mapped >= n {       // 页面已经被映射了，直接返回
		return
	}
	sysMap(add(unsafe.Pointer(h.spans), h.spans_mapped), n-h.spans_mapped, h.arena_reserved, &memstats.other_sys)
	h.spans_mapped = n // 已经被映射了
}

// sweep列表中的spans，直到至少回收了npages个页面到堆上
// 返回真正回收了多少页面
// Sweeps spans in list until reclaims at least npages into heap.
// Returns the actual number of pages reclaimed.
func (h *mheap) reclaimList(list *mSpanList, npages uintptr) uintptr {
	n := uintptr(0)
	sg := mheap_.sweepgen // 获取当前sweep的代数
retry:
	for s := list.first; s != nil; s = s.next { // 遍历mSpanList
		if s.sweepgen == sg-2 && atomic.Cas(&s.sweepgen, sg-2, sg-1) { // 如果改mspan需要进行sweep
			list.remove(s) // 从列表中删除该mspan
			// swept spans are at the end of the list
			list.insertBack(s) // 将该mspan放到列表最后
			unlock(&h.lock)
			snpages := s.npages // 获取该mspan中页面的数量
			if s.sweep(false) { // 对页面执行sweep
				n += snpages // 增加已经被sweep的页面的数量
			}
			lock(&h.lock)
			if n >= npages { // 已经sweep了足够的页面，返回
				return n
			}
			// the span could have been moved elsewhere
			goto retry
		}
		if s.sweepgen == sg-1 { // 该span正在被后台sweeper进行sweep，略过
			// the span is being sweept by background sweeper, skip
			continue
		}
		// 已经sweep的空span，所有后续的mspan或者已经被sweep了，或者正在sweep过程中
		// already swept empty span,
		// all subsequent ones must also be either swept or in process of sweeping
		break
	}
	return n
}

// sweep并且回收至少npage个页面，在分配npage个页面前调用
// Sweeps and reclaims at least npage pages into heap.
// Called before allocating npage pages.
func (h *mheap) reclaim(npage uintptr) {
	// 首先尝试sweep busy的span，回收大对象的，也就是size的值大于npage
	// First try to sweep busy spans with large objects of size >= npage,
	// this has good chances of reclaiming the necessary space.
	for i := int(npage); i < len(h.busy); i++ {
		if h.reclaimList(&h.busy[i], npage) != 0 { // 回收完成，返回
			return // Bingo!
		}
	}

	// 尝试回收busylarge中的对象
	// Then -- even larger objects.
	if h.reclaimList(&h.busylarge, npage) != 0 {
		return // Bingo!
	}

	// 现在尝试回收小对象
	// Now try smaller objects.
	// One such object is not enough, so we need to reclaim several of them.
	reclaimed := uintptr(0) // 用reclaimed记录已经回收的页面的数量
	for i := 0; i < int(npage) && i < len(h.busy); i++ {
		reclaimed += h.reclaimList(&h.busy[i], npage-reclaimed) // 在小对象列表中执行回收
		if reclaimed >= npage {
			return
		}
	}

	// Now sweep everything that is not yet swept.
	unlock(&h.lock)
	for {
		n := sweepone()       // 执行一次sweepone，返回sweep的页面的数量
		if n == ^uintptr(0) { // all spans are swept 如果所有的页面已经被sweep了，跳出
			break
		}
		reclaimed += n // 增加已经回收的页面的数量
		if reclaimed >= npage {
			break
		}
	}
	lock(&h.lock)
}

// 分配一个保存npage个页面的mspan,sizeclass为该mspan保存的元素的大小
// Allocate a new span of npage pages from the heap for GC'd memory
// and record its size class in the HeapMap and HeapMapCache.
func (h *mheap) alloc_m(npage uintptr, sizeclass int32, large bool) *mspan {
	_g_ := getg()        // 获取当前的goroutine
	if _g_ != _g_.m.g0 { // 如果没有在g0的栈进行分配，抛出异常
		throw("_mheap_alloc not on g0 stack")
	}
	lock(&h.lock) // 锁定堆

	// 为了避免过度的栈增长，在分配n个页面前，需要sweep并且回收至少n个页面
	// To prevent excessive heap growth, before allocating n pages
	// we need to sweep and reclaim at least n pages.
	if h.sweepdone == 0 { // 如果并不是所有的mspan都被sweep了
		// TODO(austin): This tends to sweep a large number of
		// spans in order to find a few completely free spans
		// (for example, in the garbage benchmark, this sweeps
		// ~30x the number of pages its trying to allocate).
		// If GC kept a bit for whether there were any marks
		// in a span, we could release these free spans
		// at the end of GC and eliminate this entirely.
		h.reclaim(npage) // 尝试回收npage个页面
	}

	// 从cache向global传输统计信息
	// transfer stats from cache to global
	memstats.heap_scan += uint64(_g_.m.mcache.local_scan)
	_g_.m.mcache.local_scan = 0
	memstats.tinyallocs += uint64(_g_.m.mcache.local_tinyallocs)
	_g_.m.mcache.local_tinyallocs = 0

	s := h.allocSpanLocked(npage) // 开始执行mspan的分配
	if s != nil {                 // 如果mspan分配成功
		// Record span info, because gc needs to be
		// able to map interior pointer to containing span.
		atomic.Store(&s.sweepgen, h.sweepgen) // 把heap的sweepgen拷贝到mspan上
		s.state = _MSpanInUse                 // 该mspan的状态为使用中
		s.freelist = 0                        // 空闲列表为0
		s.ref = 0                             // 空闲列表中元素的数量为0
		s.sizeclass = uint8(sizeclass)        // 设置该mspan的sizeclass
		if sizeclass == 0 {                   // sizeclass为0，做大对象分配
			s.elemsize = s.npages << _PageShift // 可分配的元素大小和mspan可用容量相同
			s.divShift = 0
			s.divMul = 0
			s.divShift2 = 0
			s.baseMask = 0
		} else {
			s.elemsize = uintptr(class_to_size[sizeclass]) // 如果sizeclass不为0，转换为具体的大小
			m := &class_to_divmagic[sizeclass]             // 返回对应sizeclass的除的魔数
			s.divShift = m.shift
			s.divMul = m.mul
			s.divShift2 = m.shift2
			s.baseMask = m.baseMask
		}

		// update stats, sweep lists
		h.pagesInUse += uint64(npage) // 正在使用的页面的数量增加
		if large {
			memstats.heap_objects++                                      // 堆对象的数量增加
			atomic.Xadd64(&memstats.heap_live, int64(npage<<_PageShift)) // 增加活跃的内存数量
			// Swept spans are at the end of lists.
			if s.npages < uintptr(len(h.free)) { // 如果mspan分配的页面数量可以包含在mheap的free列表中
				h.busy[s.npages].insertBack(s) // 将分配的mspan加入busy列表
			} else {
				h.busylarge.insertBack(s) // 将分配的mspan加入busylarge列表
			}
		}
	}
	// heap_scan and heap_live were updated.
	if gcBlackenEnabled != 0 {
		gcController.revise()
	}

	if trace.enabled {
		traceHeapAlloc()
	}

	// h_spans is accessed concurrently without synchronization
	// from other threads. Hence, there must be a store/store
	// barrier here to ensure the writes to h_spans above happen
	// before the caller can publish a pointer p to an object
	// allocated from s. As soon as this happens, the garbage
	// collector running on another processor could read p and
	// look up s in h_spans. The unlock acts as the barrier to
	// order these writes. On the read side, the data dependency
	// between p and the index in h_spans orders the reads.
	unlock(&h.lock)
	return s // 返回mspan
}

// 与alloc_m类似，但是可以控制是否将分配的mspan中的内容清0
func (h *mheap) alloc(npage uintptr, sizeclass int32, large bool, needzero bool) *mspan {
	// Don't do any operations that lock the heap on the G stack.
	// It might trigger stack growth, and the stack growth code needs
	// to be able to allocate heap.
	var s *mspan
	systemstack(func() {
		s = h.alloc_m(npage, sizeclass, large)
	})

	if s != nil {
		if needzero && s.needzero != 0 { // 如果在分配时需要清空内容
			memclr(unsafe.Pointer(s.start<<_PageShift), s.npages<<_PageShift) // 将内容清空
		}
		s.needzero = 0
	}
	return s
}

// 分配npage个页面，用作栈空间
func (h *mheap) allocStack(npage uintptr) *mspan {
	_g_ := getg()
	if _g_ != _g_.m.g0 {
		throw("mheap_allocstack not on g0 stack")
	}
	lock(&h.lock)
	s := h.allocSpanLocked(npage)
	if s != nil {
		s.state = _MSpanStack // 该mspan用作栈使用
		s.freelist = 0
		s.ref = 0
		memstats.stacks_inuse += uint64(s.npages << _PageShift)
	}

	// This unlock acts as a release barrier. See mHeap_Alloc_m.
	unlock(&h.lock)
	return s
}

// Allocates a span of the given size.  h must be locked.
// The returned span has been removed from the
// free list, but its state is still MSpanFree.
func (h *mheap) allocSpanLocked(npage uintptr) *mspan { // 分配一个对应指定npage个的mspan
	var list *mSpanList
	var s *mspan

	// Try in fixed-size lists up to max.
	for i := int(npage); i < len(h.free); i++ { // 先找free队列中的mspan
		list = &h.free[i]
		if !list.isEmpty() {
			s = list.first
			goto HaveSpan
		}
	}

	// Best fit in list of large spans.
	list = &h.freelarge
	s = h.allocLarge(npage) // 从freelarge队列中查找mspan
	if s == nil {           // 如果没有查找到
		if !h.grow(npage) { // 往mheap中填充npage个页面空间
			return nil
		}
		s = h.allocLarge(npage) // 从freeLarge中再找一遍
		if s == nil {
			return nil
		}
	}

HaveSpan: // 找到了mspan
	// Mark span in use.
	if s.state != _MSpanFree {
		throw("MHeap_AllocLocked - MSpan not free")
	}
	if s.npages < npage {
		throw("MHeap_AllocLocked - bad npages")
	}
	list.remove(s) // 从列表中移除mspan
	if s.inList() {
		throw("still in list")
	}
	if s.npreleased > 0 {
		sysUsed(unsafe.Pointer(s.start<<_PageShift), s.npages<<_PageShift)
		memstats.heap_released -= uint64(s.npreleased << _PageShift)
		s.npreleased = 0
	}

	if s.npages > npage {
		// Trim extra and put it back in the heap.
		t := (*mspan)(h.spanalloc.alloc())
		t.init(s.start+pageID(npage), s.npages-npage)
		s.npages = npage
		p := uintptr(t.start)
		p -= (h.arena_start >> _PageShift)
		if p > 0 {
			h_spans[p-1] = s
		}
		h_spans[p] = t
		h_spans[p+t.npages-1] = t
		t.needzero = s.needzero
		s.state = _MSpanStack // prevent coalescing with s
		t.state = _MSpanStack
		h.freeSpanLocked(t, false, false, s.unusedsince)
		s.state = _MSpanFree
	}
	s.unusedsince = 0

	p := uintptr(s.start)
	p -= (h.arena_start >> _PageShift)
	for n := uintptr(0); n < npage; n++ {
		h_spans[p+n] = s
	}

	memstats.heap_inuse += uint64(npage << _PageShift)
	memstats.heap_idle -= uint64(npage << _PageShift)

	//println("spanalloc", hex(s.start<<_PageShift))
	if s.inList() {
		throw("still in list")
	}
	return s
}

// Allocate a span of exactly npage pages from the list of large spans.
func (h *mheap) allocLarge(npage uintptr) *mspan { // 在freelarge列表中查找最合适的mspan
	return bestFit(&h.freelarge, npage, nil)
}

// 查找mspan列表list，使mspan页的数量正好>=npage
// 如果发现多个最小的mspan，使用起始地址最小的那个
// Search list for smallest span with >= npage pages.
// If there are multiple smallest spans, take the one
// with the earliest starting address.
func bestFit(list *mSpanList, npage uintptr, best *mspan) *mspan {
	for s := list.first; s != nil; s = s.next { // 变量mspan列表
		if s.npages < npage {
			continue
		}
		if best == nil || s.npages < best.npages || (s.npages == best.npages && s.start < best.start) {
			best = s
		}
	}
	return best
}

// 试图添加至少npage个页面的内存到heap上，返回是否成功
// mheap堆必须被锁定
// Try to add at least npage pages of memory to the heap,
// returning whether it worked.
//
// h must be locked.
func (h *mheap) grow(npage uintptr) bool {
	// Ask for a big chunk, to reduce the number of mappings
	// the operating system needs to track; also amortizes
	// the overhead of an operating system mapping.
	// Allocate a multiple of 64kB.
	npage = round(npage, (64<<10)/_PageSize) // 分配页面必须为64K的整数倍
	ask := npage << _PageShift               // 需要分配的空间大小
	if ask < _HeapAllocChunk {               // 至少申请1M
		ask = _HeapAllocChunk
	}

	v := h.sysAlloc(ask) // 分配ask大小的内存
	if v == nil {        // 如果分配内存失败
		if ask > npage<<_PageShift {
			ask = npage << _PageShift // 重新调整内存大小
			v = h.sysAlloc(ask)       // 再分配一遍
		}
		if v == nil { // 如果仍然出错，打印分配内存错误
			print("runtime: out of memory: cannot allocate ", ask, "-byte block (", memstats.heap_sys, " in use)\n")
			return false
		}
	}

	// Create a fake "in use" span and free it, so that the
	// right coalescing happens.
	s := (*mspan)(h.spanalloc.alloc())                      // 先分配一个mspan结构
	s.init(pageID(uintptr(v)>>_PageShift), ask>>_PageShift) // 初始化该mspan
	p := uintptr(s.start)                                   // 获得mspan起始页面号
	p -= (h.arena_start >> _PageShift)                      // 计算页面号到arena_start地址，相差多少个页面
	for i := p; i < p+s.npages; i++ {                       // 把h_spans页面号到mspan映射表中的域进行赋值
		h_spans[i] = s
	}
	atomic.Store(&s.sweepgen, h.sweepgen) // 获取当前堆得代数，赋值给新的mspan
	s.state = _MSpanInUse                 // 设置mspan的状态为正在被使用
	h.pagesInUse += uint64(npage)         // 设置当前mheap中正在被使用的页面的数量
	h.freeSpanLocked(s, false, true, 0)
	return true
}

// Look up the span at the given address.
// Address is guaranteed to be in map
// and is guaranteed to be start or end of span.
func (h *mheap) lookup(v unsafe.Pointer) *mspan { // 查找地址v所属的mspan
	p := uintptr(v)
	p -= h.arena_start
	return h_spans[p>>_PageShift]
}

// 查找指定地址所在的span，地址不保证一定在map中
// Look up the span at the given address.
// Address is *not* guaranteed to be in map
// and may be anywhere in the span.
// Map entries for the middle of a span are only
// valid for allocated spans.  Free spans may have
// other garbage in their middles, so we have to
// check for that.
func (h *mheap) lookupMaybe(v unsafe.Pointer) *mspan {
	if uintptr(v) < h.arena_start || uintptr(v) >= h.arena_used { // 如果地址不在堆得区域内，返回nil
		return nil
	}
	p := uintptr(v) >> _PageShift // 返回地址v所在的页偏移
	q := p
	q -= h.arena_start >> _PageShift // 返回页的相对位置
	s := h_spans[q]                  // 找到对应的mspan
	if s == nil || p < uintptr(s.start) || uintptr(v) >= uintptr(unsafe.Pointer(s.limit)) || s.state != _MSpanInUse {
		return nil
	}
	return s
}

// 将mspan释放回mheap中
// Free the span back into the heap.
func (h *mheap) freeSpan(s *mspan, acct int32) {
	systemstack(func() {
		mp := getg().m // 获得goroutine所属的线程
		lock(&h.lock)  // 给mheap加锁
		memstats.heap_scan += uint64(mp.mcache.local_scan)
		mp.mcache.local_scan = 0
		memstats.tinyallocs += uint64(mp.mcache.local_tinyallocs)
		mp.mcache.local_tinyallocs = 0
		if acct != 0 {
			memstats.heap_objects--
		}
		if gcBlackenEnabled != 0 {
			// heap_scan changed.
			gcController.revise()
		}
		h.freeSpanLocked(s, true, true, 0)
		unlock(&h.lock) // mheap解锁
	})
}

func (h *mheap) freeStack(s *mspan) {
	_g_ := getg()
	if _g_ != _g_.m.g0 {
		throw("mheap_freestack not on g0 stack")
	}
	s.needzero = 1 // 需要清空内容
	lock(&h.lock)
	memstats.stacks_inuse -= uint64(s.npages << _PageShift) // 栈正在使用的页面，统计计数减小
	h.freeSpanLocked(s, true, true, 0)                      // 释放mspan到mheap中
	unlock(&h.lock)
}

// 释放mspan,s必须在busy列表中,acctinuse和acctidle表明是否计数正在inuse或idle状态的页面
// 将mspan归还到mheap的free列表中，并且尝试和前一个及后一个mspan进行合并
// s must be on a busy list (h.busy or h.busylarge) or unlinked.
func (h *mheap) freeSpanLocked(s *mspan, acctinuse, acctidle bool, unusedsince int64) {
	switch s.state {
	case _MSpanStack:
		if s.ref != 0 { // 如果freelist列表数量非0，抛出异常
			throw("MHeap_FreeSpanLocked - invalid stack free")
		}
	case _MSpanInUse:
		if s.ref != 0 || s.sweepgen != h.sweepgen { // 如果freelist列表数量非0，或者mspan和mheap的代数不同，抛出异常
			print("MHeap_FreeSpanLocked - span ", s, " ptr ", hex(s.start<<_PageShift), " ref ", s.ref, " sweepgen ", s.sweepgen, "/", h.sweepgen, "\n")
			throw("MHeap_FreeSpanLocked - invalid free")
		}
		h.pagesInUse -= uint64(s.npages) // 正在被使用的页面数量减少
	default:
		throw("MHeap_FreeSpanLocked - invalid span state") // 类型不符合，抛出异常
	}

	if acctinuse {
		memstats.heap_inuse -= uint64(s.npages << _PageShift)
	}
	if acctidle {
		memstats.heap_idle += uint64(s.npages << _PageShift)
	}
	s.state = _MSpanFree // 页面的状态变为free
	if s.inList() {      // 如果mspan在列表中
		h.busyList(s.npages).remove(s) // 从busy列表中释放mspan
	}

	// Stamp newly unused spans. The scavenger will use that
	// info to potentially give back some pages to the OS.
	s.unusedsince = unusedsince
	if unusedsince == 0 {
		s.unusedsince = nanotime() // 设置该mspan从什么时候开始未使用
	}
	s.npreleased = 0

	// Coalesce with earlier, later spans.
	p := uintptr(s.start)            // 获得起始页面号
	p -= h.arena_start >> _PageShift // 获得相对于arena_start地址的页面号
	if p > 0 {                       // 如果相对的页面号大于0，也就是，不是第一个
		t := h_spans[p-1]                      // 取得前一个页面对应的mspan
		if t != nil && t.state == _MSpanFree { // 如果前一个页面的状态为MSpanFree，合并两个mspan
			s.start = t.start
			s.npages += t.npages
			s.npreleased = t.npreleased // absorb released pages
			s.needzero |= t.needzero
			p -= t.npages
			h_spans[p] = s
			h.freeList(t.npages).remove(t)
			t.state = _MSpanDead
			h.spanalloc.free(unsafe.Pointer(t))
		}
	}
	if (p+s.npages)*sys.PtrSize < h.spans_mapped { // 尝试再和后面的mspan合并
		t := h_spans[p+s.npages]
		if t != nil && t.state == _MSpanFree {
			s.npages += t.npages
			s.npreleased += t.npreleased
			s.needzero |= t.needzero
			h_spans[p+s.npages-1] = s
			h.freeList(t.npages).remove(t)
			t.state = _MSpanDead
			h.spanalloc.free(unsafe.Pointer(t))
		}
	}

	// Insert s into appropriate list.
	h.freeList(s.npages).insert(s) // 将mspan放入free列表中
}

// 返回npages个页面对应的free的mspanlist
func (h *mheap) freeList(npages uintptr) *mSpanList {
	if npages < uintptr(len(h.free)) {
		return &h.free[npages]
	}
	return &h.freelarge
}

// 返回npages个页面对应的busy的mspanlist
func (h *mheap) busyList(npages uintptr) *mSpanList {
	if npages < uintptr(len(h.free)) {
		return &h.busy[npages]
	}
	return &h.busylarge
}

func scavengelist(list *mSpanList, now, limit uint64) uintptr {
	// 如果物理页面大小，比堆的逻辑页面大小大，返回0
	if sys.PhysPageSize > _PageSize {
		// golang.org/issue/9993
		// If the physical page size of the machine is larger than
		// our logical heap page size the kernel may round up the
		// amount to be freed to its page size and corrupt the heap
		// pages surrounding the unused block.
		return 0
	}

	if list.isEmpty() { // 列表为空，返回0
		return 0
	}

	var sumreleased uintptr
	for s := list.first; s != nil; s = s.next { // 遍历整个列表
		if (now-uint64(s.unusedsince)) > limit && s.npreleased != s.npages {
			released := (s.npages - s.npreleased) << _PageShift
			memstats.heap_released += uint64(released)
			sumreleased += released
			s.npreleased = s.npages
			sysUnused(unsafe.Pointer(s.start<<_PageShift), s.npages<<_PageShift) // 声明mspan对应的内存不再使用
		}
	}
	return sumreleased
}

func (h *mheap) scavenge(k int32, now, limit uint64) { // 声明free列表中的mspan对应的内存不再使用，如果时间limit后仍然空闲的话
	lock(&h.lock)
	var sumreleased uintptr
	for i := 0; i < len(h.free); i++ {
		sumreleased += scavengelist(&h.free[i], now, limit)
	}
	sumreleased += scavengelist(&h.freelarge, now, limit)
	unlock(&h.lock)

	if debug.gctrace > 0 {
		if sumreleased > 0 {
			print("scvg", k, ": ", sumreleased>>20, " MB released\n")
		}
		// TODO(dvyukov): these stats are incorrect as we don't subtract stack usage from heap.
		// But we can't call ReadMemStats on g0 holding locks.
		print("scvg", k, ": inuse: ", memstats.heap_inuse>>20, ", idle: ", memstats.heap_idle>>20, ", sys: ", memstats.heap_sys>>20, ", released: ", memstats.heap_released>>20, ", consumed: ", (memstats.heap_sys-memstats.heap_released)>>20, " (MB)\n")
	}
}

//go:linkname runtime_debug_freeOSMemory runtime/debug.freeOSMemory
func runtime_debug_freeOSMemory() {
	gcStart(gcForceBlockMode, false)
	systemstack(func() { mheap_.scavenge(-1, ^uint64(0), 0) })
}

// Initialize a new span with the given start and npages.
func (span *mspan) init(start pageID, npages uintptr) { // 初始化mspan,start为起始页面ID,npages是页面数量
	span.next = nil
	span.prev = nil
	span.list = nil
	span.start = start
	span.npages = npages
	span.freelist = 0 // 空闲对象列表为空
	span.ref = 0      // 空闲对象列表中元素的数量为0
	span.sizeclass = 0
	span.incache = false
	span.elemsize = 0
	span.state = _MSpanDead
	span.unusedsince = 0
	span.npreleased = 0
	span.speciallock.key = 0
	span.specials = nil
	span.needzero = 0
}

func (span *mspan) inList() bool { // 判断mspan是否在列表中
	return span.prev != nil
}

// Initialize an empty doubly-linked list.
func (list *mSpanList) init() {
	list.first = nil
	list.last = &list.first
}

func (list *mSpanList) remove(span *mspan) { // 将mspan从列表中移除
	if span.prev == nil || span.list != list {
		println("failed MSpanList_Remove", span, span.prev, span.list, list)
		throw("MSpanList_Remove")
	}
	if span.next != nil {
		span.next.prev = span.prev
	} else {
		// TODO: After we remove the span.list != list check above,
		// we could at least still check list.last == &span.next here.
		list.last = span.prev
	}
	*span.prev = span.next
	span.next = nil
	span.prev = nil
	span.list = nil
}

func (list *mSpanList) isEmpty() bool { // 判断mspan列表是否为空
	return list.first == nil
}

func (list *mSpanList) insert(span *mspan) { // 将mspan加入到列表头
	if span.next != nil || span.prev != nil || span.list != nil {
		println("failed MSpanList_Insert", span, span.next, span.prev, span.list)
		throw("MSpanList_Insert")
	}
	span.next = list.first
	if list.first != nil {
		list.first.prev = &span.next
	} else {
		list.last = &span.next
	}
	list.first = span
	span.prev = &list.first
	span.list = list
}

func (list *mSpanList) insertBack(span *mspan) { // 将mspan加入到列表尾
	if span.next != nil || span.prev != nil || span.list != nil {
		println("failed MSpanList_InsertBack", span, span.next, span.prev, span.list)
		throw("MSpanList_InsertBack")
	}
	span.next = nil
	span.prev = list.last
	*list.last = span
	list.last = &span.next
	span.list = list
}

const (
	_KindSpecialFinalizer = 1
	_KindSpecialProfile   = 2
	// Note: The finalizer special must be first because if we're freeing
	// an object, a finalizer special will cause the freeing operation
	// to abort, and we want to keep the other special records around
	// if that happens.
)

type special struct { // special结构
	next   *special // linked list in span
	offset uint16   // span offset of object
	kind   byte     // kind of special
}

// Adds the special record s to the list of special records for
// the object p.  All fields of s should be filled in except for
// offset & next, which this routine will fill in.
// Returns true if the special was successfully added, false otherwise.
// (The add will fail only if a record with the same p and s->kind
//  already exists.)
func addspecial(p unsafe.Pointer, s *special) bool {
	span := mheap_.lookupMaybe(p)
	if span == nil {
		throw("addspecial on invalid pointer")
	}

	// Ensure that the span is swept.
	// Sweeping accesses the specials list w/o locks, so we have
	// to synchronize with it. And it's just much safer.
	mp := acquirem()
	span.ensureSwept()

	offset := uintptr(p) - uintptr(span.start<<_PageShift)
	kind := s.kind

	lock(&span.speciallock)

	// Find splice point, check for existing record.
	t := &span.specials
	for {
		x := *t
		if x == nil {
			break
		}
		if offset == uintptr(x.offset) && kind == x.kind {
			unlock(&span.speciallock)
			releasem(mp)
			return false // already exists
		}
		if offset < uintptr(x.offset) || (offset == uintptr(x.offset) && kind < x.kind) {
			break
		}
		t = &x.next
	}

	// Splice in record, fill in offset.
	s.offset = uint16(offset)
	s.next = *t
	*t = s
	unlock(&span.speciallock)
	releasem(mp)

	return true
}

// Removes the Special record of the given kind for the object p.
// Returns the record if the record existed, nil otherwise.
// The caller must FixAlloc_Free the result.
func removespecial(p unsafe.Pointer, kind uint8) *special {
	span := mheap_.lookupMaybe(p)
	if span == nil {
		throw("removespecial on invalid pointer")
	}

	// Ensure that the span is swept.
	// Sweeping accesses the specials list w/o locks, so we have
	// to synchronize with it. And it's just much safer.
	mp := acquirem()
	span.ensureSwept()

	offset := uintptr(p) - uintptr(span.start<<_PageShift)

	lock(&span.speciallock)
	t := &span.specials
	for {
		s := *t
		if s == nil {
			break
		}
		// This function is used for finalizers only, so we don't check for
		// "interior" specials (p must be exactly equal to s->offset).
		if offset == uintptr(s.offset) && kind == s.kind {
			*t = s.next
			unlock(&span.speciallock)
			releasem(mp)
			return s
		}
		t = &s.next
	}
	unlock(&span.speciallock)
	releasem(mp)
	return nil
}

// The described object has a finalizer set for it.
type specialfinalizer struct {
	special special
	fn      *funcval
	nret    uintptr
	fint    *_type
	ot      *ptrtype
}

// Adds a finalizer to the object p.  Returns true if it succeeded.
func addfinalizer(p unsafe.Pointer, f *funcval, nret uintptr, fint *_type, ot *ptrtype) bool {
	lock(&mheap_.speciallock)
	s := (*specialfinalizer)(mheap_.specialfinalizeralloc.alloc())
	unlock(&mheap_.speciallock)
	s.special.kind = _KindSpecialFinalizer
	s.fn = f
	s.nret = nret
	s.fint = fint
	s.ot = ot
	if addspecial(p, &s.special) {
		// This is responsible for maintaining the same
		// GC-related invariants as markrootSpans in any
		// situation where it's possible that markrootSpans
		// has already run but mark termination hasn't yet.
		if gcphase != _GCoff {
			_, base, _ := findObject(p)
			mp := acquirem()
			gcw := &mp.p.ptr().gcw
			// Mark everything reachable from the object
			// so it's retained for the finalizer.
			scanobject(uintptr(base), gcw)
			// Mark the finalizer itself, since the
			// special isn't part of the GC'd heap.
			scanblock(uintptr(unsafe.Pointer(&s.fn)), sys.PtrSize, &oneptrmask[0], gcw)
			if gcBlackenPromptly {
				gcw.dispose()
			}
			releasem(mp)
		}
		return true
	}

	// There was an old finalizer
	lock(&mheap_.speciallock)
	mheap_.specialfinalizeralloc.free(unsafe.Pointer(s))
	unlock(&mheap_.speciallock)
	return false
}

// Removes the finalizer (if any) from the object p.
func removefinalizer(p unsafe.Pointer) {
	s := (*specialfinalizer)(unsafe.Pointer(removespecial(p, _KindSpecialFinalizer)))
	if s == nil {
		return // there wasn't a finalizer to remove
	}
	lock(&mheap_.speciallock)
	mheap_.specialfinalizeralloc.free(unsafe.Pointer(s))
	unlock(&mheap_.speciallock)
}

// The described object is being heap profiled.
type specialprofile struct {
	special special
	b       *bucket
}

// Set the heap profile bucket associated with addr to b.
func setprofilebucket(p unsafe.Pointer, b *bucket) {
	lock(&mheap_.speciallock)
	s := (*specialprofile)(mheap_.specialprofilealloc.alloc())
	unlock(&mheap_.speciallock)
	s.special.kind = _KindSpecialProfile
	s.b = b
	if !addspecial(p, &s.special) {
		throw("setprofilebucket: profile already set")
	}
}

// Do whatever cleanup needs to be done to deallocate s.  It has
// already been unlinked from the MSpan specials list.
func freespecial(s *special, p unsafe.Pointer, size uintptr) {
	switch s.kind {
	case _KindSpecialFinalizer:
		sf := (*specialfinalizer)(unsafe.Pointer(s))
		queuefinalizer(p, sf.fn, sf.nret, sf.fint, sf.ot)
		lock(&mheap_.speciallock)
		mheap_.specialfinalizeralloc.free(unsafe.Pointer(sf))
		unlock(&mheap_.speciallock)
	case _KindSpecialProfile:
		sp := (*specialprofile)(unsafe.Pointer(s))
		mProf_Free(sp.b, size)
		lock(&mheap_.speciallock)
		mheap_.specialprofilealloc.free(unsafe.Pointer(sp))
		unlock(&mheap_.speciallock)
	default:
		throw("bad special kind")
		panic("not reached")
	}
}
