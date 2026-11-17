package metrics

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	r := New()
	c := r.Counter("godepot_things_total", "Things done.", "kind")
	g := r.Gauge("godepot_up", "One while up.")
	plain := r.Counter("godepot_passes_total", "Passes.", "")
	c.Add("b", 2)
	c.Add("a", 1)
	c.Add("b", 3)
	g.Set(1)
	var sb strings.Builder
	r.Render(&sb)
	want := `# HELP godepot_things_total Things done.
# TYPE godepot_things_total counter
godepot_things_total{kind="a"} 1
godepot_things_total{kind="b"} 5
# HELP godepot_up One while up.
# TYPE godepot_up gauge
godepot_up 1
# HELP godepot_passes_total Passes.
# TYPE godepot_passes_total counter
godepot_passes_total 0
`
	if sb.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", sb.String(), want)
	}
	if c.Value("b") != 5 || g.Value() != 1 || plain.Value("") != 0 {
		t.Fatal("values")
	}
}

func TestTwiceIsAProgrammingError(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a metric registered twice was accepted")
		}
	}()
	r := New()
	r.Gauge("godepot_x", "x")
	r.Gauge("godepot_x", "x")
}
