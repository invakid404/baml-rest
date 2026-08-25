package main

import (
	"fmt"
	"math"
	"reflect"
	"testing"
)

// items builds a weighted item list from a name prefix and weights, so the
// distributions below read as the shapes they model.
func items(prefix string, weights ...float64) []Item {
	out := make([]Item, 0, len(weights))
	for i, w := range weights {
		out = append(out, Item{ID: fmt.Sprintf("%s%02d", prefix, i), Weight: w})
	}
	return out
}

// repeat is the long tail: n identical plankton units.
func repeat(prefix string, n int, w float64) []Item {
	weights := make([]float64, n)
	for i := range weights {
		weights[i] = w
	}
	return items(prefix, weights...)
}

func totalWeight(in []Item) float64 {
	t := 0.0
	for _, it := range in {
		t += it.Weight
	}
	return t
}

func flatten(buckets [][]Item) []string {
	var out []string
	for _, b := range buckets {
		for _, it := range b {
			out = append(out, it.ID)
		}
	}
	return out
}

// The distributions the bucketer actually has to survive, anchored on the
// design's §2.4 estimate table (seconds of -race -count=100 work).
var (
	// uniformTail: the plankton alone, no whale.
	uniformTail = repeat("tail", 24, 40)

	// skewed: the real baml-rest shape read off the design's estimate
	// table — a smooth long tail plus two dominators.
	skewed = append(
		items("big", 900, 420, 160, 120, 110, 95, 90, 80),
		repeat("small", 16, 25)...,
	)

	// whaleAndPlankton: the pathological case. One 900s package and 20
	// cheap ones — the shape that makes per-package bucketing useless.
	whaleAndPlankton = append(items("whale", 900), repeat("plankton", 20, 30)...)

	// harpooned: the same total work, but the whale has been count-sharded
	// six ways and the runner-up three ways. This is the distribution the
	// hybrid whale-splitting decision is supposed to produce.
	harpooned = append(
		append(repeat("whaleshard", 6, 150), repeat("debamlslice", 3, 140)...),
		repeat("plankton", 20, 30)...,
	)
)

func TestKarmarkarKarpSchedulesEveryItemExactlyOnce(t *testing.T) {
	cases := []struct {
		name  string
		items []Item
		k     int
	}{
		{"uniform tail", uniformTail, 6},
		{"skewed", skewed, 6},
		{"whale and plankton", whaleAndPlankton, 6},
		{"harpooned", harpooned, 6},
		{"k of one", skewed, 1},
		{"k above item count", items("few", 10, 20, 30), 6},
		{"single item", items("only", 42), 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buckets := karmarkarKarp(tc.items, tc.k)
			if len(buckets) != tc.k {
				t.Fatalf("got %d buckets, want %d", len(buckets), tc.k)
			}
			seen := map[string]int{}
			for _, b := range buckets {
				for _, it := range b {
					seen[it.ID]++
				}
			}
			if len(seen) != len(tc.items) {
				t.Errorf("scheduled %d distinct items, want %d", len(seen), len(tc.items))
			}
			for _, it := range tc.items {
				if seen[it.ID] != 1 {
					t.Errorf("item %s scheduled %d times, want exactly 1", it.ID, seen[it.ID])
				}
			}
			// Conservation: no weight invented or lost.
			got := 0.0
			for _, b := range buckets {
				got += totalWeight(b)
			}
			if math.Abs(got-totalWeight(tc.items)) > 1e-9 {
				t.Errorf("total bucket weight %.4f, want %.4f", got, totalWeight(tc.items))
			}
		})
	}
}

func TestKarmarkarKarpIsDeterministic(t *testing.T) {
	// Same store + same K must yield the same buckets, every run: a plan
	// that reshuffles on re-run is unreviewable and makes CI caches useless.
	for _, k := range []int{2, 4, 6, 8} {
		first := flatten(karmarkarKarp(skewed, k))
		for i := 0; i < 5; i++ {
			if got := flatten(karmarkarKarp(skewed, k)); !reflect.DeepEqual(first, got) {
				t.Fatalf("k=%d run %d differs from run 0:\n first %v\n got   %v", k, i, first, got)
			}
		}
		// Input order must not matter either — the partitioner sorts.
		shuffled := append([]Item(nil), skewed...)
		for i, j := 0, len(shuffled)-1; i < j; i, j = i+1, j-1 {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		}
		if got := flatten(karmarkarKarp(shuffled, k)); !reflect.DeepEqual(first, got) {
			t.Errorf("k=%d: reversing the input changed the partition", k)
		}
	}
}

