package main

import "golang.org/x/sys/unix"

// waitACPExit observes the child's exit while keeping it waitable by cmd.Wait.
func waitACPExit(pid int) bool {
	queue, err := unix.Kqueue()
	if err != nil {
		return false
	}
	defer unix.Close(queue)
	var watch unix.Kevent_t
	unix.SetKevent(&watch, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	watch.Fflags = unix.NOTE_EXIT
	if _, err := unix.Kevent(queue, []unix.Kevent_t{watch}, nil, nil); err != nil {
		return err == unix.ESRCH // The child exited before registration.
	}
	var event [1]unix.Kevent_t
	for {
		n, err := unix.Kevent(queue, nil, event[:], nil)
		if err != unix.EINTR {
			return err == nil && n == 1 && event[0].Fflags&unix.NOTE_EXIT != 0
		}
	}
}
