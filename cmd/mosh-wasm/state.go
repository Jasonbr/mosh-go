//go:build js && wasm

package main

import (
	"sync"

	mosh "github.com/unixshells/mosh-go"
	vt "github.com/unixshells/vt-go"
)

// stateTracker implements Dart-style received state management.
// Each incoming diff is applied to a copy of the base state (looked up
// by oldNum), producing a new state stored by newNum. The display
// always shows the latest received state, diffed against what's
// currently on screen. This eliminates garbled output from missed
// diffs and overlapping state transitions.
type stateTracker struct {
	mu sync.Mutex

	shadow      *vt.Emulator
	shadowState uint64
	cols, rows  int

	// Received states: stateNum → framebuffer snapshot.
	states       map[uint64]*mosh.Framebuffer
	latestState  uint64
	displayedFB  *mosh.Framebuffer

	// Output buffer: pre-diffed ANSI ready for the terminal.
	output []byte
}

func newStateTracker(cols, rows int) *stateTracker {
	return &stateTracker{
		shadow: vt.NewEmulator(cols, rows),
		cols:   cols,
		rows:   rows,
		states: make(map[uint64]*mosh.Framebuffer),
	}
}

// applyDiff processes an incoming server diff using state tracking.
func (st *stateTracker) applyDiff(diff []byte, oldNum, newNum uint64) {
	st.mu.Lock()
	defer st.mu.Unlock()

	// Already have this state — skip.
	if _, ok := st.states[newNum]; ok {
		return
	}

	// Snapshot current shadow as oldNum if we don't have it.
	if oldNum == st.shadowState {
		if _, ok := st.states[oldNum]; !ok {
			st.states[oldNum] = mosh.SnapshotEmulator(st.shadow, true)
		}
	}

	// Need the base state to apply the diff.
	base, ok := st.states[oldNum]
	if !ok {
		// After a resize we have no states. Start fresh with a
		// clean emulator so the first post-resize diff works.
		st.shadow = vt.NewEmulator(st.cols, st.rows)
		st.shadowState = oldNum
		base = mosh.SnapshotEmulator(st.shadow, true)
		st.states[oldNum] = base
	} else if st.shadowState != oldNum {
		st.shadow = vt.NewEmulator(st.cols, st.rows)
		// Write the base framebuffer as ANSI to restore state.
		if base != nil {
			st.shadow.Write(base.Diff(nil))
		}
	}

	// Apply diff: feed hoststrings to shadow.
	instrs, err := mosh.UnmarshalHostMessage(diff)
	if err != nil {
		return
	}
	for _, hi := range instrs {
		if len(hi.Hoststring) > 0 {
			st.shadow.Write(hi.Hoststring)
		}
	}
	st.shadowState = newNum

	// Snapshot result.
	snap := mosh.SnapshotEmulator(st.shadow, true)
	st.states[newNum] = snap
	if newNum > st.latestState {
		st.latestState = newNum
	}

	// Bound states list.
	if len(st.states) > 128 {
		st.pruneStates()
	}

	// Display diff: compute ANSI difference between displayed and latest.
	latest := st.states[st.latestState]
	if latest != nil {
		ansi := latest.Diff(st.displayedFB)
		if len(ansi) > 0 {
			st.output = append(st.output, ansi...)
		}
		st.displayedFB = latest
	}
}

// poll returns accumulated ANSI output, or nil.
func (st *stateTracker) poll() []byte {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.output) == 0 {
		return nil
	}
	out := st.output
	st.output = nil
	return out
}

// resize resets the state tracker for new dimensions.
func (st *stateTracker) resize(cols, rows int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.cols = cols
	st.rows = rows
	st.shadow = vt.NewEmulator(cols, rows)
	st.shadowState = 0
	st.states = make(map[uint64]*mosh.Framebuffer)
	st.displayedFB = nil
}

func (st *stateTracker) pruneStates() {
	if len(st.states) <= 64 {
		return
	}
	// Keep the 64 most recent states.
	var nums []uint64
	for n := range st.states {
		nums = append(nums, n)
	}
	// Simple sort for small lists.
	for i := range nums {
		for j := i + 1; j < len(nums); j++ {
			if nums[j] < nums[i] {
				nums[i], nums[j] = nums[j], nums[i]
			}
		}
	}
	for _, n := range nums[:len(nums)-64] {
		delete(st.states, n)
	}
}