func TestKarmarkarKarpBalanceQuality(t *testing.T) {
	// The only honest yardstick is the theoretical lower bound on any
	// K-way makespan: max(total/K, heaviest single unit). Measuring against
	// total/K alone would score a floor-bound distribution as a balancer
	// failure when the balancer had no move to make.
	cases := []struct {
		name      string
		items     []Item
		k         int
		maxOver   float64 // percent over max(ideal, heaviest item)
		wantExact float64 // if non-zero, the makespan must be exactly this
		note      string
	}{
		{name: "uniform tail", items: uniformTail, k: 6, wantExact: 160,
			note: "24 identical units over 6 buckets partitions exactly"},
		{name: "skewed real shape", items: skewed, k: 6, wantExact: 900,
			note: "floor-bound: the 900s package alone exceeds total/6, so the makespan IS the whale"},
		{name: "harpooned", items: harpooned, k: 6, maxOver: 5,
			note: "after whale-splitting the tail packs tightly around total/6"},
		{name: "harpooned k=4", items: harpooned, k: 4, maxOver: 5},
		{name: "harpooned k=8", items: harpooned, k: 8, wantExact: 280,
			note: "optimal: 9 units weigh >=140 but only 8 buckets exist, so some bucket must hold two — the cheapest such pair is 140+140"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buckets := karmarkarKarp(tc.items, tc.k)
			ideal := totalWeight(tc.items) / float64(tc.k)
			heaviest := 0.0
			for _, it := range tc.items {
				if it.Weight > heaviest {
					heaviest = it.Weight
				}
			}
			bound := math.Max(ideal, heaviest)
			span := makespan(buckets)
			t.Logf("makespan %.1fs, ideal %.1fs, lower bound %.1fs (%.2f%% over) — %s",
				span, ideal, bound, (span-bound)/bound*100, tc.note)
			if tc.wantExact != 0 {
				if math.Abs(span-tc.wantExact) > 1e-9 {
					t.Fatalf("makespan %.1fs, want exactly %.1fs — %s", span, tc.wantExact, tc.note)
				}
				return
			}
			if over := (span - bound) / bound * 100; over > tc.maxOver {
				t.Errorf("makespan %.1fs is %.2f%% over the %.1fs lower bound, allowed %.2f%% — %s",
					span, over, bound, tc.maxOver, tc.note)
			}
		})
	}
}

func TestUnsplitWhaleSetsTheFloor(t *testing.T) {
	// The single most important feasibility fact in the design: while the
	// dominator is one indivisible unit, no K can push the makespan below
	// its weight. This test is the executable statement of that floor —
	// and of why the hybrid whale-splitting decision exists.
	for _, k := range []int{2, 3, 4, 6, 8, 16} {
		span := makespan(karmarkarKarp(whaleAndPlankton, k))
		if span < 900 {
			t.Fatalf("k=%d: makespan %.1fs is below the 900s whale — the floor cannot be beaten without splitting it", k, span)
		}
		if k >= 4 && span != 900 {
			t.Errorf("k=%d: makespan %.1fs, want exactly the whale's 900s once the tail fits around it", k, span)
		}
	}

	// And the payoff: harpooning the same total work drops the makespan far
	// below the floor.
	span := makespan(karmarkarKarp(harpooned, 6))
	if span >= 900 {
		t.Errorf("harpooned makespan %.1fs did not beat the 900s floor", span)
	}
	t.Logf("floor %.1fs -> harpooned %.1fs at K=6", 900.0, span)
}

