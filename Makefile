#!make
PACKAGE_BASE := 'github.com/marco-rozz/ready'
PACKAGE_NAME := 'ready'

AGENT_BIN := 'ready'

DATE := snapshot-local-`date +%Y%m%d%H%M%S`
RELEASE_VERSION := `git describe --tags 2>/dev/null`
BUILD := `git rev-parse HEAD`

# go mod init -modfile=go.tool.mod github.com/marco-rozz/ready
install-tools:
	go get -tool -modfile=go.tool.mod github.com/golangci/golangci-lint/v2/cmd/golangci-lint
	go get -tool -modfile=go.tool.mod golang.org/x/vuln/cmd/govulncheck
	go get -tool -modfile=go.tool.mod mvdan.cc/gofumpt

tidy:
	go mod tidy -compat=1.27

qa: tidy lint gofumpt vuln test

lint:
	go fmt $(shell go list ./... | grep -v /vendor/ | grep -v ready-landing) && \
	go vet $(shell go list ./... | grep -v /vendor/ | grep -v ready-landing) && \
	go tool -modfile=go.tool.mod golangci-lint run -v --issues-exit-code 1

lint-fix:
	go tool -modfile=go.tool.mod golangci-lint run -v --fix

#gofumpt:
#	go tool -modfile=go.tool.mod gofumpt -w -extra -lang go1.25 .

gofumpt:
	go tool -modfile=go.tool.mod gofumpt -w -extra -lang go1.27 $(shell go list -f '{{.Dir}}' ./... | grep -v '/vendor/' | grep -v '/pkg/mod/')

build:
	CGO_ENABLED=0 go build -o ${AGENT_BIN} ${PACKAGE_BASE}/cmd

# snapshot runs goreleaser in snapshot mode: a full local bundle check before
# a tag is cut (archives, checksums, the skill library in the archive) — no
# artifacts are published and nothing touches the git history.
snapshot:
	goreleaser release --snapshot --clean

raspy:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ${AGENT_BIN} ${PACKAGE_BASE}/cmd

# Raspberry Pi (armhf): build static — the noshiftprobe tag removes purego's
# cgo_import_dynamic, which would otherwise force a dynamically linked ELF
# whose /lib/ld-linux.so.3 interpreter does not exist on armhf (armv6/armv7).
raspy1:
	GOOS=linux GOARCH=arm GOARM=6 CGO_ENABLED=0 go build -tags noshiftprobe -o ${AGENT_BIN} ${PACKAGE_BASE}/cmd

vuln:
	go tool -modfile=go.tool.mod govulncheck -show verbose ./...

test:
	go test -race -covermode=atomic $(shell go list ./... | grep -v /vendor/)
	# go test -bench=. $(shell go list ./... | grep -v /vendor/)
	go test -coverprofile=coverage.out ./...    # Generate report coverage Go

bench:
	go test -bench=. $(shell go list ./... | grep -v /vendor/)

clean:
	rm -rf ${AGENT_BIN}
	rm -rf ${AGENT_BIN}.exe

# deploy on my (2014) Raspberry Pi 1 B+ (armv6l, armhf)
scp: clean raspy1
	scp ${AGENT_BIN} raspy@192.168.1.115:${AGENT_BIN}

.PHONY: clean test build format snapshot
