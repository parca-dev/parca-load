package sync

import (
	"log"
	"time"
)

type expireValue[v any] struct {
	expires time.Time
	value   v
}

type ExpireMap[k comparable, v any] struct {
	m   *Map[k, expireValue[v]]
	def time.Duration
}

func NewExpireMap[k comparable, v any](def time.Duration) *ExpireMap[k, v] {
	return &ExpireMap[k, v]{
		def: def,
		m:   NewMap[k, expireValue[v]](),
	}
}

func (em *ExpireMap[k, v]) Store(key k, value v) {
	em.m.Store(key, expireValue[v]{
		expires: time.Now().Add(em.def),
		value:   value,
	})
}

func (em *ExpireMap[k, v]) Load(key k) (v, bool) {
	ev, ok := em.m.Load(key)
	if !ok {
		return ev.value, false
	}
	if time.Now().After(ev.expires) {
		log.Printf("%v already expired with %v", key, ev.value)
		// TODO: Delete?
		return ev.value, false
	}
	return ev.value, true
}

func (em *ExpireMap[k, v]) Range(f func(key k, value v) bool) {
	em.m.mtx.Lock()
	defer em.m.mtx.Unlock()

	for k, ev := range em.m.elements {
		if time.Now().After(ev.expires) {
			delete(em.m.elements, k)
			continue
		}
		if !f(k, ev.value) {
			break
		}
	}
}
