package main

import (
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// tableWidth is what a table is fitted to: $COLUMNS, else the terminal's
// width, else (not a terminal) 100.
func tableWidth() int {
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 0 {
		return n
	}
	if n := ttyWidth(os.Stdout); n > 0 {
		return n
	}
	return 100
}

// shortNames fits names to width runes and keeps them apart. A matrix
// job's name, "build (linux, dev, bucket-1, x) / deploy", first loses the
// values every job of its matrix has, "build (linux, …, x) / deploy"; then
// the end its matrix shares, "build (linux, …, x)…"; then its values' end,
// its head kept short. Any other long name is cut in its middle. Names
// that would then read the same are cut less, until they differ.
func shortNames(names []string, width int) map[string]string {
	width = max(width, 8)
	parts := map[string]mparts{}
	groups := map[[2]string][]string{}
	for _, n := range names {
		if p, ok := matrix(n); ok {
			parts[n] = p
			k := [2]string{p.head, p.tail}
			groups[k] = append(groups[k], n)
		}
	}
	// The values (by place) and the end that every job of a matrix shares.
	for k, ns := range groups {
		if len(ns) < 2 {
			// Not a matrix with others: cut like any other name.
			delete(groups, k)
			for _, n := range ns {
				delete(parts, n)
			}
			continue
		}
		most := 0
		for _, n := range ns {
			most = max(most, len(parts[n].vals))
		}
		same := make([]bool, most)
		for i := range most {
			have, first, agree := 0, "", true
			for _, n := range ns {
				v := parts[n].vals
				if i >= len(v) {
					continue
				}
				if have == 0 {
					first = v[i]
				} else if v[i] != first {
					agree = false
				}
				have++
			}
			same[i] = agree && have >= 2
		}
		for _, n := range ns {
			p := parts[n]
			var keep []string
			for i, v := range p.vals {
				switch {
				case !same[i]:
					keep = append(keep, v)
				case len(keep) == 0 || keep[len(keep)-1] != "…":
					keep = append(keep, "…")
				}
			}
			p.vals, p.sharedTail = keep, true
			parts[n] = p
		}
	}
	short := func(n string, budget int) string {
		p, ok := parts[n]
		if !ok || utf8.RuneCountInString(n) <= budget {
			return cutMiddle(n, budget)
		}
		return p.fit(budget)
	}
	out := map[string]string{}
	longest := 0
	for _, n := range names {
		out[n] = short(n, width)
		longest = max(longest, utf8.RuneCountInString(n))
	}
	for budget := width + 1; ; budget++ {
		seen := map[string][]string{}
		for _, n := range names {
			seen[out[n]] = append(seen[out[n]], n)
		}
		clash := false
		for _, ns := range seen {
			if len(ns) < 2 || allSame(ns) {
				continue
			}
			clash = true
			for _, n := range ns {
				// Keep where it parts from the others.
				at := 0
				for _, o := range ns {
					if o != n {
						at = max(at, commonPrefix(n, o))
					}
				}
				switch {
				case budget > longest:
					out[n] = n
				case budget == width+1:
					out[n] = cutAround(n, at, width)
				default:
					out[n] = cutAround(n, at, budget)
				}
			}
		}
		if !clash || budget > longest {
			return out
		}
	}
}

func allSame(ns []string) bool {
	for _, n := range ns {
		if n != ns[0] {
			return false
		}
	}
	return true
}

// mparts is a matrix job's name: "head (v1, v2, …)tail", or as GitHub cuts
// a long one, "head (v1, v2...tail" (closer "...").
type mparts struct {
	head       string
	vals       []string
	closer     string // ")" or "..."
	tail       string
	sharedTail bool // its matrix's other jobs end the same
}

func matrix(n string) (mparts, bool) {
	i := strings.Index(n, " (")
	if i < 0 {
		return mparts{}, false
	}
	rest := n[i+2:]
	j, closer := strings.Index(rest, ")"), ")"
	if k := strings.Index(rest, "..."); k >= 0 && (j < 0 || k < j) {
		j, closer = k, "..."
	}
	if j < 0 {
		return mparts{}, false
	}
	return mparts{head: n[:i], vals: strings.Split(rest[:j], ", "), closer: closer, tail: rest[j+len(closer):]}, true
}

// fit: the whole name if it fits; else without the end its matrix shares;
// else the head (cut to a third) and as much of the values as fits.
func (p mparts) fit(budget int) string {
	vals := strings.Join(p.vals, ", ")
	closer := p.closer
	if closer == "..." {
		closer = "…"
	}
	whole := p.head + " (" + vals + closer + p.tail
	if utf8.RuneCountInString(whole) <= budget {
		return whole
	}
	if p.sharedTail && p.tail != "" {
		end := closer
		if end == ")" {
			end = ")…"
		}
		if s := p.head + " (" + vals + end; utf8.RuneCountInString(s) <= budget {
			return s
		}
	}
	head := p.head
	if utf8.RuneCountInString(head) > budget/3 {
		head = cutMiddle(head, max(budget/3, 4))
	}
	s := []rune(head + " (" + vals)
	if len(s) <= budget {
		return string(s)
	}
	return string(s[:budget-1]) + "…"
}

// cutMiddle keeps a name's first two thirds and its end, with … between.
func cutMiddle(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	tail := (width - 1) / 3
	head := width - 1 - tail
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

// commonPrefix is how many runes a and b begin with alike.
func commonPrefix(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	n := 0
	for n < len(ra) && n < len(rb) && ra[n] == rb[n] {
		n++
	}
	return n
}

// cutAround cuts s to width keeping the rune at index at (where it parts
// from a name like it) in view: its head, …, the runes around at, …, its end.
func cutAround(s string, at, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	tail := (width - 1) / 3
	if at < width-1-tail {
		return cutMiddle(s, width) // the head shows it already
	}
	head, end := width/4, width/6
	mid := width - head - end - 2
	from := min(max(at-mid/3, head), len(r)-end-mid)
	if from <= head {
		return cutMiddle(s, width)
	}
	if from+mid >= len(r)-end {
		return string(r[:head]) + "…" + string(r[from:])
	}
	return string(r[:head]) + "…" + string(r[from:from+mid]) + "…" + string(r[len(r)-end:])
}
