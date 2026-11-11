package block

// SearchMarker caches "item I starts at user-facing index N" so a
// subsequent position lookup near N can walk the linked list from
// I instead of from branch.Start. Without markers, every Array /
// Text Insert at index P walks O(P) items; B4 (259k sequential
// edits on a 100k-char doc) is the canonical pathological workload
// — see BENCHMARKS.md and docs/yrs-port-notes/types-array.md
// finding 1.
//
// Mirrors yrs's SearchMarker (yrs/src/branch.rs around the
// `search_marker` field) with the same ~80-marker LRU cap.
//
// Concurrency: MarkerList is mutated only under the doc write lock
// (held by TransactionMut). Read-only access from a read txn is
// safe as long as no concurrent writer holds the same Branch.
type SearchMarker struct {
	// Item is the linked-list node this marker points at. May be
	// a tombstone (in which case Index is the position immediately
	// after the tombstone run that includes Item — slightly stale
	// markers still resolve correctly because the search walks
	// forward from the marker and counts live items).
	Item *Item

	// Index is the user-facing index where Item starts in document
	// order (countable, non-deleted items only).
	Index uint64

	// Timestamp is a monotonic clock value bumped on Touch / Add;
	// used to evict the oldest marker when len == Cap.
	Timestamp uint64
}

// markerCap is the LRU cap matching yrs INTERNALS' ~80-entry
// budget. Picked empirically by yrs as the sweet spot between
// memory overhead and hit rate on real editing traces.
const markerCap = 80

// MarkerList is a fixed-cap pool of SearchMarkers attached to a
// Branch. Construct lazily — branches that never see large
// findPosition calls pay nothing.
type MarkerList struct {
	markers []SearchMarker
	clock   uint64
}

// NewMarkerList returns an empty MarkerList sized to markerCap.
func NewMarkerList() *MarkerList {
	return &MarkerList{markers: make([]SearchMarker, 0, markerCap)}
}

// Nearest returns a pointer to the marker whose Index is closest
// to target, or nil if the list is empty. The caller may walk
// forward (target > marker.Index) or backward (target <
// marker.Index) from marker.Item; both directions are supported
// by the linked-list structure (Item.Right / Item.Left).
//
// Returned pointer aliases internal state; callers must NOT retain
// it past the next list mutation (Add / Touch / ShiftAfter /
// ShrinkAfter).
func (m *MarkerList) Nearest(target uint64) *SearchMarker {
	if m == nil || len(m.markers) == 0 {
		return nil
	}
	bestIdx := 0
	bestDist := absDiff(m.markers[0].Index, target)
	for i := 1; i < len(m.markers); i++ {
		d := absDiff(m.markers[i].Index, target)
		if d < bestDist {
			bestDist = d
			bestIdx = i
		}
	}
	return &m.markers[bestIdx]
}

// Around returns the marker with the greatest Index at or before target
// and the one with the smallest Index after it; either may be nil. A walk
// to target can start at the first and go right, or at the second and go
// left. The pointers are valid until the next list mutation, as for
// Nearest.
func (m *MarkerList) Around(target uint64) (before, after *SearchMarker) {
	if m == nil {
		return nil, nil
	}
	for i := range m.markers {
		mk := &m.markers[i]
		if mk.Index <= target {
			if before == nil || mk.Index > before.Index {
				before = mk
			}
		} else if after == nil || mk.Index < after.Index {
			after = mk
		}
	}
	return before, after
}

// Repoint moves the marker on from, an item that was merged into its left
// neighbour to, onto to. shift is what to counted for before the merge:
// its length if it is live and countable, else zero. The merged item
// starts where to started, that much earlier. Every other marker stays as
// it is: a merge changes no other item's position. Mirrors what yjs
// Item.mergeWith does with its search markers.
func (m *MarkerList) Repoint(from, to *Item, shift uint64) {
	if m == nil {
		return
	}
	at, has := -1, false
	for i := range m.markers {
		switch m.markers[i].Item {
		case from:
			at = i
		case to:
			has = true
		}
	}
	if at < 0 {
		return
	}
	if has || m.markers[at].Index < shift {
		// to has its own marker already (or the marker was off): drop this one.
		m.markers = append(m.markers[:at], m.markers[at+1:]...)
		return
	}
	m.markers[at].Item = to
	m.markers[at].Index -= shift
}

// Touch bumps the marker's timestamp so the next eviction skips
// it. Call after a successful Nearest hit + walk.
func (m *MarkerList) Touch(mk *SearchMarker) {
	if m == nil || mk == nil {
		return
	}
	m.clock++
	mk.Timestamp = m.clock
}

// Add records a fresh marker. If at cap, evicts the marker with
// the smallest timestamp (LRU). If the new marker's item already
// has a marker, the existing one is updated in-place rather than
// adding a duplicate — keeps the cache useful when the same item
// is touched repeatedly.
func (m *MarkerList) Add(item *Item, index uint64) {
	if m == nil || item == nil {
		return
	}
	m.clock++
	for i := range m.markers {
		if m.markers[i].Item == item {
			m.markers[i].Index = index
			m.markers[i].Timestamp = m.clock
			return
		}
	}
	if len(m.markers) < markerCap {
		m.markers = append(m.markers, SearchMarker{Item: item, Index: index, Timestamp: m.clock})
		return
	}
	oldestIdx := 0
	for i := 1; i < len(m.markers); i++ {
		if m.markers[i].Timestamp < m.markers[oldestIdx].Timestamp {
			oldestIdx = i
		}
	}
	m.markers[oldestIdx] = SearchMarker{Item: item, Index: index, Timestamp: m.clock}
}

// ShiftAfter is called after an Insert at insertIdx with length
// insertLen. Markers whose Index is strictly greater than
// insertIdx shift right by insertLen (their items now appear
// `insertLen` positions later). Markers at exactly insertIdx and
// before are unaffected — the newly-inserted item lives at
// insertIdx but did not displace anything before it.
func (m *MarkerList) ShiftAfter(insertIdx, insertLen uint64) {
	if m == nil || insertLen == 0 {
		return
	}
	for i := range m.markers {
		if m.markers[i].Index > insertIdx {
			m.markers[i].Index += insertLen
		}
	}
}

// ShrinkAfter is called after a Delete at [delIdx, delIdx+delLen).
// Markers whose Index falls inside the deleted range are dropped
// (their items may now point at tombstones for which we can't
// reliably regenerate a position without re-walking). Markers
// strictly after the range shift left by delLen. Markers strictly
// before are unaffected.
func (m *MarkerList) ShrinkAfter(delIdx, delLen uint64) {
	if m == nil || delLen == 0 || len(m.markers) == 0 {
		return
	}
	delEnd := delIdx + delLen
	out := m.markers[:0]
	for _, mk := range m.markers {
		switch {
		case mk.Index < delIdx:
			out = append(out, mk)
		case mk.Index >= delEnd:
			mk.Index -= delLen
			out = append(out, mk)
		default:
			// inside [delIdx, delEnd) — drop
		}
	}
	m.markers = out
}

// Invalidate clears every marker. Use when the underlying linked
// list undergoes structural changes the shift helpers can't
// describe (deep nested operations, bulk apply, etc.). The next
// findPosition call will walk from branch.Start as if no cache
// existed.
func (m *MarkerList) Invalidate() {
	if m == nil {
		return
	}
	m.markers = m.markers[:0]
}

// Len returns the current marker count — useful only for tests
// and debug observability.
func (m *MarkerList) Len() int {
	if m == nil {
		return 0
	}
	return len(m.markers)
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}
