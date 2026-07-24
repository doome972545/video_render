# videoremix build targets.
#
# On Windows you can also use scripts/build.ps1 and scripts/fetch-binaries.ps1.

BINARY      := videoremix
CMD         := ./cmd/videoremix
EMBED_DIR   := internal/binaries/embedded

# Detect .exe suffix on Windows.
ifeq ($(OS),Windows_NT)
	EXT := .exe
else
	EXT :=
endif

.PHONY: all dev portable test vet fmt clean tidy

all: dev

## dev: small binary; relies on ffmpeg/yt-dlp on PATH or in a bin/ folder.
dev:
	go build -o $(BINARY)$(EXT) $(CMD)

## portable: self-contained binary with ffmpeg/ffprobe/yt-dlp embedded.
## Requires the executables to be present in $(EMBED_DIR) first.
portable:
	go build -tags embed_binaries -o $(BINARY)-portable$(EXT) $(CMD)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

clean:
	- rm -f $(BINARY)$(EXT) $(BINARY)-portable$(EXT)
