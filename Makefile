# Root Makefile — builds all binaries into .build/
#
# Requires Go 1.21+ for the -C flag.

.PHONY: all build build-agc build-probe

all: build

build: build-agc build-probe

build-agc:
	go build -C cmd/agc -o ../../.build/agc .

build-probe:
	go build -C cmd/probe -o ../../.build/probe .
