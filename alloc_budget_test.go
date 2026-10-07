package moejs_test

import (
	"errors"
	"runtime"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Yachiyo-5i/moejs"
	"github.com/stretchr/testify/require"
)

const allocBudget = 16 << 20

func allocScript(t *testing.T, src string) *moejs.Script {
	t.Helper()
	s, err := moejs.CompileScript("alloc.js", src)
	require.NoError(t, err)
	return s
}

func mustModule(t *testing.T, src string) *moejs.Module {
	t.Helper()
	m, err := moejs.Compile("alloc.js", src)
	require.NoError(t, err)
	return m
}

// heapSpan samples HeapInuse while fn runs. The sampler does not touch the
// runtime. Do not call this from a parallel test.
func heapSpan(fn func()) (before, peak uint64) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	before = ms.HeapInuse
	var maxH atomic.Uint64
	maxH.Store(before)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var m runtime.MemStats
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				runtime.ReadMemStats(&m)
				for {
					old := maxH.Load()
					if m.HeapInuse <= old || maxH.CompareAndSwap(old, m.HeapInuse) {
						break
					}
				}
				return
			case <-tick.C:
				runtime.ReadMemStats(&m)
				for {
					old := maxH.Load()
					if m.HeapInuse <= old || maxH.CompareAndSwap(old, m.HeapInuse) {
						break
					}
				}
			}
		}
	}()
	fn()
	close(stop)
	wg.Wait()
	return before, maxH.Load()
}

func TestAllocBudgetBombs(t *testing.T) {
	bombs := []struct {
		name string
		src  string
		mod  bool
	}{
		{"string doubling", `let s = "x"; for (;;) s += s;`, false},
		{"repeat", `"x".repeat(1 << 28);`, false},
		{"big array", `new Array(1e8).fill(0);`, false},
		{"push loop", `const a = []; for (;;) a.push({});`, false},
		{"property explosion", `const o = {}; for (let i = 0; ; i++) o["k" + i] = i;`, false},
		{"array buffer", `new ArrayBuffer(1 << 30);`, false},
		{"map", `const m = new Map(); for (let i = 0; ; i++) m.set(i, i);`, false},
		{"closures", `const fs = []; for (;;) fs.push(() => fs);`, false},
		{"json", `JSON.stringify(Array(1e7).fill("abcdefgh"));`, false},
		{"regexp", `const s = "a".repeat(1 << 20); const out = []; for (const m of s.matchAll(/a/g)) out.push(m);`, false},
		{"microtask", `globalThis.fin = false; queueMicrotask(() => { try { const a = []; for (;;) a.push({}); } catch (e) { globalThis.fin = "caught"; } finally { globalThis.fin = "fin"; } });`, false},
		{"module", `export let fin = false; try { const a = []; for (;;) a.push({}); } catch (e) { fin = "caught"; } finally { fin = "fin"; }`, true},
	}
	slack := uint64(allocBudget)*4 + 32<<20
	for _, b := range bombs {
		t.Run(b.name, func(t *testing.T) {
			rt := moejs.NewRuntime(moejs.Options{MaxAllocBytes: allocBudget})
			var err error
			before, peak := heapSpan(func() {
				if b.mod {
					err = rt.Load(mustModule(t, b.src))
					return
				}
				src := b.src
				if b.name != "microtask" {
					src = "var fin = false;\ntry {\n" + b.src + "\n} catch (e) { fin = \"caught\"; } finally { fin = \"fin\"; }\n"
				}
				_, err = rt.RunScript(allocScript(t, src))
			})
			require.ErrorIs(t, err, moejs.ErrAllocLimit)
			var lim *moejs.AllocLimitError
			require.ErrorAs(t, err, &lim)
			require.Equal(t, int64(allocBudget), lim.Limit)
			require.Greater(t, lim.Used+lim.Requested, lim.Limit)
			require.LessOrEqual(t, peak, before+slack, "heap grew past the budget slack")

			// The overrun belongs to this call. Objects may be half-updated, so
			// a later script is only required not to panic.
			require.False(t, rt.Realm().Interrupted())
			rt.ClearInterrupt()
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("reuse panicked: %v", p)
					}
				}()
				var fin moejs.Value
				if b.mod {
					var ok bool
					fin, ok = rt.Export("fin")
					require.True(t, ok)
				} else {
					var ferr error
					fin, ferr = rt.RunScript(allocScript(t, "fin"))
					require.NoError(t, ferr)
				}
				got, gerr := rt.ToGo(fin)
				require.NoError(t, gerr)
				require.Equal(t, false, got)
				_, _ = rt.RunScript(allocScript(t, "1 + 1"))
			}()
		})
	}
}

