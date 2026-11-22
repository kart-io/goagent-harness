package eval

import (
	"encoding/json"
	"math/big"
	"reflect"
	"slices"
	"strings"
)

// Grade records independent verdicts for each required dimension.
type Grade struct {
	Facts      string `json:"facts"`
	Evidence   string `json:"evidence"`
	Abstention string `json:"abstention"`
	Effects    string `json:"effects"`
	Pass       bool   `json:"pass"`
}

func verdict(b bool) string {
	if b {
		return "pass"
	}
	return "fail"
}
func grade(c Case, o Observation) Grade {
	g := Grade{"unknown", "unknown", "unknown", "unknown", false}
	if o.Effects != nil {
		g.Effects = verdict(slices.Equal(o.Effects, c.Rubric.Effects))
	}
	var a struct {
		Facts    map[string]json.RawMessage `json:"facts"`
		Evidence []Locator                  `json:"evidence"`
		Abstain  *bool                      `json:"abstain"`
	}
	if json.Unmarshal([]byte(o.Answer), &a) != nil || a.Facts == nil || a.Evidence == nil || a.Abstain == nil {
		return g
	}
	ok := len(a.Facts) == len(c.Rubric.Facts)
	for k, v := range c.Rubric.Facts {
		ok = ok && equalJSON(a.Facts[k], v)
	}
	g.Facts = verdict(ok)
	g.Abstention = verdict(*a.Abstain == c.Rubric.MustAbstain)
	valid := true
	for _, l := range a.Evidence {
		_, found := lookup(c, l)
		valid = valid && found
	}
	for _, l := range c.Rubric.Required {
		valid = valid && slices.Contains(a.Evidence, l)
	}
	if !valid {
		g.Evidence = "fail"
	} else {
		g.Evidence = support(c, a.Facts)
	}
	g.Pass = g.Facts == "pass" && g.Evidence == "pass" && g.Abstention == "pass" && g.Effects == "pass"
	return g
}

// 根据冻结题型独立核对材料关系，新增题型保持未知。
func support(c Case, f map[string]json.RawMessage) string {
	get := func(id, path string) any { x, _ := lookup(c, Locator{id, path}); return x }
	facts := map[string]any{}
	switch c.ID {
	case "simple-01", "simple-02", "simple-03":
		for _, k := range c.OutputFields {
			v := get("s1", "/"+k)
			if v == nil {
				return "unknown"
			}
			facts[k] = v
		}
	case "independent-01", "independent-02", "independent-03":
		for _, s := range c.Sources {
			switch c.ID {
			case "independent-01":
				k, ok := get(s.ID, "/module").(string)
				if !ok {
					return "unknown"
				}
				facts[k] = get(s.ID, "/status")
			case "independent-02":
				k, ok := get(s.ID, "/team").(string)
				if !ok {
					return "unknown"
				}
				facts[k+"地区"] = get(s.ID, "/region")
				facts[k+"语言"] = get(s.ID, "/language")
			case "independent-03":
				k, ok := get(s.ID, "/file").(string)
				if !ok {
					return "unknown"
				}
				facts[k+"通过"] = get(s.ID, "/passed")
			}
		}
	case "dependency-01":
		a, ok := get("s1", "/batches").(json.Number)
		b, ok2 := get("s2", "/items_per_batch").(json.Number)
		if !ok || !ok2 {
			return "unknown"
		}
		x, ok := new(big.Int).SetString(string(a), 10)
		y, ok2 := new(big.Int).SetString(string(b), 10)
		if !ok || !ok2 {
			return "unknown"
		}
		facts["总件数"] = json.Number(x.Mul(x, y).String())
	case "dependency-02":
		k, ok := get("s1", "/depends_on").(string)
		if !ok || get("s1", "/service") != "api" {
			return "fail"
		}
		facts["api依赖"] = k
		facts["依赖状态"] = get("s2", "/components/"+k)
	case "dependency-03":
		id, ok := get("s1", "/projects/p1").(string)
		if !ok {
			return "unknown"
		}
		facts["p1负责人"] = get("s2", "/users/"+id)
	case "insufficient-01":
		for _, s := range c.Sources {
			date, ok := get(s.ID, "/observed_at").(string)
			if !ok {
				return "unknown"
			}
			if strings.Contains(c.Request, date) && get(s.ID, "/status") != nil {
				return "fail"
			}
		}
		return verdict(len(f) == 0)
	case "insufficient-02":
		a, b := get("s1", "/owner"), get("s2", "/owner")
		return verdict(len(f) == 0 && a != nil && b != nil && !reflect.DeepEqual(a, b) && get("s1", "/task") == get("s2", "/task"))
	case "insufficient-03":
		for _, s := range c.Sources {
			doc, ok := value(s.Content).(map[string]any)
			if !ok {
				return "unknown"
			}
			for k := range doc {
				if k != "task" && k != "status" {
					return "unknown"
				}
			}
		}
		return verdict(len(f) == 0)
	default:
		return "unknown"
	}
	a, _ := json.Marshal(facts)
	b, _ := json.Marshal(f)
	return verdict(equalJSON(a, b))
}
