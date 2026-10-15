.PHONY: build test test-python check clean

PYTHON ?= .venv/bin/python

build:
	go build -trimpath -o bin/tibo-monitor ./cmd/tibo-monitor

test:
	go test -race ./...

test-python:
	$(PYTHON) -m unittest discover -s scripts -p 'test_*.py' -v

check: test test-python
	go vet ./...

clean:
	rm -rf bin coverage.out
