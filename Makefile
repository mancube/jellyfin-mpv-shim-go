# mpv-shim-go — build, test, install. Packaging helpers live in packaging/.

BINARY  := mpv-shim
VERSION ?= 1.0.1
PREFIX  ?= /usr/local
BINDIR  ?= $(PREFIX)/bin
APPDIR  := /usr/share/applications
ICONDIR := /usr/share/icons/hicolor/256x256/apps
DESKTOP := packaging/desktop/io.github.ikac.mpv-shim.desktop
ICON    := packaging/icon/io.github.ikac.mpv-shim.png
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt install uninstall desktop app pkg clean

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

test: vet
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

## install: copy the binary and the launcher files (needs root)
install: build
	install -Dm755 $(BINARY) $(DESTDIR)$(BINDIR)/$(BINARY)
	install -Dm644 $(DESKTOP) $(DESTDIR)$(APPDIR)/io.github.ikac.mpv-shim.desktop
	install -Dm644 $(ICON) $(DESTDIR)$(ICONDIR)/io.github.ikac.mpv-shim.png
	@command -v update-desktop-database >/dev/null && \
		update-desktop-database $(DESTDIR)$(APPDIR) || true
	@command -v gtk-update-icon-cache >/dev/null && \
		gtk-update-icon-cache -f -t $(DESTDIR)/usr/share/icons/hicolor || true

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/$(BINARY)
	rm -f $(DESTDIR)$(APPDIR)/io.github.ikac.mpv-shim.desktop
	rm -f $(DESTDIR)$(ICONDIR)/io.github.ikac.mpv-shim.png
	@command -v update-desktop-database >/dev/null && \
		update-desktop-database $(DESTDIR)$(APPDIR) || true

## app: macOS .app bundle (unsigned)
app:
	VERSION=$(VERSION) ./packaging/macos/make-app.sh

## pkg: Arch package in the current directory
pkg:
	makepkg -f packaging/arch/PKGBUILD

clean:
	rm -rf $(BINARY) dist src pkg *.pkg.tar.*
