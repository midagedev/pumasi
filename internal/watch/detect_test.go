package watch

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

func TestPrefixL1MatchesDirect(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for n := 1; n < 60; n++ {
		x := make([]float64, n)
		for i := range x {
			x[i] = math.Round(rng.NormFloat64()*4) / 2 // ties too
		}
		for _, rev := range []bool{false, true} {
			got := prefixL1(x, rev)
			for p := 1; p <= n; p++ {
				seg := slices.Clone(x[:p])
				if rev {
					seg = slices.Clone(x[n-p:])
				}
				m := median(slices.Clone(seg))
				want := 0.0
				for _, v := range seg {
					want += math.Abs(v - m)
				}
				if math.Abs(got[p]-want) > 1e-9 {
					t.Fatalf("n %d p %d rev %v: %v want %v", n, p, rev, got[p], want)
				}
			}
		}
	}
}

func TestMannWhitneyZ(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5}
	if z := mannWhitneyZ(a, a); z != 0 {
		t.Fatalf("same: %v", z)
	}
	if z := mannWhitneyZ(a, []float64{6, 7, 8, 9, 10}); z < 2.5 {
		t.Fatalf("larger: %v", z)
	}
	if z := mannWhitneyZ([]float64{6, 7, 8, 9, 10}, a); z > -2.5 {
		t.Fatalf("smaller: %v", z)
	}
}
