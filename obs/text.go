package obs

import (
	"strconv"
	"strings"
)

// textBuilder renders Prometheus text exposition format.
//
// The format is simple enough to emit directly, and doing so keeps the metrics
// surface readable by a standard scraper without adding a client library to a
// project whose core packages are stdlib-only.
type textBuilder struct {
	out strings.Builder
}

func (b *textBuilder) metric(name, kind string, value float64) {
	b.out.WriteString("# TYPE ")
	b.out.WriteString(name)
	b.out.WriteString(" ")
	b.out.WriteString(kind)
	b.out.WriteString("\n")
	b.out.WriteString(name)
	b.out.WriteString(" ")
	b.out.WriteString(strconv.FormatFloat(value, 'g', -1, 64))
	b.out.WriteString("\n")
}

func (b *textBuilder) String() string { return b.out.String() }
