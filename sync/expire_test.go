package sync

import (
	"testing"
	"time"

	"github.com/matryer/is"
)

func TestNewExpireMap(t *testing.T) {
	is := is.New(t)

	m := NewExpireMap[string, int](10 * time.Millisecond)

	m.Store("foo", 1)
	v, ok := m.Load("foo")
	is.True(ok)
	is.Equal(v, 1)

	time.Sleep(12 * time.Millisecond)
	v, ok = m.Load("foo")
	is.True(!ok)
	is.Equal(v, 1)
}

func TestExpireMap_Range(t *testing.T) {
	is := is.New(t)

	m := NewExpireMap[int, int](100 * time.Millisecond)
	for i := 0; i < 10; i++ {
		m.Store(i, i)
	}

	time.Sleep(50 * time.Millisecond)

	for i := 10; i < 20; i++ {
		m.Store(i, i)
	}
	is.Equal(len(m.m.elements), 20)

	var ints []int
	m.Range(func(k int, v int) bool {
		ints = append(ints, v)
		return true
	})
	is.Equal(len(ints), 20)

	time.Sleep(50 * time.Millisecond)

	is.Equal(len(m.m.elements), 20)

	ints = ints[:0]
	m.Range(func(k int, v int) bool {
		ints = append(ints, v)
		return true
	})
	is.Equal(len(ints), 10)
	is.Equal(len(m.m.elements), 10)
}
