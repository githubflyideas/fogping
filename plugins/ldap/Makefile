# `make` builds the executable fogping runs. It must be called `probe`.
# The one dependency is the LDAP client library; `go.sum` pins its exact version.
probe: main.go go.mod go.sum
	CGO_ENABLED=0 go build -trimpath -o probe .

test:
	go test ./...

clean:
	rm -f probe
.PHONY: test clean
