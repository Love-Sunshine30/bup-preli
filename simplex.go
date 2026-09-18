package main

import (
	"errors"
	"math"
)

// Two-phase dense-tableau simplex for:  min c'x  s.t.  Ax = b,  x >= 0.
// Dantzig pricing with a Bland's-rule fallback so it cannot cycle.

var ErrInfeasible = errors.New("lp: infeasible")
var ErrUnbounded = errors.New("lp: unbounded")
var ErrIterLimit = errors.New("lp: iteration limit")

const (
	eps      = 1e-9
	maxIters = 60000
	blandAt  = 4000
)

type lpTab struct {
	m, n  int
	t     [][]float64 // (m+1) x (n+1); last col RHS, last row reduced costs
	basis []int
}

// SolveLP returns x (length n) and the objective value.
func SolveLP(c []float64, A [][]float64, b []float64) ([]float64, float64, error) {
	m := len(A)
	n := len(c)

	// Copy and make RHS non-negative.
	aa := make([][]float64, m)
	bb := make([]float64, m)
	for i := 0; i < m; i++ {
		row := make([]float64, n)
		copy(row, A[i])
		bb[i] = b[i]
		if bb[i] < 0 {
			for j := range row {
				row[j] = -row[j]
			}
			bb[i] = -bb[i]
		}
		aa[i] = row
	}

	// Phase 1 tableau: n original + m artificial columns.
	w := n + m
	tab := &lpTab{m: m, n: w, basis: make([]int, m)}
	tab.t = make([][]float64, m+1)
	for i := 0; i < m; i++ {
		r := make([]float64, w+1)
		copy(r, aa[i])
		r[n+i] = 1
		r[w] = bb[i]
		tab.t[i] = r
		tab.basis[i] = n + i
	}
	obj := make([]float64, w+1)
	for j := 0; j < n; j++ {
		s := 0.0
		for i := 0; i < m; i++ {
			s += aa[i][j]
		}
		obj[j] = -s
	}
	s := 0.0
	for i := 0; i < m; i++ {
		s += bb[i]
	}
	obj[w] = -s
	tab.t[m] = obj

	if err := tab.run(n); err != nil {
		return nil, 0, err
	}
	if -tab.t[m][w] > 1e-6 {
		return nil, 0, ErrInfeasible
	}

	// Drive artificials out of the basis; drop redundant rows.
	keep := make([]bool, m)
	for i := range keep {
		keep[i] = true
	}
	for i := 0; i < m; i++ {
		if tab.basis[i] < n {
			continue
		}
		piv := -1
		for j := 0; j < n; j++ {
			if math.Abs(tab.t[i][j]) > 1e-7 {
				piv = j
				break
			}
		}
		if piv >= 0 {
			tab.pivot(i, piv)
		} else {
			keep[i] = false
		}
	}

	// Phase 2: rebuild a tableau over the original columns only.
	rows := [][]float64{}
	basis := []int{}
	for i := 0; i < m; i++ {
		if !keep[i] {
			continue
		}
		r := make([]float64, n+1)
		copy(r, tab.t[i][:n])
		r[n] = tab.t[i][w]
		rows = append(rows, r)
		basis = append(basis, tab.basis[i])
	}
	m2 := len(rows)
	t2 := &lpTab{m: m2, n: n, basis: basis}
	t2.t = append(rows, make([]float64, n+1))
	for j := 0; j <= n; j++ {
		v := 0.0
		if j < n {
			v = c[j]
		}
		for i := 0; i < m2; i++ {
			v -= c[basis[i]] * t2.t[i][j]
		}
		t2.t[m2][j] = v
	}
	if err := t2.run(n); err != nil {
		return nil, 0, err
	}

	x := make([]float64, n)
	for i := 0; i < m2; i++ {
		if t2.basis[i] < n {
			x[t2.basis[i]] = t2.t[i][n]
		}
	}
	z := 0.0
	for j := 0; j < n; j++ {
		z += c[j] * x[j]
	}
	return x, z, nil
}

// run performs simplex iterations; only columns < limit may enter.
func (tb *lpTab) run(limit int) error {
	m, n := tb.m, tb.n
	for it := 0; it < maxIters; it++ {
		enter := -1
		if it < blandAt {
			best := -eps
			for j := 0; j < limit; j++ {
				if tb.t[m][j] < best {
					best = tb.t[m][j]
					enter = j
				}
			}
		} else {
			for j := 0; j < limit; j++ {
				if tb.t[m][j] < -eps {
					enter = j
					break
				}
			}
		}
		if enter < 0 {
			return nil
		}
		leave, best := -1, math.Inf(1)
		for i := 0; i < m; i++ {
			a := tb.t[i][enter]
			if a > 1e-10 {
				r := tb.t[i][n] / a
				if r < best-1e-12 || (math.Abs(r-best) <= 1e-12 && leave >= 0 && tb.basis[i] < tb.basis[leave]) {
					best, leave = r, i
				}
			}
		}
		if leave < 0 {
			return ErrUnbounded
		}
		tb.pivot(leave, enter)
	}
	return ErrIterLimit
}

func (tb *lpTab) pivot(r, c int) {
	n := tb.n
	p := tb.t[r][c]
	for j := 0; j <= n; j++ {
		tb.t[r][j] /= p
	}
	for i := 0; i <= tb.m; i++ {
		if i == r {
			continue
		}
		f := tb.t[i][c]
		if f == 0 {
			continue
		}
		for j := 0; j <= n; j++ {
			tb.t[i][j] -= f * tb.t[r][j]
		}
	}
	tb.basis[r] = c
}
