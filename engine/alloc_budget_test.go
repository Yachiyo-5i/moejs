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
