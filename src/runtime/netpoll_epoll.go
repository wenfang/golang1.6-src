// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build linux

package runtime

import "unsafe"

func epollcreate(size int32) int32
func epollcreate1(flags int32) int32

//go:noescape
func epollctl(epfd, op, fd int32, ev *epollevent) int32

//go:noescape
func epollwait(epfd int32, ev *epollevent, nev, timeout int32) int32
func closeonexec(fd int32)

var (
	epfd int32 = -1 // epoll descriptor 全局的epoll描述符
)

func netpollinit() { // 初始化epoll,创建epoll句柄并设置close_on_exec
	epfd = epollcreate1(_EPOLL_CLOEXEC) // 创建epfd
	if epfd >= 0 {                      // 创建epfd成功
		return
	}
	epfd = epollcreate(1024)
	if epfd >= 0 {
		closeonexec(epfd)
		return
	}
	println("netpollinit: failed to create epoll descriptor", -epfd)
	throw("netpollinit: failed to create descriptor")
}

func netpollopen(fd uintptr, pd *pollDesc) int32 { // 打开fd句柄
	var ev epollevent                                         // 声明一个epollevent结构
	ev.events = _EPOLLIN | _EPOLLOUT | _EPOLLRDHUP | _EPOLLET // 设置监听事件
	*(**pollDesc)(unsafe.Pointer(&ev.data)) = pd              // 将pollDesc设置到ev.data中
	return -epollctl(epfd, _EPOLL_CTL_ADD, int32(fd), &ev)    // 执行epoll_ctl监听所有事件
}

func netpollclose(fd uintptr) int32 { // 删除fd对应的epoll事件
	var ev epollevent
	return -epollctl(epfd, _EPOLL_CTL_DEL, int32(fd), &ev) // 删除对fd的事件监听
}

func netpollarm(pd *pollDesc, mode int) {
	throw("unused")
}

// 返回变为可运行状态的goroutine的列表,block设置是否为阻塞等待
// polls for ready network connections
// returns list of goroutines that become runnable
func netpoll(block bool) *g {
	if epfd == -1 { // 没有设置epoll句柄，返回
		return nil
	}
	waitms := int32(-1) // 如果阻塞等待，将等待时间设置为-1，永久阻塞
	if !block {         // 如果不阻塞，立刻返回
		waitms = 0 // 设置立即返回
	}
	var events [128]epollevent // 最多等待128个事件
retry:
	n := epollwait(epfd, &events[0], int32(len(events)), waitms) // 执行epoll_wait
	if n < 0 {                                                   // 执行出错
		if n != -_EINTR { // 被打断，但不是被信号打断的，抛出异常, epollwait failed
			println("runtime: epollwait on fd", epfd, "failed with", -n)
			throw("epollwait failed")
		}
		goto retry // 无论出现什么错误都跳转到retry执行,区别是除了EINTR错误会打印一次提示
	}
	var gp guintptr
	for i := int32(0); i < n; i++ { // 遍历所有的事件
		ev := &events[i]    // 取回事件结构
		if ev.events == 0 { // 如果未发生事件，继续下一个
			continue
		}
		var mode int32                                                 // 根据模式设置读或者写
		if ev.events&(_EPOLLIN|_EPOLLRDHUP|_EPOLLHUP|_EPOLLERR) != 0 { // 如果产生了读事件
			mode += 'r' // 设置读模式
		}
		if ev.events&(_EPOLLOUT|_EPOLLHUP|_EPOLLERR) != 0 { // 如果产生了写事件
			mode += 'w' // 设置写模式
		}
		if mode != 0 { // 如果发生了某种事件
			pd := *(**pollDesc)(unsafe.Pointer(&ev.data)) // 取出来pollDesc结构

			netpollready(&gp, pd, mode) // 调用netpollready，由gp指针返回goroutine
		}
	}
	if block && gp == 0 { // 如果是阻塞模式，但是没有发现goroutine
		goto retry // 转到retry，重试
	}
	return gp.ptr() // 返回变为可用状态的gorutine列表的指针
}
