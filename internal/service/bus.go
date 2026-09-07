package service

import (
	"sync"
	"time"
)

type Event struct {
	Kind     string    `json:"kind"`
	EntityID string    `json:"entityId,omitempty"`
	At       time.Time `json:"at"`
}

type Bus struct {
	mu   sync.Mutex
	next int
	subs map[int]chan Event
}

func NewBus() *Bus { return &Bus{subs: map[int]chan Event{}} }

func (b *Bus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan Event, 16)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if current, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(current)
		}
	}
}

func (b *Bus) Publish(kind, entityID string) {
	event := Event{Kind: kind, EntityID: entityID, At: time.Now().UTC()}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- event:
		default:
		}
	}
}
