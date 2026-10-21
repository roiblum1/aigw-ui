#!/bin/sh
# Sets up a person's machine for the AI gateway, from the hub alone, so it
# works in a network with no internet: aigw-token, signed in with the
# person's personal key; Claude Code, with the gateway's address, the
# models to ask for and the aigw-usage plugin; and Oh My Pi, with the
# extension that adds the person's models and shows the budget. The
# tenant's page (/my-usage) shows the line to run:
#
#   curl -fsSL HUB/clients/install.sh | sh -s -- HUB
#
# HUB is the hub's address. The hub gives the gateway's address and the
# models; an address given after HUB is used instead of the hub's. The
# personal key is asked for on the terminal, or taken from
# AIGW_PERSONAL_KEY; it is never put on a command line. Claude Code and
# Oh My Pi are each set up when they are installed, and skipped with what
# to do otherwise; the settings need node. Last, it checks that the gateway
# takes the token. Needs sh and curl.
set -eu

fail() {
	echo "aigw install: $*" >&2
	exit 1
}
say() { echo "aigw install: $*" >&2; }

[ $# -ge 1 ] || fail "usage: install.sh HUB_URL [GATEWAY_URL]"
hub=${1%/}
bin=${AIGW_BIN_DIR:-$HOME/.local/bin}
claude_dir=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
omp_dir=$HOME/.omp/agent
command -v curl >/dev/null || fail "curl is needed"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

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

# 3. What the hub sets clients up with: the gateway's address and the
# person's models (GET /api/v1/my/models, with the personal key). A hub
# before 0.34.0 has none of it.
setup=$tmp/setup.json
printf 'Authorization: Bearer %s\n' "$(cat "$config/personal-key")" |
	curl -fsS --max-time 30 -H @- -o "$setup" "$hub/api/v1/my/models" 2>/dev/null || rm -f "$setup"
field() { [ -r "$setup" ] && sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" "$setup" || true; }
gateway=${2:-$(field gateway_url)}
gateway=${gateway%/}
# An address copied from a client's settings ends with its API's path.
gateway=${gateway%/anthropic}
gateway=${gateway%/v1}
[ -n "$gateway" ] || say "the hub is not told the gateway's address (config.gatewayURL): ask its operator, or give it after the hub's"

# 4. Claude Code: aigw-token as the API key helper, the gateway's Anthropic
# API as the address, the hub's models where the person has none of their
# own, and the plugin's status line when there is none. Everything else in
# the settings is kept, and the old file beside them.
if command -v claude >/dev/null || [ -d "$claude_dir" ]; then
	settings="$claude_dir/settings.json"
	if command -v node >/dev/null; then
		mkdir -p "$claude_dir"
		if model=$(AIGW_SETTINGS="$settings" AIGW_HELPER="$bin/aigw-token" AIGW_GATEWAY="$gateway" AIGW_SETUP="$setup" node -e '
const fs = require("fs");
const file = process.env.AIGW_SETTINGS;
let s = {};
if (fs.existsSync(file)) {
  try { s = JSON.parse(fs.readFileSync(file, "utf8")); } catch { console.error("aigw install: " + file + " is not JSON; left as it is"); process.exit(3); }
  fs.copyFileSync(file, file + ".aigw-backup");
}
let setup = {};
try { setup = JSON.parse(fs.readFileSync(process.env.AIGW_SETUP, "utf8")); } catch {}
s.apiKeyHelper = process.env.AIGW_HELPER;
if (process.env.AIGW_GATEWAY) s.env = { ...(s.env || {}), ANTHROPIC_BASE_URL: process.env.AIGW_GATEWAY + "/anthropic" };
// Claude Code asks for Claude models unless told otherwise. A model the
// person set that they may call is kept; any other is replaced.
const mine = new Set((setup.models || []).map((m) => m.name));
const cc = setup.claude_code;
if (cc) {
  const want = {
    ANTHROPIC_MODEL: cc.model,
    ANTHROPIC_DEFAULT_OPUS_MODEL: cc.model,
    ANTHROPIC_DEFAULT_SONNET_MODEL: cc.model,
    ANTHROPIC_DEFAULT_FABLE_MODEL: cc.model,
    ANTHROPIC_DEFAULT_HAIKU_MODEL: cc.small_model,
    ANTHROPIC_SMALL_FAST_MODEL: cc.small_model,
  };
  for (const [k, v] of Object.entries(want)) if (!mine.has((s.env || {})[k])) s.env = { ...(s.env || {}), [k]: v };
}
// Claude Code takes a model it does not know for 200,000 tokens long and
// asks for answers of up to 32,000, which a shorter model refuses: the
// length of the model it works with, when the hub knows it, and a quarter
// of it for the answer.
const used = (setup.models || []).find((m) => m.name === (s.env || {}).ANTHROPIC_MODEL);
if (used && used.context_window > 0) {
  s.env.CLAUDE_CODE_MAX_CONTEXT_TOKENS = String(used.context_window);
  s.env.CLAUDE_CODE_MAX_OUTPUT_TOKENS = String(Math.min(32000, Math.floor(used.context_window / 4)));
}
if (!s.statusLine) s.statusLine = { type: "command", command: "node ~/.claude/aigw-usage/statusline.mjs", refreshInterval: 30 };
fs.writeFileSync(file, JSON.stringify(s, null, 2) + "\n");
console.log((s.env || {}).ANTHROPIC_MODEL || "");
'); then
			say "Claude Code settings written to $settings${model:+, model $model}"
			[ -n "$model" ] || say "the hub gave no model for Claude Code: set env.ANTHROPIC_MODEL in $settings to one of your models"
		fi
	else
		say "node is not installed: put this in $settings yourself:"
		shown=$gateway
		[ -n "$shown" ] || shown="<the gateway's address>"
		cat >&2 <<EOF
{
  "apiKeyHelper": "$bin/aigw-token",
  "env": {
    "ANTHROPIC_BASE_URL": "$shown/anthropic",
    "ANTHROPIC_MODEL": "$(field model)",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "$(field small_model)"
  }
}
EOF
	fi

	# The aigw-usage plugin, from a copy of its marketplace the hub serves.
	if command -v claude >/dev/null; then
		market="$claude_dir/aigw-marketplace"
		rm -rf "$market.tmp" && mkdir -p "$market.tmp"
		curl -fsS --max-time 60 "$hub/clients/claude-code.tar.gz" | tar -xzf - -C "$market.tmp" || fail "could not download the plugin from $hub"
		rm -rf "$market" && mv "$market.tmp" "$market"
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
		say "claude is not on the PATH: the plugin is skipped; run this again once it is"
	fi
else
	say "Claude Code is not installed: skipped; run this again once it is"
fi

# 5. Oh My Pi: the extension adds the person's models from the hub as the
# provider aigw, with aigw-token for tokens, and shows the budget.
if command -v omp >/dev/null || [ -d "$omp_dir" ]; then
	mkdir -p "$omp_dir/extensions"
	curl -fsS --max-time 30 -o "$omp_dir/extensions/aigw-usage.ts.tmp" "$hub/clients/aigw-usage.ts" || fail "could not download the Oh My Pi extension from $hub"
	mv "$omp_dir/extensions/aigw-usage.ts.tmp" "$omp_dir/extensions/aigw-usage.ts"
	say "the Oh My Pi extension is in $omp_dir/extensions: omp --model aigw/<model> (omp models aigw lists them)"
	# Its roles: the hub's model as the default, and its small model as
	# smol, for omp's quick tasks. A role the person set to another
	# provider is theirs; one on a gateway model they may no longer call
	# is replaced.
	if command -v omp >/dev/null && command -v node >/dev/null; then
		roles=$(omp config get modelRoles --json 2>/dev/null) || roles='{}'
		if new=$(AIGW_SETUP="$setup" AIGW_ROLES="$roles" node -e '
const fs = require("fs");
let setup = {};
try { setup = JSON.parse(fs.readFileSync(process.env.AIGW_SETUP, "utf8")); } catch {}
const cc = setup.claude_code;
if (!cc) process.exit(0);
let roles = {};
try { roles = JSON.parse(process.env.AIGW_ROLES).value || {}; } catch {}
const mine = new Set((setup.models || []).map((m) => "aigw/" + m.name));
const want = { default: "aigw/" + cc.model, smol: "aigw/" + cc.small_model };
let changed = false;
for (const [role, model] of Object.entries(want)) {
  // A role may carry a thinking level after a colon.
  const now = (roles[role] || "").replace(/:[^/]*$/, "");
  if (!now || (now.startsWith("aigw/") && !mine.has(now))) {
    roles[role] = model;
    changed = true;
  }
}
if (changed) console.log(JSON.stringify(roles));
') && [ -n "$new" ]; then
			omp config set modelRoles "$new" >/dev/null && say "Oh My Pi's model roles: $new"
		fi
	fi
	if grep -Eq '^[[:space:]]+aigw:[[:space:]]*(#.*)?$' "$omp_dir/models.yml" 2>/dev/null; then
		say "$omp_dir/models.yml defines the provider aigw itself, so the hub's models are not added: remove it there to get them"
	fi
else
	say "Oh My Pi is not installed: skipped; run this again once it is"
fi

# 6. The gateway takes the token. While it takes API keys too, it takes a
# personal key's token once a sync has listed it, a few seconds after the
# first sign-in.
if [ -n "$gateway" ]; then
	wait=${AIGW_CHECK_SECONDS:-60}
	start=$(date +%s)
	while :; do
		code=$(printf 'Authorization: Bearer %s\n' "$("$bin/aigw-token")" |
			curl -sS --max-time 10 -H @- -o /dev/null -w '%{http_code}' "$gateway/v1/models" 2>/dev/null) || code=000
		[ "$code" = 401 ] && [ $(($(date +%s) - start)) -lt "$wait" ] || break
		[ "${waited:-}" ] || say "waiting for the gateway to take your token"
		waited=1
		sleep 2
	done
	case $code in
	200) say "the gateway at $gateway takes your token" ;;
	401) say "the gateway at $gateway still refuses your token (401): tell the hub's operator" ;;
	000) say "the gateway at $gateway cannot be reached from here" ;;
	*) say "the gateway at $gateway answered HTTP $code" ;;
	esac
fi
say "done"
