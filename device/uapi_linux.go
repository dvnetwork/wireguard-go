/* SPDX-License-Identifier: GPL-2.0
 *
 * Copyright (C) 2017-2018 WireGuard LLC. All Rights Reserved.
 */

package device

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path"

	"github.com/dvnetwork/wireguard-go/rwcancel"
	"golang.org/x/sys/unix"
)

var socketDirectory = "/var/run/wireguard"

type UAPIListener struct {
	listener        net.Listener // unix socket listener
	connNew         chan net.Conn
	connErr         chan error
	inotifyFd       int
	inotifyRWCancel *rwcancel.RWCancel
}

func (l *UAPIListener) Accept() (net.Conn, error) {
	for {
		select {
		case conn := <-l.connNew:
			return conn, nil

		case err := <-l.connErr:
			return nil, err
		}
	}
}

func (l *UAPIListener) Close() error {
	err1 := unix.Close(l.inotifyFd)
	err2 := l.inotifyRWCancel.Cancel()
	err3 := l.listener.Close()
	if err1 != nil {
		return err1
	}
	if err2 != nil {
		return err2
	}
	return err3
}

func (l *UAPIListener) Addr() net.Addr {
	return l.listener.Addr()
}

func UAPIListen(name string, file *os.File) (net.Listener, error) {

	// wrap file in listener

	listener, err := net.FileListener(file)
	if err != nil {
		return nil, err
	}

	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(true)
	}

	uapi := &UAPIListener{
		listener: listener,
		connNew:  make(chan net.Conn, 1),
		connErr:  make(chan error, 1),
	}

	// watch for deletion of socket

	socketPath := path.Join(
		socketDirectory,
		fmt.Sprintf(socketName, name),
	)

	uapi.inotifyFd, err = unix.InotifyInit()
	if err != nil {
		return nil, err
	}

	_, err = unix.InotifyAddWatch(
		uapi.inotifyFd,
		socketPath,
		unix.IN_ATTRIB|
			unix.IN_DELETE|
			unix.IN_DELETE_SELF,
	)

	if err != nil {
		return nil, err
	}

	uapi.inotifyRWCancel, err = rwcancel.NewRWCancel(uapi.inotifyFd)
	if err != nil {
		unix.Close(uapi.inotifyFd)
		return nil, err
	}

	go func(l *UAPIListener) {
		var buff [0]byte
		for {
			// start with lstat to avoid race condition
			if _, err := os.Lstat(socketPath); os.IsNotExist(err) {
				l.connErr <- err
				return
			}
			_, err := uapi.inotifyRWCancel.Read(buff[:])
			if err != nil {
				l.connErr <- err
				return
			}
		}
	}(uapi)

	// watch for new connections

	go func(l *UAPIListener) {
		for {
			conn, err := l.listener.Accept()
			if err != nil {
				l.connErr <- err
				break
			}
			l.connNew <- conn
		}
	}(uapi)

	return uapi, nil
}

func UAPIOpen(name string) (*os.File, error) {

	// check if path exist

	err := os.MkdirAll(socketDirectory, 0755)
	if err != nil && !os.IsExist(err) {
		return nil, err
	}

	// open UNIX socket

	socketPath := path.Join(
		socketDirectory,
		fmt.Sprintf(socketName, name),
	)

	addr, err := net.ResolveUnixAddr("unix", socketPath)
	if err != nil {
		return nil, err
	}

	oldUmask := unix.Umask(0077)
	listener, err := func() (*net.UnixListener, error) {

		// initial connection attempt

		listener, err := net.ListenUnix("unix", addr)
		if err == nil {
			return listener, nil
		}

		// check if socket already active

		_, err = net.Dial("unix", socketPath)
		if err == nil {
			return nil, errors.New("unix socket in use")
		}

		// cleanup & attempt again

		err = os.Remove(socketPath)
		if err != nil {
			return nil, err
		}
		return net.ListenUnix("unix", addr)
	}()
	unix.Umask(oldUmask)

	if err != nil {
		return nil, err
	}

	return listener.File()
}
