BINARY := bin/audiosocket-bridge
TESTER := bin/audiosocket-tester
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vet run-echo run-virtual tester docker-build docker-up docker-down chatgpt-login chatgpt-verify clean

build:
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o $(BINARY) ./cmd/audiosocket-bridge
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o $(TESTER) ./cmd/audiosocket-tester

test:
	go test ./...

vet:
	go vet ./...

run-echo:
	BRIDGE_MODE=echo go run ./cmd/audiosocket-bridge

run-virtual:
	BRIDGE_MODE=virtual go run ./cmd/audiosocket-bridge

tester:
	go run ./cmd/audiosocket-tester -addr 127.0.0.1:9092

docker-build:
	docker compose build

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down

# One-time ChatGPT login: runs the browser with CF_LOGIN_ONLY=true; open
# http://<host>:6080/vnc.html (publish 6080 first) and log in. Afterwards run
# `docker compose up -d camoufox` without the override.
chatgpt-login:
	CF_LOGIN_ONLY=true docker compose up -d --build camoufox

# Simulate a call against the running bridge from inside the network.
chatgpt-verify:
	docker compose exec bridge audiosocket-tester -addr bridge:9092 -duration 5s

clean:
	rm -rf bin
