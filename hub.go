package main

import "sync"

const ringSize = 500 // max spots kept in the in-memory history ring (web UI initial load)

// Hub fans out incoming Spots to all registered subscribers.
// A single goroutine owns the subscriber map — no mutex needed for the map itself.
// The ring buffer is protected by a RWMutex for concurrent REST reads.
// An optional SpotStore receives every spot for persistent storage.
type Hub struct {
	in    chan Spot
	sub   chan subReq
	unsub chan chan Spot

	mu   sync.RWMutex
	ring []Spot // circular history, newest-first

	store *SpotStore // optional; nil if persistence is disabled
}

type subReq struct {
	ch chan Spot
}

func NewHub(store *SpotStore) *Hub {
	return &Hub{
		in:    make(chan Spot, 256),
		sub:   make(chan subReq, 16),
		unsub: make(chan chan Spot, 16),
		ring:  make([]Spot, 0, ringSize),
		store: store,
	}
}

// Publish sends a spot into the hub.
func (h *Hub) Publish(s Spot) {
	select {
	case h.in <- s:
	default:
		// drop if hub is overwhelmed
	}
}

// Subscribe returns a channel that will receive all future spots.
// The caller must call Unsubscribe when done.
func (h *Hub) Subscribe() chan Spot {
	ch := make(chan Spot, 64)
	h.sub <- subReq{ch: ch}
	return ch
}

// Unsubscribe removes the channel from the hub.
func (h *Hub) Unsubscribe(ch chan Spot) {
	h.unsub <- ch
}

// History returns a copy of the ring buffer (newest first), optionally
// filtered to a single stream type. Pass "" for all streams.
func (h *Hub) History(stream StreamType) []Spot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Spot, 0, len(h.ring))
	for _, s := range h.ring {
		if stream == "" || s.Stream == stream {
			out = append(out, s)
		}
	}
	return out
}

// Run is the hub's main loop. Call in a goroutine.
func (h *Hub) Run() {
	subs := make(map[chan Spot]struct{})

	for {
		select {
		case s := <-h.in:
			// Append to ring buffer
			h.mu.Lock()
			h.ring = append([]Spot{s}, h.ring...)
			if len(h.ring) > ringSize {
				h.ring = h.ring[:ringSize]
			}
			h.mu.Unlock()

			// Persist to store if configured
			if h.store != nil {
				h.store.Publish(s)
			}

			// Fan out to all subscribers
			for ch := range subs {
				select {
				case ch <- s:
				default:
					// slow subscriber — drop rather than block
				}
			}

		case req := <-h.sub:
			subs[req.ch] = struct{}{}

		case ch := <-h.unsub:
			delete(subs, ch)
			close(ch)
		}
	}
}
