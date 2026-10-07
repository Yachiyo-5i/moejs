package engine

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAllocBudgetCharge(t *testing.T) {
	r := NewRealm()
	require.NoError(t, r.charge(1<<20))
	require.Zero(t, r.AllocatedBytes())

	r.SetAllocBudget(1000)
	require.NoError(t, r.charge(400))
	require.Equal(t, int64(400), r.AllocatedBytes())
	err := r.charge(700)
	require.ErrorIs(t, err, ErrAllocLimit)
	var lim *AllocLimitError
	require.ErrorAs(t, err, &lim)
	require.Equal(t, int64(1000), lim.Limit)
	require.Equal(t, int64(400), lim.Used)
	require.Equal(t, int64(700), lim.Requested)
	require.Greater(t, lim.Used+lim.Requested, lim.Limit)

	r.ClearInterrupt()
	r.SetAllocBudget(1000)
	require.Zero(t, r.AllocatedBytes())
	require.NoError(t, r.charge(1000))
}

func TestAllocBudgetInterruptKeepsFirst(t *testing.T) {
	r := NewRealm()
	r.SetAllocBudget(1000)
	r.Interrupt("timeout")
	err := r.charge(5000)
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	require.Equal(t, "timeout", ie.Value)
	require.NotErrorIs(t, err, ErrAllocLimit)

	r.ClearInterrupt()
	r.SetAllocBudget(1000)
	err = r.charge(5000)
	require.ErrorIs(t, err, ErrAllocLimit)
	r.Interrupt("timeout")
	err = r.CheckInterrupt()
	require.ErrorAs(t, err, &ie)
	require.Equal(t, "timeout", ie.Value)
	var lim *AllocLimitError
	err = r.FinishOutermost(nil)
	require.ErrorIs(t, err, ErrAllocLimit)
	require.ErrorAs(t, err, &lim)
	require.Equal(t, int64(5000), lim.Requested)

	r.SetAllocBudget(1000)
	err = r.charge(5000)
	require.ErrorIs(t, err, ErrAllocLimit)
	_ = r.charge(9000)
	err = r.FinishOutermost(nil)
	require.ErrorIs(t, err, ErrAllocLimit)
	require.ErrorAs(t, err, &ie)
	lim = ie.Value.(*AllocLimitError)
	require.Equal(t, int64(5000), lim.Requested)
}

func TestInterruptLastPayloadWins(t *testing.T) {
	r := NewRealm()
	r.Interrupt("first")
	r.Interrupt("second")
	var ie *InterruptedError
	require.ErrorAs(t, r.CheckInterrupt(), &ie)
	require.Equal(t, "second", ie.Value)
}

func TestResultTooLargeCharge(t *testing.T) {
	r := NewRealm()
	require.NoError(t, r.chargeResult(1<<20))
	r.resultMax = 100
	r.resetResult()
	require.NoError(t, r.chargeResult(40))
	require.ErrorIs(t, r.chargeResult(80), ErrResultTooLarge)
	require.False(t, r.Interrupted())
	require.True(t, r.ResultTooLarge())
}
