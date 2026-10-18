// Package registry lists every source the CLI runs.
//
// It is a separate package from sources because a source (which must import
// sources for the Source interface and SourceError) cannot also be imported
// by sources without an import cycle.
package registry

import (
	"github.com/mabunemeh/limits-explain/internal/sources"
	"github.com/mabunemeh/limits-explain/internal/sources/cgroup"
	"github.com/mabunemeh/limits-explain/internal/sources/rlimits"
)

// All returns every registered source.
//
// The list is deliberately explicit and boring: adding a source is a
// two-line change, writing the type and appending its constructor here.
func All() []sources.Source {
	return []sources.Source{
		rlimits.New(),
		cgroup.New(),
	}
}
