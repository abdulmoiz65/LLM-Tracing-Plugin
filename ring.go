package main

// spanRing is a bounded FIFO of finished spans. When full, pushing drops the
// oldest span. The backing array grows on demand up to the limit, so an idle
// plugin does not reserve memory for max_queue_spans spans.
type spanRing struct {
	buf   []otlpSpan
	head  int
	n     int
	limit int
}

func (r *spanRing) len() int { return r.n }

// push appends s and reports whether an older span was dropped to make room.
func (r *spanRing) push(s otlpSpan) bool {
	if r.limit <= 0 {
		return true
	}
	if r.n == len(r.buf) && len(r.buf) < r.limit {
		r.grow(min(max(2*len(r.buf), 64), r.limit))
	}
	if r.n == len(r.buf) {
		r.buf[r.head] = s
		r.head = (r.head + 1) % len(r.buf)
		return true
	}
	r.buf[(r.head+r.n)%len(r.buf)] = s
	r.n++
	return false
}

// pop removes and returns up to k of the oldest spans.
func (r *spanRing) pop(k int) []otlpSpan {
	k = min(k, r.n)
	if k <= 0 {
		return nil
	}
	out := make([]otlpSpan, k)
	for i := range out {
		idx := (r.head + i) % len(r.buf)
		out[i] = r.buf[idx]
		r.buf[idx] = otlpSpan{}
	}
	r.head = (r.head + k) % len(r.buf)
	r.n -= k
	if r.n == 0 {
		r.head = 0
	}
	return out
}

func (r *spanRing) grow(capacity int) {
	next := make([]otlpSpan, capacity)
	for i := 0; i < r.n; i++ {
		next[i] = r.buf[(r.head+i)%len(r.buf)]
	}
	r.buf = next
	r.head = 0
}

// setLimit changes the bound, dropping the oldest spans if needed. It returns
// the number of dropped spans.
func (r *spanRing) setLimit(limit int) int {
	dropped := 0
	if r.n > limit {
		dropped = r.n - max(limit, 0)
		r.pop(dropped)
	}
	r.limit = limit
	if len(r.buf) > limit {
		r.grow(max(limit, r.n))
	}
	return dropped
}

// clear drops everything and releases the backing array.
func (r *spanRing) clear() int {
	n := r.n
	r.buf, r.head, r.n = nil, 0, 0
	return n
}
