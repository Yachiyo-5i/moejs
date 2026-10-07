package engine

import (
	"errors"
	"fmt"
	"math"
)

// ErrAllocLimit is the sentinel for an allocation budget overrun. The
// *InterruptedError a call returns unwraps to an *AllocLimitError, which
// matches this sentinel, so errors.Is(err, ErrAllocLimit) reports it.
var ErrAllocLimit = errors.New("moejs: allocation budget exceeded")

// ErrResultTooLarge is returned by ToGo, Unmarshal, ToGoInto and AppendJSON
// when the estimated result exceeds MaxResultBytes. The runtime is not
// interrupted.
var ErrResultTooLarge = errors.New("moejs: result too large")

// AllocLimitError is the Value of the *InterruptedError returned when a
// call exceeds its allocation budget. Used is what was charged before the
// failing step, Requested is what that step asked for, and
// Used+Requested > Limit.
type AllocLimitError struct {
	Limit     int64
	Used      int64
	Requested int64
}

// Error reports the overrun. The text starts with ErrAllocLimit's text.
func (e *AllocLimitError) Error() string {
	return fmt.Sprintf("moejs: allocation budget exceeded (used %d + %d > limit %d)", e.Used, e.Requested, e.Limit)
}

// Is reports whether target is ErrAllocLimit.
func (e *AllocLimitError) Is(target error) bool { return target == ErrAllocLimit }

// Estimated bytes of one allocation. Overestimates are acceptable;
// underestimates are not. Tuned against TestAllocBudgetCalibration.
const (
	allocObjectBase int64 = 128 // ordinary object header
	allocValue      int64 = 16  // one Value slot or array element
	allocStringHdr  int64 = 32  // string header besides its bytes
	allocRopeNode   int64 = 48
	allocBufferBase int64 = 64
	allocMapEntry   int64 = 128 // one entry plus the table's discarded rebuilds
	allocFuncObject int64 = 256 // funcObject is the 240-byte class
	allocEnvBase    int64 = 64
	allocFrameBase  int64 = 256
	allocBigIntBase int64 = 32
	allocBigIntWord int64 = 8
	allocExecResult int64 = 160 // regexp exec result array
)

// charge accounts n estimated bytes against the current call's budget.
// allocMax <= 0 means no budget: the check returns before any other work.
// A negative value is used only while a realm is bootstrapping (see
// newObject); SetAllocBudget clamps user values to >= 0.
// The failing step is charged and then refused, and the caller must not
// allocate after a non-nil error.
func (r *Realm) charge(n int64) error {
	if r.allocMax <= 0 || n <= 0 {
		return nil
	}
	return r.chargeSlow(n)
}

// chargeNote is charge for an allocation whose size is bounded by a
// constant. The interrupt is published when the budget is exceeded; the
// next loop back-edge or interruptEvery stops the call. The allocation
// itself still happens, so this must not be used for an unbounded make.
func (r *Realm) chargeNote(n int64) {
	if r.allocMax <= 0 || n <= 0 {
		return
	}
	_ = r.chargeSlow(n)
}

// chargeSlow records the first overrun of the outermost entry in
// realmLazy.allocOverrun and interrupts. The overrun stays until the
// outermost return: later charges fail without charging, and they interrupt
// again when a host ClearInterrupt ran meanwhile. A pending host interrupt
// fails the charge with its own payload.
//
//go:noinline
func (r *Realm) chargeSlow(n int64) error {
	if l := r.lazy; l != nil && l.allocOverrun != nil {
		if r.interruptFlag.Load() == 0 {
			r.Interrupt(l.allocOverrun)
		}
		return r.CheckInterrupt()
	}
	if r.interruptFlag.Load() != 0 {
		return r.CheckInterrupt()
	}
	used := r.allocUsed
	sum := used + n
	if sum < used {
		sum = math.MaxInt64
		n = sum - used
	}
	r.allocUsed = sum
	if sum <= r.allocMax {
		return nil
	}
	lim := &AllocLimitError{Limit: r.allocMax, Used: used, Requested: n}
	r.lazyState().allocOverrun = lim
	r.Interrupt(lim)
	return r.CheckInterrupt()
}

// endOutermost ends the outermost call into the realm: it runs the jobs the
// call queued, then reports a budget overrun of the call (allocMax > 0 only)
// in place of the call's own result, a JavaScript throw included, and
// clears the interrupt the overrun published so the next call is not blamed
// for it. The realm's objects may be half-updated after an overrun; the host
// discards the runtime.
//
//go:noinline
func (r *Realm) endOutermost(err error) error {
	if r.jobsPending {
		err = r.endJob(err)
	}
	if r.allocMax <= 0 {
		return err
	}
	return r.takeAllocOverrun(err)
}

// FinishOutermost is the end of an outermost host entry that neither holds
// jobs nor calls a function (Runtime.FromGo, ParseJSON, and the fast paths of
// Unmarshal and ToGoInto). Inside a call it returns err unchanged.
func (r *Realm) FinishOutermost(err error) error {
	if r.callDepth != 0 || r.allocMax <= 0 {
		return err
	}
	return r.takeAllocOverrun(err)
}

