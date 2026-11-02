//go:build linux

package sandbox

import (
	"fmt"
	"os/exec"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// ABI 3 起才能拦截 truncate，更低的版本兑现不了写入边界。
	landlockMinABI = 3
	// ABI 6 起才能阻止向沙箱外的进程发信号；没有 PID 命名空间时只能靠它。
	landlockFullABI = 6

	landlockWriteAccess = unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM | unix.LANDLOCK_ACCESS_FS_REFER | unix.LANDLOCK_ACCESS_FS_TRUNCATE
)

func landlockABI() int {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0
	}
	return int(abi)
}

// startLandlock 只限制一条锁定的线程，再从它启动命令：Landlock 和 seccomp 按线程继承，宿主其他线程不受影响。
// 这条 goroutine 退出时不解锁，运行时会销毁线程，受限线程不会被复用。
func startLandlock(cmd *exec.Cmd, p Policy, privateTemp string) error {
	filter, err := socketFilterProgram(p)
	if err != nil {
		return err
	}
	var writable []string
	if p.Mode == WorkspaceWrite {
		writable = append([]string{p.WorkspaceRoot, privateTemp}, p.AdditionalWritableRoots...)
	}
	// 没有 PID 命名空间隔离，新会话用来脱离宿主的控制终端，它同时也是新进程组。
	cmd.SysProcAttr.Setpgid = false
	cmd.SysProcAttr.Setsid = true
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := confineThread(p.Mode != DangerFullAccess, writable, filter); err != nil {
			started <- fmt.Errorf("%w: landlock: %v", ErrUnavailable, err)
			return
		}
		started <- cmd.Start()
	}()
	return <-started
}

func confineThread(restrictWrites bool, writable []string, filter []unix.SockFilter) error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	if restrictWrites {
		if err := restrictThreadWrites(writable); err != nil {
			return err
		}
	}
	if len(filter) == 0 {
		return nil
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0)
	runtime.KeepAlive(filter)
	return err
}

func restrictThreadWrites(writable []string) error {
	abi := landlockABI()
	if abi < landlockMinABI {
		return fmt.Errorf("kernel ABI %d is below %d", abi, landlockMinABI)
	}
	attr := unix.LandlockRulesetAttr{Handled_access_fs: landlockWriteAccess}
	if abi >= landlockFullABI {
		attr.Scoped = unix.LANDLOCK_SCOPE_SIGNAL | unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET
	}
	ruleset, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("create ruleset: %w", errno)
	}
	defer func() { _ = unix.Close(int(ruleset)) }()
	if err := addLandlockRule(int(ruleset), "/dev/null", unix.LANDLOCK_ACCESS_FS_WRITE_FILE); err != nil {
		return err
	}
	for _, root := range writable {
		if err := addLandlockRule(int(ruleset), root, landlockWriteAccess); err != nil {
			return err
		}
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, ruleset, 0, 0); errno != 0 {
		return fmt.Errorf("restrict: %w", errno)
	}
	return nil
}

func addLandlockRule(ruleset int, path string, access uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = unix.Close(fd) }()
	rule := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)}
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("add rule %s: %w", path, errno)
	}
	return nil
}
