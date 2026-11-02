package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path"
	"strconv"
	"time"

	"github.com/kart-io/goagent/harness/env"
)

const (
	defaultStateDir     = "/tmp/.goagent-commands"
	defaultMaxOutput    = 4 << 20
	startUnsupportedRC  = 78
	statusNoStateRC     = 3
	statusStoppedRC     = 4
	statusRunningRC     = 5
	maxRequestFailures  = 5
	maxPollInterval     = time.Second
	defaultPollInterval = 100 * time.Millisecond
	defaultRequestLimit = 15 * time.Second
	defaultLostGrace    = 3 * time.Second
)

// 同一个键的状态目录由 mkdir 原子创建，所以启动请求可以放心重试：第二次只会发现目录已存在。
const startScript = `d=$1; t=$2; n=$3; shift 3
for tool in setsid flock mkfifo head; do command -v "$tool" >/dev/null 2>&1 || exit 78; done
mkdir -p "$(dirname "$d")" || exit 79
mkdir "$d" 2>/dev/null || { [ -d "$d" ] && exit 0; exit 79; }
setsid sh -c "$SUPERVISOR" sh "$d" "$t" "$n" "$@" </dev/null >/dev/null 2>&1 &
exit 0
`

// supervisorScript 持有 lock 直到写完结果，所以“锁已释放且没有结果”就说明进程没了。
// 状态查询会短暂拿锁，这里要等；mkdir 已保证同键只有一个监管进程。
// 命令和看门狗都关闭 fd 9，否则留在后台的子进程会一直占着锁。
const supervisorScript = `d=$1; t=$2; n=$3; shift 3
exec 9>"$d/lock"
flock 9 || exit 1
echo $$ >"$d/supervisor"
mkfifo "$d/fifo"
{ head -c "$n" >"$d/out"; cat >/dev/null; } <"$d/fifo" 9>&- &
r=$!
setsid "$@" >"$d/fifo" 2>&1 </dev/null 9>&- &
c=$!
echo "$c" >"$d/pid.tmp"; mv "$d/pid.tmp" "$d/pid"
(
	sleep "$t" & s=$!
	trap 'kill "$s" 2>/dev/null; exit 0' TERM
	wait "$s" || exit 0
	: >"$d/timedout"
	kill -KILL -"$c" 2>/dev/null
) </dev/null >/dev/null 2>&1 9>&- &
w=$!
wait "$c"; rc=$?
wait "$r"
kill "$w" 2>/dev/null
rm -f "$d/fifo"
echo "$rc" >"$d/exit.tmp"; mv "$d/exit.tmp" "$d/exit"
`

// 先看锁再看结果：锁释放之前结果已经写好，反过来查会把刚结束的命令误判为丢失。
const statusScript = `d=$1
[ -d "$d" ] || exit 3
if flock -n "$d/lock" true 2>/dev/null; then
	[ -f "$d/exit" ] || exit 4
	timed=0; [ -e "$d/timedout" ] && timed=1
	echo "$(cat "$d/exit") $timed"
	cat "$d/out" 2>/dev/null
	exit 0
fi
exit 5
`

const cancelScript = `d=$1
[ -d "$d" ] || exit 3
i=0
while [ ! -f "$d/pid" ] && [ ! -f "$d/exit" ] && [ "$i" -lt 30 ]; do sleep 0.1; i=$((i+1)); done
[ -f "$d/exit" ] && exit 0
[ -f "$d/pid" ] && kill -KILL -"$(cat "$d/pid")" 2>/dev/null
exit 0
`

// CommandSupervisor 用沙箱内的 POSIX 工具（sh、setsid、flock、mkfifo、head）实现 KeyedExecutor，不需要 Provider 的原生能力。
// 状态放在 StateDir，随实例销毁；实例长期保留时由实例所有者清理。Exec 启动的进程必须能在调用返回后继续存活。
type CommandSupervisor struct {
	Exec PolicyExecutor
	// StateDir 须在工作区之外，默认 /tmp/.goagent-commands。
	StateDir string
	// Shell 默认 sh。
	Shell string
	// 以下默认值适合远端后端。
	PollInterval, RequestTimeout, LostGrace time.Duration
}

func (s *CommandSupervisor) stateDir(key string) string {
	dir := s.StateDir
	if dir == "" {
		dir = defaultStateDir
	}
	sum := sha256.Sum256([]byte(key))
	return path.Join(dir, hex.EncodeToString(sum[:])[:32])
}

