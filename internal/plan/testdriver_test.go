package plan

import (
	"fmt"
	"io"

	"github.com/rudi-bruchez/argosql/internal/model"
)

// fakeSink is a minimal model.Sink: every table's rows and every
// notice stay in memory, nothing ever touches a file - the same
// minimal-sink idiom tests/integration/status_test.go's own
// captureSink follows (that one lives in a different module-internal
// package and cannot be imported here), scoped to exactly what this
// package's own tests need to assert on.
type fakeSink struct {
	tables  []fakeTable
	cur     *fakeTable
	notices []model.Notice
}

type fakeTable struct {
	spec               model.TableSpec
	rows               [][]model.Cell
	collectionComplete bool
	propertiesComplete bool
}

func (s *fakeSink) Begin(spec model.TableSpec) error {
	s.cur = &fakeTable{spec: spec}
	return nil
}

func (s *fakeSink) Row(row []model.Cell) error {
	if s.cur == nil {
		return fmt.Errorf("fakeSink: Row called with no open table")
	}
	s.cur.rows = append(s.cur.rows, row)
	return nil
}

func (s *fakeSink) End(collectionComplete, propertiesComplete bool) error {
	if s.cur == nil {
		return fmt.Errorf("fakeSink: End called with no open table")
	}
	s.cur.collectionComplete = collectionComplete
	s.cur.propertiesComplete = propertiesComplete
	s.tables = append(s.tables, *s.cur)
	s.cur = nil
	return nil
}

func (s *fakeSink) File(kind, suffix string, src io.Reader) (model.Artifact, error) {
	b, err := io.ReadAll(src)
	if err != nil {
		return model.Artifact{}, err
	}
	return model.Artifact{Kind: kind, Path: "(fakeSink, never written to disk)", Bytes: int64(len(b)), Complete: true}, nil
}

func (s *fakeSink) Notice(n model.Notice) { s.notices = append(s.notices, n) }

func (s *fakeSink) table(name string) *fakeTable {
	for i := range s.tables {
		if s.tables[i].spec.Name == name {
			return &s.tables[i]
		}
	}
	return nil
}

func (s *fakeSink) noticeWithKind(kind string) *model.Notice {
	for i := range s.notices {
		if s.notices[i].Kind == kind {
			return &s.notices[i]
		}
	}
	return nil
}
