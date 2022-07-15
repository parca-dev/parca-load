package sync

import (
	"sync"
)

type Map[k comparable, v any] struct {
	mtx      *sync.RWMutex
	elements map[k]v
}

func NewMap[k comparable, v any]() *Map[k, v] {
	return &Map[k, v]{
		mtx:      &sync.RWMutex{},
		elements: make(map[k]v),
	}
}

func (sm *Map[k, v]) Store(key k, value v) {
	sm.mtx.Lock()
	defer sm.mtx.Unlock()

	sm.elements[key] = value
}

func (sm *Map[k, v]) Load(key k) (v, bool) {
	sm.mtx.RLock()
	defer sm.mtx.RUnlock()

	value, found := sm.elements[key]
	return value, found
}

func (sm *Map[k, v]) Range(f func(key k, value v) bool) {
	sm.mtx.RLock()
	defer sm.mtx.RUnlock()

	for k, v := range sm.elements {
		if !f(k, v) {
			break
		}
	}
}
