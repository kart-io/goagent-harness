package sandboxtest

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/kart-io/goagent-harness/sandbox"
)

// ErrInjected 是 Faults 注入的错误，代表网络断开：操作的结果未知。
var ErrInjected = errors.New("sandboxtest: 注入的连接错误")

// Faults 包住真实 Provider，模拟本地后端不会出现的远端故障。计数字段表示接下来几次调用失败。
// 它总是实现可选接口，内层没有实现时按 Provider 的基本能力处理。
type Faults struct {
	sandbox.Provider
	// LoseCreateResponse 大于 0 时 Create 照常分配实例，调用方只收到错误。
	LoseCreateResponse atomic.Int32
	// FailCreate 大于 0 时 Create 没有分配实例就失败，但调用方不知道。
	FailCreate atomic.Int32
	// FailConnect、FailFind、FailDestroy 大于 0 时对应调用失败。
	FailConnect, FailFind, FailDestroy atomic.Int32
}

func consume(counter *atomic.Int32) bool {
	for {
		n := counter.Load()
		if n <= 0 {
			return false
		}
		if counter.CompareAndSwap(n, n-1) {
			return true
		}
	}
}

func (f *Faults) Create(ctx context.Context, r sandbox.Request) (sandbox.Environment, error) {
	if consume(&f.FailCreate) {
		return nil, ErrInjected
	}
	e, err := f.Provider.Create(ctx, r)
	if err == nil && consume(&f.LoseCreateResponse) {
		_ = e.Close(ctx)
		return nil, ErrInjected
	}
	return e, err
}

func (f *Faults) Connect(ctx context.Context, id string, r sandbox.Request) (sandbox.Environment, error) {
	if consume(&f.FailConnect) {
		return nil, ErrInjected
	}
	return f.Provider.Connect(ctx, id, r)
}

func (f *Faults) FindByScope(ctx context.Context, r sandbox.Request) (string, bool, error) {
	if consume(&f.FailFind) {
		return "", false, ErrInjected
	}
	finder, ok := f.Provider.(sandbox.InstanceFinder)
	if !ok {
		return "", false, sandbox.ErrUnsupported
	}
	return finder.FindByScope(ctx, r)
}

func (f *Faults) DestroyInstance(ctx context.Context, id string, r sandbox.Request) error {
	if consume(&f.FailDestroy) {
		return ErrInjected
	}
	if destroyer, ok := f.Provider.(sandbox.InstanceDestroyer); ok {
		return destroyer.DestroyInstance(ctx, id, r)
	}
	e, err := f.Provider.Connect(ctx, id, r)
	if err != nil {
		return err
	}
	return e.Destroy(ctx)
}
