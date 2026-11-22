package library

// distance computes Damerau-Levenshtein distances over runes, keeping its
// buffers between calls so a scan over many words allocates once.
type distance struct {
	b          []rune
	r0, r1, r2 []int
}

func damerauLevenshtein(a, b string, maxDist int) int {
	var d distance
	return d.between([]rune(a), b, maxDist)
}

func grow(row []int, n int) []int {
	if cap(row) < n {
		return make([]int, n)
	}
	return row[:n]
}

// between returns maxDist+1 as soon as the distance is known to exceed maxDist.
func (d *distance) between(a []rune, bs string, maxDist int) int {
	d.b = d.b[:0]
	for _, r := range bs {
		d.b = append(d.b, r)
	}
	b := d.b
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	if diff := la - lb; diff > maxDist || -diff > maxDist {
		return maxDist + 1
	}
	d.r0, d.r1, d.r2 = grow(d.r0, lb+1), grow(d.r1, lb+1), grow(d.r2, lb+1)
	r0, r1, r2 := d.r0, d.r1, d.r2
	for j := range r1 {
		r1[j] = j
	}
	for i := 1; i <= la; i++ {
		r2[0] = i
		minInRow := i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			val := min(r1[j]+1, r2[j-1]+1, r1[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				val = min(val, r0[j-2]+1)
			}
			r2[j] = val
			minInRow = min(minInRow, val)
		}
		if minInRow > maxDist {
			return maxDist + 1
		}
		r0, r1, r2 = r1, r2, r0
	}
	return r1[lb]
}