func TestAllocBudgetResetsEachCall(t *testing.T) {
	const budget = 16 << 20
	rt := moejs.NewRuntime(moejs.Options{MaxAllocBytes: budget})
	s := allocScript(t, `{ const a = []; for (let i = 0; i < 100000; i++) a.push(i); }`)
	_, err := rt.RunScript(s)
	require.NoError(t, err)
	first := rt.AllocatedBytes()
	_, err = rt.RunScript(s)
	require.NoError(t, err)
	second := rt.AllocatedBytes()
	t.Logf("allocated first=%d second=%d budget=%d", first, second, budget)
	require.Greater(t, first, int64(budget)/2)
	require.Less(t, first, int64(budget))
	require.Greater(t, second, int64(budget)/2)
	require.Less(t, second, int64(budget))
	delta := first - second
	if delta < 0 {
		delta = -delta
	}
	require.Less(t, delta*5, first) // within 20%
}

func TestAllocBudgetSetMaxAppliesNextCall(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{MaxAllocBytes: 32 << 20})
	require.NoError(t, rt.SetGlobal("utils", map[string]any{
		"shrink": moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
			rt.SetMaxAllocBytes(1024)
			return moejs.Undefined(), nil
		}),
	}))
	mod := mustModule(t, `
export function go() {
  const a = [];
  for (let i = 0; i < 1000; i++) a.push(i);
  utils.shrink();
  for (let i = 0; i < 100000; i++) a.push(i);
  return a.length;
}
`)
	require.NoError(t, rt.Load(mod))
	h := mustHook(t, mod, "go")
	v, err := rt.Call(h)
	require.NoError(t, err)
	n, err := rt.ToGo(v)
	require.NoError(t, err)
	require.Equal(t, int64(101000), n)
	_, err = rt.Call(h)
	require.ErrorIs(t, err, moejs.ErrAllocLimit)
}

func TestAllocBudgetHooks(t *testing.T) {
	mod := mustModule(t, `
export function decodeRequest(ctx) {
  const body = ctx.body || {};
  return { model: String(body.model || ""), prompt: String(body.prompt || "").trim(), n: body.n | 0, stream: !!body.stream };
}
export function mapItems(items) {
  return items.map(x => ({ id: x.id, name: String(x.name) }));
}
export function paragraph(parts) {
  let s = "";
  for (const p of parts) s += p;
  return s;
}
export function parseBody(text) {
  return JSON.parse(text);
}
`)
	rt := moejs.NewRuntime(moejs.Options{MaxAllocBytes: 1 << 20})
	require.NoError(t, rt.Load(mod))
	items := make([]any, 100)
	for i := range items {
		items[i] = map[string]any{"id": i, "name": "item"}
	}
	parts := make([]any, 40)
	for i := range parts {
		parts[i] = "The quick brown fox jumps over the lazy dog. "
	}
	type call struct {
		name string
		hook moejs.Hook
		arg  any
	}
	calls := []call{
		{"decodeRequest", mustHook(t, mod, "decodeRequest"), map[string]any{"body": map[string]any{"model": "m", "prompt": " hello ", "n": 2, "stream": true}}},
		{"mapItems", mustHook(t, mod, "mapItems"), items},
		{"paragraph", mustHook(t, mod, "paragraph"), parts},
		{"parseBody", mustHook(t, mod, "parseBody"), `{"model":"m","prompt":"hello","n":2,"items":[1,2,3,4,5]}`},
	}
	var used []int64
	for _, c := range calls {
		arg, err := rt.FromGo(c.arg)
		require.NoError(t, err)
		_, err = rt.Call(c.hook, arg)
		require.NoError(t, err)
		n := rt.AllocatedBytes()
		used = append(used, n)
		t.Logf("hook %s allocated %d", c.name, n)
	}
	slices.Sort(used)
	p95 := used[int(float64(len(used)-1)*0.95)]
	t.Logf("representative hooks AllocatedBytes max=%d p95=%d", used[len(used)-1], p95)
}

