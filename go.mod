module github.com/datcal/ambar

// Minor version only, not a patch pin: golang:1.26-alpine in the Dockerfile
// should be able to build this without the toolchain auto-downloading a newer
// patch release mid-build.
go 1.26

require (
	github.com/HugoSmits86/nativewebp v1.3.0
	github.com/oov/psd v0.0.0-20260122084234-c463b6a89e2f
	github.com/srwiley/oksvg v0.0.0-20221011165216-be6e8873101c
	github.com/srwiley/rasterx v0.0.0-20220730225603-2ab79fcdd4ef
	golang.org/x/crypto v0.54.0
	golang.org/x/image v0.44.0
	golang.org/x/term v0.45.0
	modernc.org/sqlite v1.55.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gopherjs/gopherjs v1.21.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	modernc.org/libc v1.74.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
