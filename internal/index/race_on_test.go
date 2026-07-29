//go:build race

package index

// raceEnabled reports whether the binary was built with -race.
//
// The race detector multiplies runtime by roughly 30x, which makes any absolute
// wall-clock budget meaningless. See scale_test.go.
const raceEnabled = true
