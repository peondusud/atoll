package value

import (
	"errors"
	"fmt"
	"strings"
)

// MinFree is the min_free setting: what to leave free on each disk (specs.md
// §13.1, §29.5).
type MinFree struct {
	sizes    []int64
	percents []Percent
}

// ParseMinFree parses a list of one or more sizes and percentages.
func ParseMinFree(values []string) (MinFree, error) {
	if len(values) == 0 {
		return MinFree{}, errors.New("min_free: want at least one size or percentage")
	}
	var m MinFree
	for _, v := range values {
		if strings.HasSuffix(v, "%") {
			p, err := ParsePercent(v)
			if err != nil {
				return MinFree{}, fmt.Errorf("min_free: %w", err)
			}
			m.percents = append(m.percents, p)
			continue
		}
		s, err := ParseSize(v)
		if err != nil {
			return MinFree{}, fmt.Errorf("min_free: %w", err)
		}
		m.sizes = append(m.sizes, s)
	}
	return m, nil
}

// Reserved returns what min_free keeps free on a disk of this capacity: the
// smallest of its values. An error means that capacity is negative.
func (m MinFree) Reserved(capacity int64) (int64, error) {
	if capacity < 0 {
		return 0, fmt.Errorf("min_free: negative capacity %d", capacity)
	}
	first := true
	var reserved int64
	for _, s := range m.sizes {
		if first || s < reserved {
			reserved, first = s, false
		}
	}
	for _, p := range m.percents {
		if s := p.Of(capacity); first || s < reserved {
			reserved, first = s, false
		}
	}
	return reserved, nil
}

// SmallestSize returns the smallest of the sizes, which holds whatever the
// capacity; false when there are only percentages.
func (m MinFree) SmallestSize() (int64, bool) {
	if len(m.sizes) == 0 {
		return 0, false
	}
	smallest := m.sizes[0]
	for _, s := range m.sizes[1:] {
		smallest = min(smallest, s)
	}
	return smallest, true
}

// Usable returns what the solver may fill on a disk of this capacity: the
// capacity minus the reserve, or 0 if the reserve is larger.
func (m MinFree) Usable(capacity int64) (int64, error) {
	reserved, err := m.Reserved(capacity)
	if err != nil {
		return 0, err
	}
	return max(capacity-reserved, 0), nil
}
