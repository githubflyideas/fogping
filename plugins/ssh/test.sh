#!/usr/bin/env bash
# Tests for the SSH prober. Run: ./test.sh
# A fake SSH server on localhost stands in for the real thing (Python is only used
# here, in the test, to play the server).
set -u
cd "$(dirname "$0")"
fail=0

tmp=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null; rm -rf "$tmp"' EXIT

# serve NAME BANNER: start a fake server in the background and set $port to its
# port. BANNER "SILENT" means accept the connection and say nothing.
serve() {
  python3 - "$2" "$tmp/$1.port" <<'PY' &
import socket, sys, threading
banner, portfile = sys.argv[1].encode(), sys.argv[2]
srv = socket.socket(); srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", 0)); srv.listen(16)
open(portfile, "w").write(str(srv.getsockname()[1]))
def handle(c):
    try:
        if banner != b"SILENT":
            c.sendall(banner + b"\r\n")
        c.recv(1)  # hold the connection open until the client hangs up
    finally:
        c.close()
while True:
    c, _ = srv.accept()
    threading.Thread(target=handle, args=(c,), daemon=True).start()
PY
  while [[ ! -s $tmp/$1.port ]]; do sleep 0.05; done
  port=$(<"$tmp/$1.port")
}

run() { # run PORT OPTIONS_JSON [HOST]
  printf '{"protocol":"fogping-probe/1","type":"ssh","host":"%s","port":%s,"packets":3,"gap_ms":1,"timeout_ms":400,"options":%s}' \
    "${3-127.0.0.1}" "$1" "$2" | ./probe
}

check() { # check NAME JSON PYTHON_EXPRESSION_ON_d
  if ! python3 -c "import json,sys; d=json.loads(sys.argv[1]); sys.exit(0 if ($3) else 1)" "$2"; then
    echo "FAIL: $1: $2"; fail=1
  else
    echo "ok:   $1"
  fi
}

serve ok 'SSH-2.0-OpenSSH_9.6'; p1=$port
check "healthy server" "$(run "$p1" '{}')" 'd["sent"]==3 and d["received"]==3 and len(d["ms"])==3'
check "custom expected banner that matches" "$(run "$p1" '{"expect_banner":"SSH-2.0-OpenSSH"}')" 'd["received"]==3'
check "banner that does not match is loss" "$(run "$p1" '{"expect_banner":"SSH-1.99"}')" 'd["received"]==0 and d["ms"]==[]'

serve http 'HTTP/1.1 400 Bad Request'; p2=$port
check "not an SSH server is loss" "$(run "$p2" '{}')" 'd["received"]==0'

serve silent 'SILENT'; p3=$port
start=$SECONDS
check "silent server times out as loss" "$(run "$p3" '{}')" 'd["received"]==0'
((SECONDS - start < 5)) || { echo "FAIL: silent server took too long"; fail=1; }

check "nothing listening is loss, not an error" "$(run 1 '{}')" 'd["received"]==0 and "error" not in d'

# Hostile input must become an error answer and must NOT run anything.
marker=$(mktemp -u)
for host in "\$(touch $marker)" '`touch '"$marker"'`' 'a;touch '"$marker" 'a b' ''; do
  out=$(run 22 '{}' "$host")
  check "hostile host '$host'" "$out" '"error" in d and "sent" not in d'
done
check "hostile banner option" "$(run 22 '{"expect_banner":"x;touch /tmp/x"}')" '"error" in d'
[[ ! -e $marker ]] || { echo "FAIL: injected command ran"; fail=1; }
check "bad port" "$(run 99999 '{}')" '"error" in d'
check "wrong protocol" "$(echo '{"protocol":"x"}' | ./probe)" '"error" in d'
check "garbage" "$(echo 'garbage' | ./probe)" '"error" in d'

exit $fail
