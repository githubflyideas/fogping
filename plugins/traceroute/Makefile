# `make` builds the executable fogping runs. It must be called `probe`.
# --locked: build exactly the dependency versions in Cargo.lock.
probe: src/main.rs src/udp_trace.rs Cargo.toml Cargo.lock
	cargo build --release --locked
	cp target/release/probe probe

test:
	cargo test --locked

clean:
	cargo clean
	rm -f probe
.PHONY: test clean
