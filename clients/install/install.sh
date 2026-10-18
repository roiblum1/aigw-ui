#!/bin/sh
# Sets up a person's machine for the AI gateway, from the hub alone, so it
# works in a network with no internet: aigw-token, signed in with the
# person's personal key; Claude Code's settings; and the aigw-usage plugin.
# The tenant's page (/my-usage) shows the line to run:
#
#   curl -fsSL HUB/clients/install.sh | sh -s -- HUB [GATEWAY]
#
# HUB is the hub's address, GATEWAY the gateway's, which Claude Code sends
# its requests to. The personal key is asked for on the terminal, or taken
# from AIGW_PERSONAL_KEY; it is never put on a command line. Each part is
# skipped, with what to do by hand, when its tool is missing: node for the
# settings, claude for the plugin. Needs sh and curl.
set -eu

fail() {
	echo "aigw install: $*" >&2
	exit 1
}
say() { echo "aigw install: $*" >&2; }

[ $# -ge 1 ] || fail "usage: install.sh HUB_URL [GATEWAY_URL]"
hub=${1%/}
gateway=${2:-}
gateway=${gateway%/}
bin=${AIGW_BIN_DIR:-$HOME/.local/bin}
claude_dir=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
command -v curl >/dev/null || fail "curl is needed"

# 1. aigw-token, from the hub.
mkdir -p "$bin"
curl -fsS --max-time 30 -o "$bin/aigw-token.tmp" "$hub/clients/aigw-token" || fail "could not download aigw-token from $hub"
chmod 755 "$bin/aigw-token.tmp"
mv "$bin/aigw-token.tmp" "$bin/aigw-token"
say "aigw-token is in $bin"
case ":$PATH:" in *":$bin:"*) ;; *) say "add $bin to your PATH to run aigw-token yourself" ;; esac

# 2. Signed in. Already signed in to this hub with a key it takes: kept.
config=${AIGW_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/aigw}
if [ "$(cat "$config/hub" 2>/dev/null)" = "$hub" ] && AIGW_TOKEN_RENEW_SECONDS=0 "$bin/aigw-token" >/dev/null 2>&1; then
	say "already signed in to $hub (aigw-token logout, then run this again, to change the key)"
elif [ -n "${AIGW_PERSONAL_KEY:-}" ]; then
	printf '%s\n' "$AIGW_PERSONAL_KEY" | "$bin/aigw-token" login "$hub"
elif [ -r /dev/tty ]; then
	# The script itself comes in on stdin: the key is read from the terminal.
	"$bin/aigw-token" login "$hub" </dev/tty
else
	fail "no terminal to ask for the personal key on: set AIGW_PERSONAL_KEY, or run $bin/aigw-token login $hub"
fi

# 3. Claude Code's settings: aigw-token as the API key helper, the gateway as
# the address, and the plugin's status line when there is none. Everything
# else in the file is kept, and the old file is kept beside it.
settings="$claude_dir/settings.json"
if command -v node >/dev/null; then
	mkdir -p "$claude_dir"
	AIGW_SETTINGS="$settings" AIGW_HELPER="$bin/aigw-token" AIGW_GATEWAY="$gateway" node -e '
const fs = require("fs");
const file = process.env.AIGW_SETTINGS;
let s = {};
if (fs.existsSync(file)) {
  try { s = JSON.parse(fs.readFileSync(file, "utf8")); } catch { console.error("aigw install: " + file + " is not JSON; left as it is"); process.exit(3); }
  fs.copyFileSync(file, file + ".aigw-backup");
}
s.apiKeyHelper = process.env.AIGW_HELPER;
if (process.env.AIGW_GATEWAY) s.env = { ...(s.env || {}), ANTHROPIC_BASE_URL: process.env.AIGW_GATEWAY };
if (!s.statusLine) s.statusLine = { type: "command", command: "node ~/.claude/aigw-usage/statusline.mjs", refreshInterval: 30 };
fs.writeFileSync(file, JSON.stringify(s, null, 2) + "\n");
' && say "Claude Code settings written to $settings"
	[ -n "$gateway" ] || say "no gateway address given: set env.ANTHROPIC_BASE_URL in $settings to the gateway's address"
else
	say "node is not installed: put this in $settings yourself:"
	shown=$gateway
	[ -n "$shown" ] || shown="<the gateway's address>"
	cat >&2 <<EOF
{
  "apiKeyHelper": "$bin/aigw-token",
  "env": { "ANTHROPIC_BASE_URL": "$shown" }
}
EOF
fi

# 4. The aigw-usage plugin, from a copy of its marketplace the hub serves.
if command -v claude >/dev/null; then
	market="$claude_dir/aigw-marketplace"
	tmp="$market.tmp"
	rm -rf "$tmp" && mkdir -p "$tmp"
	curl -fsS --max-time 60 "$hub/clients/claude-code.tar.gz" | tar -xzf - -C "$tmp" || fail "could not download the plugin from $hub"
	rm -rf "$market" && mv "$tmp" "$market"
	if claude plugin marketplace list 2>/dev/null | grep -qw aigw; then
		claude plugin marketplace update aigw >/dev/null
	else
		claude plugin marketplace add "$market" >/dev/null
	fi
	# The plugin finds the key aigw-token keeps; the hub is given so that
	# Claude Code does not ask for it. The plugin loads from the unpacked
	# folder, which stays.
	claude plugin install aigw-usage@aigw --config "hub_url=$hub" --config warn_at=90 >/dev/null
	say "the aigw-usage plugin is installed: restart Claude Code to see your budget in the status line"
else
	say "claude is not installed: the plugin is skipped; run this again once it is"
fi
say "done"
