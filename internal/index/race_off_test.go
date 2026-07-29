//go:build !race

package index

// raceEnabled reports whether the binary was built with -race. See race_on_test.go.
const raceEnabled = false
