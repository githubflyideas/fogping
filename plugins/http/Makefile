# `make` builds the executable fogping runs. It must be called `probe`.
# No dependencies: the standard library is enough, so this builds offline.
probe: main.go
	CGO_ENABLED=0 go build -trimpath -o probe .

test:
	go test ./...

clean:
	rm -f probe
.PHONY: test clean
