//go:build linux

package agent

import (
	"context"
	"golang.org/x/sys/unix"
)

// Kernel notifications invalidate measurements when an interface, address or
// route changes. Polling remains a fallback when netlink is unavailable.
func networkChanges(ctx context.Context) <-chan struct{} {
	updates := make(chan struct{}, 1)
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.NETLINK_ROUTE)
	if err != nil {
		return updates
	}
	groups := uint32(unix.RTMGRP_LINK | unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV6_IFADDR | unix.RTMGRP_IPV4_ROUTE | unix.RTMGRP_IPV6_ROUTE)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: groups}); err != nil {
		unix.Close(fd)
		return updates
	}
	go func() {
		defer unix.Close(fd)
		buffer := make([]byte, 65536)
		for ctx.Err() == nil {
			poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			n, err := unix.Poll(poll, 1000)
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				return
			}
			if n == 0 {
				continue
			}
			_, _, err = unix.Recvfrom(fd, buffer, 0)
			if err != nil && err != unix.ENOBUFS {
				continue
			}
			select {
			case updates <- struct{}{}:
			default:
			}
		}
	}()
	return updates
}
