module github.com/datcal/ambar

// Minor version only, not a patch pin: golang:1.26-alpine in the Dockerfile
// should be able to build this without the toolchain auto-downloading a newer
// patch release mid-build.
go 1.26

require (
	github.com/HugoSmits86/nativewebp v1.3.0
	github.com/bodgit/sevenzip v1.6.5
	github.com/go-audio/wav v1.1.0
	github.com/hajimehoshi/go-mp3 v0.3.4
	github.com/jfreymuth/oggvorbis v1.0.5
	github.com/mewkiz/flac v1.0.13
	github.com/nwaples/rardecode/v2 v2.3.0
	github.com/oov/psd v0.0.0-20260122084234-c463b6a89e2f
	github.com/qmuntal/gltf v0.28.0
	github.com/srwiley/oksvg v0.0.0-20221011165216-be6e8873101c
	github.com/srwiley/rasterx v0.0.0-20220730225603-2ab79fcdd4ef
	golang.org/x/crypto v0.54.0
	golang.org/x/image v0.44.0
	golang.org/x/sys v0.47.0
	golang.org/x/term v0.45.0
	modernc.org/sqlite v1.55.0
)

require (
	github.com/andybalholm/brotli v1.2.2 // indirect
	github.com/bodgit/plumbing v1.3.0 // indirect
	github.com/bodgit/windows v1.0.1 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-audio/audio v1.0.0 // indirect
	github.com/go-audio/riff v1.0.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gopherjs/gopherjs v1.21.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/icza/bitio v1.1.0 // indirect
	github.com/jfreymuth/vorbis v1.0.2 // indirect
	github.com/klauspost/compress v1.19.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mewkiz/pkg v0.0.0-20250417130911-3f050ff8c56d // indirect
	github.com/mewpkg/term v0.0.0-20241026122259-37a80af23985 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.27 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/stangelandcl/ppmd v0.1.1 // indirect
	github.com/ulikunitz/xz v0.5.15 // indirect
	go4.org v0.0.0-20260112195520-a5071408f32f // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	modernc.org/libc v1.74.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
