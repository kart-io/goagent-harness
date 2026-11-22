package planning

import (
	"fmt"
	"reflect"

	"github.com/kart-io/goagent/codec/json"
)

func initialCapacityStop() State {
	return blocked(State{}, "初始规划输入超出状态大小上限")
}

func checkInitialCapacity(c Config) error {
	data, err := json.Marshal(initialCapacityStop())
	if err != nil {
		return err
	}
	if len(data) > c.MaxStateBytes {
		return configError("MaxStateBytes 至少需要 %d 字节以保存初始阻塞结果", len(data))
	}
	return nil
}

// PrepareInitialState bounds input before DurableGraph.Run writes its first
// checkpoint. Hosts should call it in GraphFactory.Initial or before Run.
// Oversize fresh goal/request input becomes a small terminal business outcome.
// An oversize state with planning history is returned intact with an error.
func PrepareInitialState(s State, c Config) (State, error) {
	c, err := NormalizeConfig(c)
	if err != nil {
		return s, err
	}
	if err = checkInitialCapacity(c); err != nil {
		return s, err
	}
	next, err := prepareInitial(s, c)
	if err != nil {
		return s, err
	}
	return next.Clone(), nil
}
func prepareInitial(s State, c Config) (State, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return s, err
	}
	if len(data) <= c.MaxStateBytes {
		return s, nil
	}
	if !freshInitialState(s) {
		return s, fmt.Errorf("已提交的规划检查点超出状态大小上限 %d 字节", c.MaxStateBytes)
	}
	return initialCapacityStop(), nil
}

// Only the initial input envelope can be discarded. New state fields are
// retained by default and therefore cannot accidentally erase future history.
func freshInitialState(s State) bool {
	s.Request = nil
	s.Goal = GoalSpec{}
	return reflect.DeepEqual(s, State{})
}
