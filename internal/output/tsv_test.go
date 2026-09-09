package output

import (
	"testing"

	"github.com/rudi-bruchez/argosql/internal/model"
)

func TestTSVEscapes(t *testing.T) {
	cases := []struct {
		c    model.Cell
		want string
	}{
		{nil, `\N`}, {"", ""},
		{`\N`, `\\N`}, {"a\tb\nc", `a\tb\nc`},
	}
	for _, c := range cases {
		if got := EncodeTSVCell(c.c); got != c.want {
			t.Fatalf("%q != %q", got, c.want)
		}
	}
}