func TestAllocBudgetCalibration(t *testing.T) {
	prev := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(prev) })
	rt := moejs.NewRuntime(moejs.Options{MaxAllocBytes: 64 << 20})
	cases := []struct {
		name string
		src  string
	}{
		{"dense array", `{ const a = []; for (let i = 0; i < 100000; i++) a.push(i); }`},
		{"objects", `{ const a = []; for (let i = 0; i < 20000; i++) a.push({a: i, b: i}); }`},
		{"string", `"x".repeat(1 << 20);`},
		{"array buffer", `new ArrayBuffer(1 << 20);`},
		{"map", `{ const m = new Map(); for (let i = 0; i < 20000; i++) m.set(i, i); }`},
	}
	for _, c := range cases {
		s := allocScript(t, c.src)
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := rt.RunScript(s)
		require.NoError(t, err)
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		actual := after.TotalAlloc - before.TotalAlloc
		charged := rt.AllocatedBytes()
		require.NotZero(t, actual)
		ratio := float64(charged) / float64(actual)
		t.Logf("%s charged=%d actual=%d ratio=%.2f", c.name, charged, actual, ratio)
		require.GreaterOrEqual(t, ratio, 0.5, c.name)
		require.LessOrEqual(t, ratio, 4.0, c.name)
	}
}

func TestResultTooLarge(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{MaxResultBytes: 64 << 10})
	v, err := rt.RunScript(allocScript(t, `const o = {a: "hello", b: "world", c: "test"}; Array(20000).fill(o)`))
	require.NoError(t, err)

	_, err = rt.ToGo(v)
	require.ErrorIs(t, err, moejs.ErrResultTooLarge)
	var dst []any
	err = rt.Unmarshal(v, &dst)
	require.ErrorIs(t, err, moejs.ErrResultTooLarge)
	require.Nil(t, dst)
	_, err = rt.AppendJSON(nil, v)
	require.ErrorIs(t, err, moejs.ErrResultTooLarge)
	var into []map[string]string
	err = rt.ToGoInto(v, &into)
	require.ErrorIs(t, err, moejs.ErrResultTooLarge)
	require.Empty(t, into)

	sum, err := rt.RunScript(allocScript(t, "1 + 1"))
	require.NoError(t, err)
	n, err := rt.ToGo(sum)
	require.NoError(t, err)
	require.Equal(t, int64(2), n)
}

func TestAllocBudgetBypass(t *testing.T) {
	cases := []string{
		`new Uint8Array(1 << 30)`,
		`new Float64Array(1 << 27)`,
		`const a = new Uint8Array(1 << 22); for (;;) a.map(x => x)`,
		`Array(3e7).toSorted()`,
		`Array(3e7).toReversed()`,
		`Array(3e7).with(0, 1)`,
		`Array(3e7).toSpliced(0, 0)`,
		`Array.from(Array(3e7))`,
		`Array.from("x".repeat(1 << 23))`,
		`Array.from({length: 3e7})`,
		`[...Array(3e7).keys()]`,
		`"x".repeat(1 << 23).split("")`,
		`"x".padStart(1 << 29)`,
		`Array(1 << 22).fill(0).flat()`,
		`Object.keys(Array(1 << 22).fill(0))`,
		`Object.entries(Array(1 << 22).fill(0))`,
		`[..."x".repeat(1 << 23)]`,
	}
	slack := uint64(allocBudget)*4 + 32<<20
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			rt := moejs.NewRuntime(moejs.Options{MaxAllocBytes: allocBudget})
			var err error
			before, peak := heapSpan(func() {
				_, err = rt.RunScript(allocScript(t, src+"; 0"))
			})
			require.ErrorIs(t, err, moejs.ErrAllocLimit)
			require.LessOrEqual(t, peak, before+slack, "heap grew past the budget slack")
		})
	}
}

func TestAllocBudgetDoesNotLeak(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{MaxAllocBytes: allocBudget})
	_, err := rt.RunScript(allocScript(t, `Array.from(Array(3e6)); 0`))
	require.ErrorIs(t, err, moejs.ErrAllocLimit)
	require.False(t, rt.Realm().Interrupted())
	_, err = rt.RunScript(allocScript(t, `1 + 1`))
	require.NoError(t, err)
}

