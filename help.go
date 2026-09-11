package main

import (
	"fmt"
	"io"
	"strings"
)

// helpText is written so that someone who never opens the README can copy a line
// and have fogping running correctly. Keep every command runnable as printed.
const helpText = `fogping {{VERSION}} — SmokePing-style latency and packet-loss graphs.
One binary with a built-in database and web UI. No config file.

Start it, then open http://<this-server-ip>:8518 in a browser.
Data is kept in ./data under the directory you start it from:
always start it from the same directory.

EXAMPLES

  First try (web UI open to anyone who can reach port 8518):
    ./fogping

  With a login (recommended):
    ./fogping user=admin passwd=change-me

  Several users (names and passwords are paired by position):
    ./fogping user=alice,bob passwd=alice-pw,bob-pw

  Add, edit or delete targets: start with --edit, open the web UI, click
  "✎ targets". Changes apply at once. When done, press Ctrl-C and start again
  without --edit, so the UI is read-only:
    ./fogping --edit user=admin passwd=change-me

  Keep 90 days of history instead of 40:
    ./fogping --days=90 user=admin passwd=change-me

  Run in the background with a log file, and stop it again:
    nohup ./fogping user=admin passwd=change-me >> fogping.log 2>&1 &
    pkill -x fogping

  Listen on 127.0.0.1 only, when Nginx/Caddy in front of it handles access:
    ./fogping --localhost
    ./fogping --localhost --edit

  Upgrade: stop it, replace the fogping file, start it again. ./data is kept.

PING TARGETS SHOW 100% LOSS?

  This host does not allow ICMP for normal users. Fix it with one of:

    # until the next reboot:
    sudo sysctl -w net.ipv4.ping_group_range="0 2147483647"

    # permanently:
    echo 'net.ipv4.ping_group_range = 0 2147483647' | sudo tee /etc/sysctl.d/99-fogping.conf
    sudo sysctl --system

    # or give this binary the permission (redo after each upgrade):
    sudo setcap cap_net_raw+ep ./fogping

  TCP targets need nothing.

FLAGS

  --edit          allow changing targets in the web UI
                  (refuses to start without user=/passwd= or --localhost)
  --localhost     listen on 127.0.0.1:8518 instead of all interfaces (0.0.0.0:8518)
  --days=N        days of history to keep (default 40)
  --version       print the version and exit
  user=  passwd=  turn on the login page (comma-separated lists)

  Flags and user=/passwd= can be given in any order. The port is always 8518.

Full guide, systemd unit, reverse proxy: https://github.com/githubflyideas/fogping
`

func printHelp(w io.Writer) {
	fmt.Fprint(w, strings.Replace(helpText, "{{VERSION}}", version, 1))
}
