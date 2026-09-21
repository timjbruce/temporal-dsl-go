package workflow

import (
	"time"

	"dsl/dsl"
)

// ActivityDuration exposes the simulated step duration to tests. It is a plain
// function, not a method, so registering Activities with a worker still picks up
// only the real activity methods.
func ActivityDuration(a *Activities, def dsl.StepDef, seed string) time.Duration {
	return a.duration(def, seed)
}
