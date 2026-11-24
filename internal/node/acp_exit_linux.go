package node

import "golang.org/x/sys/unix"

// waitACPExit observes the child's exit while keeping it waitable by cmd.Wait.
func waitACPExit(pid int) bool {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err != unix.EINTR {
			return err == nil
		}
	}
}
