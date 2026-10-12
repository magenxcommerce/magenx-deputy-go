BINARY   := magenx-deputy-go
PREFIX   ?= /usr/local
SBINDIR  ?= $(PREFIX)/sbin
CONFDIR  ?= /etc/magenx-deputy-go
UNITDIR  ?= /lib/systemd/system

GO       := go
GOFLAGS  := -trimpath
CGO_ENABLED := 0
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "devel")
LDFLAGS  := -X github.com/magenxcommerce/magenx-deputy-go/version.Version=$(VERSION) -s -w

.PHONY: all build install uninstall test clean

all: build

build:
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) .

# config.yaml holds secrets: never overwrite an existing one.
install: build
	install -D -m 0755 $(BINARY) $(DESTDIR)$(SBINDIR)/$(BINARY)
	test -f $(DESTDIR)$(CONFDIR)/config.yaml || install -D -m 0640 config.example.yaml $(DESTDIR)$(CONFDIR)/config.yaml
	install -D -m 0644 $(BINARY).service $(DESTDIR)$(UNITDIR)/$(BINARY).service

# Keeps $(CONFDIR) (secrets); remove it by hand if no longer needed.
uninstall:
	rm -f $(DESTDIR)$(SBINDIR)/$(BINARY)
	rm -f $(DESTDIR)$(UNITDIR)/$(BINARY).service

test:
	$(GO) test -v ./...

clean:
	rm -f $(BINARY)
