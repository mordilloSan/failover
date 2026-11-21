# Local targets. On the TNAS use deploy/docker-compose.yml directly (see README).
.PHONY: test lint e2e start stop logs server-down server-up

test:
	go test -race ./...

lint:
	test -z "$$(gofmt -l .)"
	go vet ./...
	golangci-lint run ./...
	shellcheck e2e/run.sh scripts/third-party-notices.sh
	shfmt -d e2e/run.sh scripts/third-party-notices.sh

# Full end-to-end test against the local Docker (~3 min).
e2e:
	./e2e/run.sh test

# Agent + fake server + fake TNAS NPM, left running to try the web UI.
start:
	./e2e/run.sh start

stop:
	./e2e/run.sh stop

logs:
	docker logs -f e2e-agent

# Simulate the server failing / coming back (only while `make start` runs).
server-down:
	docker stop e2e-server

server-up:
	docker start e2e-server
