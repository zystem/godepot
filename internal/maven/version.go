package maven

import (
	"strings"
	"unicode"
)

// A version is split into items at ".", "-" and "+", and where digits meet letters; numbers are compared as numbers and the words as Maven compares them (alpha < beta < milestone < rc < snapshot <
// the release < sp < any other word, which are put in the order of the letters). This is the order of Maven's ComparableVersion without its nested lists, which differ only for versions that mix
// several "-" with words.
type item struct {
	num  string // digits, without leading zeros; "" for a word
	word string
}

func items(v string) []item {
	var out []item
	var cur []rune
	digit := false
	flush := func() {
		if len(cur) == 0 {
			return
		}
		s := string(cur)
		if digit {
			s = strings.TrimLeft(s, "0")
			out = append(out, item{num: s})
		} else {
			out = append(out, item{word: strings.ToLower(s)})
		}
		cur = nil
	}
	for _, r := range v {
		switch {
		case r == '.' || r == '-' || r == '+' || r == '_':
			flush()
		case unicode.IsDigit(r):
			if len(cur) > 0 && !digit {
				flush()
			}
			digit = true
			cur = append(cur, r)
		default:
			if len(cur) > 0 && digit {
				flush()
			}
			digit = false
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

func rank(w string) int {
	switch w {
	case "alpha", "a":
		return 1
	case "beta", "b":
		return 2
	case "milestone", "m":
		return 3
	case "rc", "cr":
		return 4
	case "snapshot":
		return 5
	case "", "ga", "final", "release":
		return 6
	case "sp":
		return 7
	}
	return 8
}

// compare orders two versions: -1, 0 or 1.
func compare(a, b string) int {
	x, y := items(a), items(b)
	for i := 0; i < len(x) || i < len(y); i++ {
		var p, q item
		switch {
		case i >= len(x):
			p = pad(y[i])
			q = y[i]
		case i >= len(y):
			p, q = x[i], pad(x[i])
		default:
			p, q = x[i], y[i]
		}
		if c := cmpItem(p, q); c != 0 {
			return c
		}
	}
	return 0
}

// pad is the item that stands for a missing one: 0 where there is a number, the release where there is a word.
func pad(o item) item {
	if o.word == "" {
		return item{num: ""} // the number 0
	}
	return item{word: ""}
}

func cmpItem(p, q item) int {
	pw, qw := p.word != "", q.word != ""
	switch {
	case !pw && !qw:
		if len(p.num) != len(q.num) {
			if len(p.num) < len(q.num) {
				return -1
			}
			return 1
		}
		return strings.Compare(p.num, q.num)
	case pw && qw:
		rp, rq := rank(p.word), rank(q.word)
		if rp != rq {
			if rp < rq {
				return -1
			}
			return 1
		}
		if rp == 8 {
			return strings.Compare(p.word, q.word)
		}
		return 0
	case !pw && qw: // a number against a word: the number is greater, but 0 is the release
		if p.num == "" {
			return cmpRank(6, rank(q.word))
		}
		return 1
	default:
		if q.num == "" {
			return cmpRank(rank(p.word), 6)
		}
		return -1
	}
}

func cmpRank(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
