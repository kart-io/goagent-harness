// Package eval plans isolated trials and grades answers against frozen synthetic materials.
package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Source is a public document supplied to the answerer.
type Source struct {
	ID      string          `json:"id"`
	Title   string          `json:"title"`
	Content json.RawMessage `json:"content"`
}

// Locator identifies a JSON value inside a source.
type Locator struct {
	SourceID string `json:"source_id"`
	Pointer  string `json:"json_pointer"`
}

// Rubric holds private grading labels.
type Rubric struct {
	Version     string                     `json:"version"`
	Facts       map[string]json.RawMessage `json:"expected_facts"`
	Required    []Locator                  `json:"required_source_locators"`
	MustAbstain bool                       `json:"must_abstain"`
	Effects     []string                   `json:"expected_final_effects"`
}

// UnmarshalJSON requires every rubric to explicitly label abstention.
func (r *Rubric) UnmarshalJSON(data []byte) error {
	type fields Rubric
	var raw struct {
		fields
		Abstain *bool `json:"must_abstain"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Abstain == nil {
		return fmt.Errorf("缺少拒答标签")
	}
	*r = Rubric(raw.fields)
	r.MustAbstain = *raw.Abstain
	return nil
}

// Case combines public material with independent private labels.
type Case struct {
	ID           string   `json:"id"`
	Category     string   `json:"category"`
	Request      string   `json:"request"`
	Sources      []Source `json:"sources"`
	OutputFields []string `json:"output_fields"`
	Rubric       Rubric   `json:"rubric"`
}

// Dataset holds a versioned synthetic question set.
type Dataset struct {
	Version   string `json:"version"`
	Synthetic bool   `json:"synthetic"`
	Cases     []Case `json:"cases"`
	digest    string
}

// Load reads and validates the frozen dataset.
func Load(path string) (Dataset, error) {
	var d Dataset
	b, e := os.ReadFile(path)
	if e != nil {
		return d, e
	}
	if e = json.Unmarshal(b, &d); e != nil {
		return d, e
	}
	if d.Version == "" || !d.Synthetic || len(d.Cases) == 0 {
		return d, fmt.Errorf("缺少合成题集元数据")
	}
	d.digest = fmt.Sprintf("%x", sha256.Sum256(b))
	ids := map[string]bool{}
	for _, c := range d.Cases {
		if c.ID == "" || ids[c.ID] || c.Request == "" || len(c.OutputFields) == 0 || len(c.Sources) == 0 || c.Rubric.Version == "" || c.Rubric.Facts == nil || c.Rubric.Required == nil || c.Rubric.Effects == nil {
			return d, fmt.Errorf("题目字段错误 %q", c.ID)
		}
		ids[c.ID] = true
		seen := map[string]bool{}
		for _, s := range c.Sources {
			if seen[s.ID] || s.ID == "" || !json.Valid(s.Content) {
				return d, fmt.Errorf("材料错误 %s", c.ID)
			}
			seen[s.ID] = true
		}
		for k := range c.Rubric.Facts {
			if !slices.Contains(c.OutputFields, k) {
				return d, fmt.Errorf("缺少输出字段 %s", k)
			}
		}
		for _, l := range c.Rubric.Required {
			if _, ok := lookup(c, l); !ok {
				return d, fmt.Errorf("材料定位失败 %s", c.ID)
			}
		}
	}
	return d, nil
}
func value(b json.RawMessage) any {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var x any
	if dec.Decode(&x) != nil {
		return nil
	}
	return x
}
func canonical(x any) any {
	switch v := x.(type) {
	case json.Number:
		r, ok := new(big.Rat).SetString(string(v))
		if ok {
			return r
		}
	case []any:
		for i, n := range v {
			v[i] = canonical(n)
		}
	case map[string]any:
		for k, n := range v {
			v[k] = canonical(n)
		}
	}
	return x
}
func equalJSON(a, b json.RawMessage) bool {
	return json.Valid(a) && json.Valid(b) && reflect.DeepEqual(canonical(value(a)), canonical(value(b)))
}
func lookup(c Case, l Locator) (any, bool) {
	for _, s := range c.Sources {
		if s.ID != l.SourceID {
			continue
		}
		x := value(s.Content)
		if l.Pointer == "" {
			return x, true
		}
		if !strings.HasPrefix(l.Pointer, "/") {
			break
		}
		for _, p := range strings.Split(l.Pointer[1:], "/") {
			p = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
			switch v := x.(type) {
			case map[string]any:
				var ok bool
				x, ok = v[p]
				if !ok {
					return nil, false
				}
			case []any:
				i, e := strconv.Atoi(p)
				if e != nil || strconv.Itoa(i) != p || i < 0 || i >= len(v) {
					return nil, false
				}
				x = v[i]
			default:
				return nil, false
			}
		}
		return x, true
	}
	return nil, false
}