func (r *Realm) takeAllocOverrun(err error) error {
	l := r.lazy
	if l == nil || l.allocOverrun == nil {
		return err
	}
	lim := l.allocOverrun
	l.allocOverrun = nil
	r.ClearInterrupt()
	return &InterruptedError{Value: lim}
}

// SetAllocBudget installs n as the allocation budget and zeroes the counter.
// A negative n means no budget. Runtime.Call, Load and RunScript do this at
// entry; a host that drives the realm directly can too.
func (r *Realm) SetAllocBudget(n int64) {
	if n < 0 {
		n = 0
	}
	r.allocMax = n
	r.allocUsed = 0
	if r.lazy != nil {
		r.lazy.allocOverrun = nil
	}
}

// AllocatedBytes is the estimated bytes charged to the current or most
// recent outermost entry.
func (r *Realm) AllocatedBytes() int64 { return r.allocUsed }

// nextSliceCap estimates the capacity growslice would give a slice that
// must hold need elements, matching its doubling below 256 and its slower
// growth above so the charge tracks TotalAlloc.
func nextSliceCap(old, need int) int {
	if need < 0 {
		return 0
	}
	if old <= 0 {
		if need < 1 {
			return 1
		}
		return need
	}
	n := old
	if old < 256 {
		n = old * 2
	}
	for n < need {
		var add int
		if n < 256 {
			add = n
		} else {
			add = (n + 3*256) / 4
		}
		if add < 1 || n > math.MaxInt-add {
			return need
		}
		n += add
	}
	return n
}

// chargeSliceGrow charges the new backing array a slice growth to need
// elements would allocate. It returns the budget error when that array
// would exceed the budget; the caller then skips the growth.
func (r *Realm) chargeSliceGrow(oldCap, need int) error {
	if r.allocMax <= 0 || need <= oldCap {
		return nil
	}
	return r.charge(int64(nextSliceCap(oldCap, need)) * allocValue)
}

// allocValues charges element storage for n values and allocates it only
// when the budget allows. NewArrayFromSlice charges the object header.
func (r *Realm) allocValues(n int) ([]Value, error) {
	if n < 0 {
		n = 0
	}
	if err := r.chargeSliceGrow(0, n); err != nil {
		return nil, err
	}
	if n == 0 {
		return []Value{}, nil
	}
	return make([]Value, n), nil
}

// allocValuesCap is allocValues for a 0-length slice with room for c values.
func (r *Realm) allocValuesCap(c int) ([]Value, error) {
	if c < 0 {
		c = 0
	}
	if err := r.chargeSliceGrow(0, c); err != nil {
		return nil, err
	}
	if c == 0 {
		return nil, nil
	}
	return make([]Value, 0, c), nil
}

// appendCharged appends v, charging a growth of the backing array first.
// A budget error leaves s unchanged.
func (r *Realm) appendCharged(s []Value, v Value) ([]Value, error) {
	if len(s) == cap(s) && r.allocMax > 0 {
		if err := r.chargeSliceGrow(cap(s), len(s)+1); err != nil {
			return s, err
		}
	}
	return append(s, v), nil
}

// growElements is growWithHoles that charges the new backing array first.
func (r *Realm) growElements(elements []Value, n int) ([]Value, error) {
	if n < 0 {
		n = 0
	}
	if cap(elements) >= n {
		return growWithHoles(elements, n), nil
	}
	if err := r.chargeSliceGrow(cap(elements), n); err != nil {
		return elements, err
	}
	return growWithHoles(elements, n), nil
}

// chargeResult accounts n estimated bytes of a host export (ToGo, Unmarshal,
// ToGoInto, AppendJSON). resultMax == 0 means no bound. Exceeding it is
// ErrResultTooLarge and does not interrupt the runtime.
func (r *Realm) chargeResult(n int64) error {
	if r.resultMax == 0 || n <= 0 {
		return nil
	}
	r.resultUsed += n
	if r.resultUsed < 0 || r.resultUsed > r.resultMax {
		if r.resultUsed < 0 {
			r.resultUsed = math.MaxInt64
		}
		return ErrResultTooLarge
	}
	return nil
}

// noteResult records an export size tracked outside chargeResult (the JSON
// walk's own counter) and reports whether it exceeds the result bound.
func (r *Realm) noteResult(n int64) error {
	if r.resultMax == 0 || n <= r.resultMax {
		if n > r.resultUsed {
			r.resultUsed = n
		}
		return nil
	}
	r.resultUsed = n
	return ErrResultTooLarge
}

// resetResult clears the export counter at the start of one ToGo, Unmarshal,
// ToGoInto or AppendJSON.
func (r *Realm) resetResult() { r.resultUsed = 0 }

// ResultTooLarge reports whether the last host export stopped because the
// estimated result exceeded the result limit. ToGoInto returns only a bool;
// callers that would otherwise build the value another way consult this.
func (r *Realm) ResultTooLarge() bool {
	return r.resultMax > 0 && r.resultUsed > r.resultMax
}