func TestAllocBudgetNestedCallDoesNotReset(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{MaxAllocBytes: allocBudget})
	mod := mustModule(t, `
export function noop() {}
export function bomb() {
  const keep = [];
  for (let i = 0; i < 200; i++) { keep.push("x".repeat(1 << 20)); host(); }
  return keep.length;
}
`)
	require.NoError(t, rt.Load(mod))
	noop := mustHook(t, mod, "noop")
	require.NoError(t, rt.SetGlobal("host", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		_, err := rt.Call(noop)
		return moejs.Undefined(), err
	})))
	_, err := rt.Call(mustHook(t, mod, "bomb"))
	require.ErrorIs(t, err, moejs.ErrAllocLimit)
	require.Greater(t, rt.AllocatedBytes(), int64(allocBudget))
}

// A host interrupt pending before the overrun stops the call with its own
// payload; an overrun that came first is what the call returns.
func TestAllocBudgetInterruptPrecedence(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{MaxAllocBytes: allocBudget})
	rt.Interrupt("timeout")
	_, err := rt.RunScript(allocScript(t, `new Uint8Array(1 << 30)`))
	var ie *moejs.InterruptedError
	require.ErrorAs(t, err, &ie)
	require.Equal(t, "timeout", ie.Value)

	rt = moejs.NewRuntime(moejs.Options{MaxAllocBytes: allocBudget})
	huge := allocScript(t, `new Uint8Array(1 << 28)`)
	require.NoError(t, rt.SetGlobal("late", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
		// Nested, so this keeps the outer budget. The overrun is published
		// here; the host interrupt after it must not replace the payload.
		_, _ = rt.RunScript(huge)
		r.Interrupt("timeout")
		return moejs.Undefined(), r.CheckInterrupt()
	})))
	_, err = rt.RunScript(allocScript(t, `late()`))
	require.ErrorIs(t, err, moejs.ErrAllocLimit)
	require.ErrorAs(t, err, &ie)
	_, ok := ie.Value.(*moejs.AllocLimitError)
	require.True(t, ok)
}

func TestAllocBudgetCallDoesNotLeak(t *testing.T) {
	const budget = 1 << 20
	rt := moejs.NewRuntime(moejs.Options{MaxAllocBytes: budget})
	mod := mustModule(t, `
export function f() { let o; for (let i = 0; i < 12000; i++) o = {}; return 1; }
export function g() { return 2; }
`)
	require.NoError(t, rt.Load(mod))
	f := mustHook(t, mod, "f")
	g := mustHook(t, mod, "g")
	_, err := rt.Call(f)
	require.ErrorIs(t, err, moejs.ErrAllocLimit)
	require.False(t, rt.Realm().Interrupted())
	v, err := rt.Call(g)
	require.NoError(t, err)
	n, err := rt.ToGo(v)
	require.NoError(t, err)
	require.Equal(t, int64(2), n)
}

func TestAllocBudgetExceptionMaskedByOverrun(t *testing.T) {
	const budget = 1 << 20
	rt := moejs.NewRuntime(moejs.Options{MaxAllocBytes: budget})
	src := "let o;\n"
	for i := 0; i < 12000; i++ {
		src += "o = {};\n"
	}
	src += "throw new Error('boom');\n"
	_, err := rt.RunScript(allocScript(t, src))
	require.ErrorIs(t, err, moejs.ErrAllocLimit)
	require.False(t, rt.Realm().Interrupted())
}

func TestAllocBudgetClearInterruptDoesNotCancelOverrun(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{MaxAllocBytes: allocBudget})
	require.NoError(t, rt.SetGlobal("clear", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
		r.ClearInterrupt()
		return moejs.Undefined(), nil
	})))
	var err error
	slack := uint64(allocBudget)*4 + 32<<20
	before, peak := heapSpan(func() {
		_, err = rt.RunScript(allocScript(t, `const keep = []; for (let i = 0; i < 512; i++) { keep.push("x".repeat(1 << 20)); clear(); }`))
	})
	require.ErrorIs(t, err, moejs.ErrAllocLimit)
	require.LessOrEqual(t, peak, before+slack, "heap grew past the budget slack")
	require.False(t, rt.Realm().Interrupted())
}

func TestAllocBudgetInterruptRace(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{MaxAllocBytes: 1 << 20})
	s := allocScript(t, `const a = []; for (;;) a.push({});`)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				rt.Interrupt(errors.New("stop"))
				time.Sleep(time.Millisecond)
			}
		}
	}()
	_, err := rt.RunScript(s)
	close(stop)
	var ie *moejs.InterruptedError
	require.ErrorAs(t, err, &ie)
}