func (s *CommandSupervisor) script(ctx context.Context, dir string, p Policy, script string, args ...string) (env.Result, error) {
	timeout := s.RequestTimeout
	if timeout <= 0 {
		timeout = defaultRequestLimit
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	shell := s.Shell
	if shell == "" {
		shell = "sh"
	}
	return s.Exec.RunWithPolicy(requestCtx, env.Command{Argv: append([]string{shell, "-c", script, "sh"}, args...), Dir: dir}, p)
}

// KeyDir 返回这个键的状态目录，供需要先做后端专有处理的 Provider 使用。
func (s *CommandSupervisor) KeyDir(key string) string { return s.stateDir(key) }

// RunKeyed 启动或附着命令，等到它结束。
func (s *CommandSupervisor) RunKeyed(ctx context.Context, c KeyedCommand, p Policy) (KeyedResult, error) {
	if c.Key == "" || len(c.Command.Argv) == 0 {
		return KeyedResult{}, errors.New("sandbox: keyed command requires key and argv")
	}
	dir := s.stateDir(c.Key)
	if !c.AttachOnly {
		if err := s.start(ctx, dir, c, p); err != nil {
			return KeyedResult{}, err
		}
	}
	return s.wait(ctx, dir, c.Key, p)
}

func (s *CommandSupervisor) start(ctx context.Context, dir string, c KeyedCommand, p Policy) error {
	limit := c.MaxOutput
	if limit <= 0 {
		limit = defaultMaxOutput
	}
	seconds := int(math.Ceil(c.Timeout.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	args := append([]string{dir, strconv.Itoa(seconds), strconv.Itoa(limit)}, c.Command.Argv...)
	var last error
	for range maxRequestFailures {
		res, err := s.script(ctx, c.Command.Dir, p, "SUPERVISOR="+shellQuote(supervisorScript)+"\n"+startScript, args...)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			last = err
		case res.ExitCode == 0:
			return nil
		case res.ExitCode == startUnsupportedRC || res.ExitCode == 127:
			return fmt.Errorf("%w: command supervisor needs sh, setsid, flock, mkfifo and head", ErrUnsupported)
		default:
			last = fmt.Errorf("启动命令监管失败，退出码 %d: %s", res.ExitCode, res.Output)
		}
	}
	return fmt.Errorf("%w: command %s: %v", ErrUnconfirmed, c.Key, last)
}

func (s *CommandSupervisor) wait(ctx context.Context, dir, key string, p Policy) (KeyedResult, error) {
	interval := s.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	grace := s.LostGrace
	if grace <= 0 {
		grace = defaultLostGrace
	}
	var stoppedAt time.Time
	failures := 0
	for {
		res, err := s.script(ctx, ".", p, statusScript, dir)
		switch {
		case ctx.Err() != nil:
			return KeyedResult{}, ctx.Err()
		case err != nil:
			if failures++; failures >= maxRequestFailures {
				return KeyedResult{}, fmt.Errorf("%w: command %s: %v", ErrUnconfirmed, key, err)
			}
		case res.ExitCode == 0:
			return parseStatus(res.Output, key)
		case res.ExitCode == statusNoStateRC:
			return KeyedResult{}, fmt.Errorf("%w: %s", ErrCommandLost, key)
		case res.ExitCode == statusStoppedRC:
			// 监管进程刚启动时还没拿到锁，留一点时间再下结论。
			failures = 0
			if stoppedAt.IsZero() {
				stoppedAt = time.Now()
			} else if time.Since(stoppedAt) > grace {
				return KeyedResult{}, fmt.Errorf("%w: %s", ErrCommandLost, key)
			}
		case res.ExitCode == statusRunningRC:
			failures, stoppedAt = 0, time.Time{}
		default:
			if failures++; failures >= maxRequestFailures {
				return KeyedResult{}, fmt.Errorf("%w: command %s: 查询退出码 %d: %s", ErrUnconfirmed, key, res.ExitCode, res.Output)
			}
		}
		select {
		case <-ctx.Done():
			return KeyedResult{}, ctx.Err()
		case <-time.After(interval):
		}
		if interval *= 2; interval > maxPollInterval {
			interval = maxPollInterval
		}
	}
}

func parseStatus(output []byte, key string) (KeyedResult, error) {
	line, body, ok := bytes.Cut(output, []byte("\n"))
	fields := bytes.Fields(line)
	if !ok || len(fields) != 2 {
		return KeyedResult{}, fmt.Errorf("%w: command %s: 状态格式错误", ErrUnconfirmed, key)
	}
	code, err := strconv.Atoi(string(fields[0]))
	if err != nil {
		return KeyedResult{}, fmt.Errorf("%w: command %s: 状态格式错误", ErrUnconfirmed, key)
	}
	return KeyedResult{Result: env.Result{Output: body, ExitCode: code}, TimedOut: string(fields[1]) == "1"}, nil
}

// CancelKeyed 终止命令及其进程组，已结束的命令不受影响。
func (s *CommandSupervisor) CancelKeyed(ctx context.Context, key string, p Policy) (bool, error) {
	res, err := s.script(ctx, ".", p, cancelScript, s.stateDir(key))
	if err != nil {
		return false, err
	}
	switch res.ExitCode {
	case 0:
		return true, nil
	case statusNoStateRC:
		return false, nil
	}
	return false, fmt.Errorf("取消命令失败，退出码 %d: %s", res.ExitCode, res.Output)
}