func TestKarmarkarKarpIsAtLeastAsGoodAsLPT(t *testing.T) {
	// Owner decision 4 upgraded the design's proposed LPT to KK. LPT is
	// kept one function away so that decision stays checkable rather than
	// assumed. Per-instance superiority is NOT a property of KK in general
	// (see TestKarmarkarKarpBeatsLPTOnAverage for the claim that is); this
	// pins the specific distributions baml-rest models, where a regression
	// to a worse partition would be a real loss of wall-time.
	for _, tc := range []struct {
		name  string
		items []Item
	}{
		{"uniform tail", uniformTail},
		{"skewed real shape", skewed},
		{"whale and plankton", whaleAndPlankton},
		{"harpooned", harpooned},
	} {
		for _, k := range []int{2, 4, 6, 8} {
			kk := makespan(karmarkarKarp(tc.items, k))
			greedy := makespan(longestProcessingTime(tc.items, k))
			if kk > greedy+1e-9 {
				t.Errorf("%s k=%d: KK makespan %.2f worse than LPT %.2f", tc.name, k, kk, greedy)
			}
			t.Logf("%s k=%d: KK %.1fs vs LPT %.1fs", tc.name, k, kk, greedy)
		}
	}
}

// lcg is a hermetic deterministic generator: the average-case claim below
// must not move when the toolchain's math/rand does.
type lcg uint64

func (l *lcg) next() float64 {
	*l = lcg(uint64(*l)*6364136223846793005 + 1442695040888963407)
	return float64((uint64(*l)>>11)&((1<<40)-1)) / float64(uint64(1)<<40)
}

// skewedSample draws a long-tailed distribution: many cheap units, a few
// expensive ones — the shape every real test suite has.
func skewedSample(seed uint64, n int) []Item {
	r := lcg(seed)
	out := make([]Item, 0, n)
	for i := 0; i < n; i++ {
		u := r.next()
		if u < 1e-6 {
			u = 1e-6
		}
		// -log(u) is an exponential draw; the scale only sets the units.
		w := -math.Log(u) * 40
		out = append(out, Item{ID: fmt.Sprintf("unit%03d", i), Weight: math.Round(w*100) / 100})
	}
	return out
}

func TestKarmarkarKarpBeatsLPTOnAverage(t *testing.T) {
	// The actual claim behind owner decision 4: KK shares LPT's worst-case
	// bound but is better ON AVERAGE. Asserting per-instance superiority
	// would be false — LPT wins the occasional instance — so this measures
	// the mean residual imbalance over a sweep, which is what a split
	// regenerated on every CI run actually experiences.
	var kkTotal, lptTotal float64
	instances := 0
	kkWins, lptWins := 0, 0
	for seed := uint64(1); seed <= 24; seed++ {
		in := skewedSample(seed*7919, 40)
		for _, k := range []int{4, 6, 8} {
			ideal := totalWeight(in) / float64(k)
			kk := (makespan(karmarkarKarp(in, k)) - ideal) / ideal * 100
			lp := (makespan(longestProcessingTime(in, k)) - ideal) / ideal * 100
			kkTotal += kk
			lptTotal += lp
			instances++
			switch {
			case kk < lp-1e-9:
				kkWins++
			case lp < kk-1e-9:
				lptWins++
			}
		}
	}
	kkMean := kkTotal / float64(instances)
	lptMean := lptTotal / float64(instances)
	t.Logf("over %d instances: KK mean imbalance %.3f%% (%d wins), LPT mean %.3f%% (%d wins)",
		instances, kkMean, kkWins, lptMean, lptWins)
	if kkMean >= lptMean {
		t.Errorf("KK mean imbalance %.3f%% is not better than LPT's %.3f%%", kkMean, lptMean)
	}
	if kkWins <= lptWins {
		t.Errorf("KK won %d instances vs LPT's %d; the upgrade should win more often than it loses", kkWins, lptWins)
	}
}

func TestKarmarkarKarpDegenerateK(t *testing.T) {
	if got := karmarkarKarp(skewed, 0); got != nil {
		t.Errorf("k=0 returned %v, want nil", got)
	}
	if got := karmarkarKarp(nil, 3); len(got) != 3 {
		t.Errorf("no items returned %d buckets, want 3 empty ones", len(got))
	}
	single := karmarkarKarp(skewed, 1)
	if len(single) != 1 || len(single[0]) != len(skewed) {
		t.Fatalf("k=1 must put everything in one bucket, got %d buckets", len(single))
	}
}
