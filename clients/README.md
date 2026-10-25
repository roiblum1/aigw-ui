# Usage in the coding agent

For the people who use the models: `aigw-token`, which signs Claude Code and
Oh My Pi in to the gateway with a personal key, and two small add-ons that
show a tenant's budget inside the coding agent and warn before it is spent;
the Oh My Pi one also adds the tenant's models.

## From the hub

Every file here is also in the hub's binary and served at
`<hub>/clients/` with no sign-in, so a machine with no internet installs
from the hub (since 0.33.0). The admin gives you a personal key with the
line to run, which `<hub>/my-usage` shows again once you sign in with the
key:

```sh
curl -fsSL https://aigw.example.com/clients/install.sh | sh -s -- https://aigw.example.com
```

On Windows, in PowerShell (see [Windows](#windows)):

```powershell
iex "& { $(irm https://aigw.example.com/clients/install.ps1) } https://aigw.example.com"
```

[`install/install.sh`](install/install.sh):

1. installs `aigw-token` in `~/.local/bin` and signs it in (it asks for the
   personal key on the terminal, or reads `AIGW_PERSONAL_KEY`);
2. asks the hub for the gateway's address and your models (`GET
   /api/v1/my/models`, 0.34.0). An address given after the hub's is used
   instead, without a trailing `/anthropic` or `/v1`;
3. with Claude Code installed (`claude`, or `~/.claude`), sets in
   `~/.claude/settings.json`, with `node`: `apiKeyHelper`,
   `ANTHROPIC_BASE_URL` (the gateway with `/anthropic`), the models
   (`ANTHROPIC_MODEL`, `ANTHROPIC_DEFAULT_OPUS_MODEL`, `_SONNET_`,
   `_FABLE_`, `_HAIKU_` and `ANTHROPIC_SMALL_FAST_MODEL`; one you set that
   you may call is kept), `CLAUDE_CODE_MAX_CONTEXT_TOKENS` and
   `CLAUDE_CODE_MAX_OUTPUT_TOKENS` when the hub knows the model's length,
   and the status line. Everything else is kept; the old file is kept as
   `settings.json.aigw-backup`. Then it installs the `aigw-usage` plugin
   from `<hub>/clients/claude-code.tar.gz` with `claude plugin marketplace
   add` and `claude plugin install`;
4. with Oh My Pi installed (`omp`, or `~/.omp/agent`), puts the extension in
   `~/.omp/agent/extensions/aigw-usage.ts`, and sets omp's model roles
   (`omp config set modelRoles`): `default` to the hub's model for Claude
   Code and `smol`, omp's quick tasks, to its small one. A role you set to
   another provider is kept; one on a gateway model you may no longer call
   is replaced;
5. checks that the gateway takes the token (`GET /v1/models`), waiting up to
   60 s (`AIGW_CHECK_SECONDS`) while the gateways list a new token.

Without `node`, `claude` or `omp` it says what to do by hand. Run it again
after a hub upgrade, or when your models change, to get the new files and
settings; it keeps the sign-in.

**It keeps itself up to date** (0.36.1). The installer keeps the version
of the client files it installed in `~/.config/aigw/clients`. The hub names
its own with every token (`clients_version`), and when they differ, or a
week after the last setup, `aigw-token` runs the installer again from the
hub, in the background and with nothing shown: no key is asked for, a
gateway address given by hand before is kept, the gateway's check is
skipped, and the output goes to `~/.config/aigw/update.log`. A try that did
not finish is made again a day later. A hub upgrade reaches a machine within
about 10 minutes of its next use of Claude Code or omp; one that changes no
client file moves nobody. `AIGW_AUTO_UPDATE=0` turns it off;
`AIGW_UPDATE_DAYS` changes the week. A machine set up by hand, without the
installer, is left alone.

In Git Bash, MSYS2 or Cygwin it stops and gives the PowerShell line. In WSL
it sets up the tools installed in WSL and leaves out Windows' own programs,
which WSL puts on the `PATH` under `/mnt/<drive>/` (`AIGW_WINDOWS_DRIVES`
if wsl.conf moves them): it names them and gives the PowerShell line.

### Windows

Claude Code on Windows runs its `apiKeyHelper` with `cmd.exe`, and Oh My Pi
runs commands with its own shell, so neither can run the `sh` `aigw-token`.
[`install/install.ps1`](install/install.ps1) does the same steps from
PowerShell (Windows PowerShell 5.1 or PowerShell 7), from the hub alone:

1. puts [`aigw-token/aigw-token.ps1`](aigw-token/aigw-token.ps1) in
   `%USERPROFILE%\.local\share\aigw` and `aigw-token.cmd` in
   `%USERPROFILE%\.local\bin`, and signs in (the key is asked for hidden,
   or read from `AIGW_PERSONAL_KEY`). The `.ps1` keeps the same files as the
   `sh` one in `%USERPROFILE%\.config\aigw`;
2. writes Claude Code's settings as `install.sh` does, without `node`, with
   `apiKeyHelper` set to `powershell.exe -NoLogo -NoProfile -ExecutionPolicy
   Bypass -File "<the .ps1>"`. The plugin and the status line (`node
   "<home>/.claude/aigw-usage/statusline.mjs"`) need `node`, and are left
   out without it;
3. sets up Oh My Pi as `install.sh` does; the extension runs the same
   `powershell.exe` line for a token on Windows;
4. checks that the gateway takes the token.

It runs inside your PowerShell through `iex`, so the execution policy does
not stop it. Claude Code and omp run `aigw-token.ps1` with `-ExecutionPolicy
Bypass`, which a policy set by Group Policy overrides: then ask IT to allow
it.

| `<hub>/clients/` | Is |
|---|---|
| `install.sh` | The installer above |
| `install.ps1` | The installer for Windows, in PowerShell |
| `aigw-token` | [`aigw-token/aigw-token`](aigw-token/aigw-token) |
| `aigw-token.ps1` | [`aigw-token/aigw-token.ps1`](aigw-token/aigw-token.ps1), the same for Windows |
| `claude-code.tar.gz` | [`claude-code/`](claude-code) as the marketplace `aigw` |
| `aigw-usage.ts` | [`omp/aigw-usage.ts`](omp/aigw-usage.ts) |

## aigw-token

[`aigw-token/aigw-token`](aigw-token/aigw-token) is Claude Code's
`apiKeyHelper` for the gateway. It trades your personal key at the hub for a
token that carries your team's budget and models as they are now, and gets a
new one every 10 minutes (`AIGW_TOKEN_RENEW_SECONDS`), so you never handle a
token and a change of budget reaches you by itself. If the hub cannot be
reached it goes on printing the token it has until that expires (7 days by
default). Needs `sh` and `curl` (macOS, Linux, WSL); on Windows, the
PowerShell `aigw-token.ps1` that `install.ps1` puts in place does the same.

```sh
curl -fsSLo ~/.local/bin/aigw-token \
  https://raw.githubusercontent.com/roiblum1/aigw-ui/main/clients/aigw-token/aigw-token
chmod +x ~/.local/bin/aigw-token
aigw-token login https://aigw.example.com   # paste your personal key when asked
```

Then in `~/.claude/settings.json`:

```json
{
  "apiKeyHelper": "aigw-token",
  "env": { "ANTHROPIC_BASE_URL": "https://<the gateway>" }
}
```

`aigw-token logout` forgets the key. Prompts go to the gateway; the hub is
only asked for tokens.

| | For | Shows |
|---|---|---|
| [`claude-code/`](claude-code) | Claude Code | The budget in the status line; a message when a budget is nearly spent, spent, or served as best-effort |
| [`omp/aigw-usage.ts`](omp/aigw-usage.ts) | Oh My Pi (`omp`) | The same in the status area, and `/aigw-usage` for all budgets |

Both show the budget across models as **All models** next to the budgets
per model, and the one of them furthest gone in the status line.

```
GLM5.3 $47.00 of $50.00 (94%) · resets 18:00
```

Both ask the hub's `GET /api/v1/my/usage` with the tenant's own API key, the
same call the page `/my-usage` makes. They need `config.tenantPage` on, which
is the default. A model with prices is shown in dollars, any other in
tokens. No prompt and no answer is sent to the hub.

## What you need

- The address of the hub, for example `https://aigw.example.com`. It is not
  the gateway's address.
- Your key: the personal key `aigw-token` signed in with is found by itself,
  else a token or API key for the gateway.

## Claude Code

Needs `node` 18 or later on the machine.

```
/plugin marketplace add roiblum1/aigw-ui
/plugin install aigw-usage@aigw
```

Claude Code asks for three options when the plugin is enabled:

| Option | Empty means |
|---|---|
| Hub address | The environment variable `AIGW_HUB_URL` |
| API key | `AIGW_API_KEY`, then the key Claude Code already sends to the gateway (`ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY`) |
| Warn at, percent | 90 |

Restart Claude Code once, then run `/aigw-usage:setup`. It adds the status
line to your `~/.claude/settings.json`. A plugin cannot do that by itself,
and the command asks before it replaces a status line you already have.

How it works:

- A hook asks the hub when a session starts, when you send a prompt and
  when an answer ends: at most every 30 seconds, and 5 seconds after an
  answer. It keeps the answer in `~/.claude/aigw-usage/usage.json`.
- The status line only reads that file. It has no key and makes no request.
- The key is never written to a file.
- A warning is shown once per budget, level and period: at the warning
  percent, when the budget is spent, when you are served as best-effort,
  and when you are refused.
- A hub that is down or a wrong key is asked again after one and five
  minutes, not on every prompt. The status line goes on showing the last
  figures, marked `old`.

## Oh My Pi

The extension [`omp/aigw-usage.ts`](omp/aigw-usage.ts) adds your models to
omp: when omp starts it asks the hub for them (`GET /api/v1/my/models`,
with the personal key `aigw-token` keeps) and registers them as the
provider `aigw` on the gateway's `/v1`, with their lengths and prices, and
`!aigw-token` as the command for a token. `omp models aigw` lists them; `omp
--model aigw/<model>` uses one. Nothing is written to
`~/.omp/agent/models.yml`. If the hub cannot be reached, it adds the list
it kept last (`~/.config/aigw/models.json`). The installer puts it in
place; by hand:

```sh
mkdir -p ~/.omp/agent/extensions
curl -fsSLo ~/.omp/agent/extensions/aigw-usage.ts https://<the hub>/clients/aigw-usage.ts
```

A model's length is what its servers report (vLLM's `max_model_len`), or
the `aigw-ui.io/max-model-len` of its deployments, or else the market's
figure for it; the longest answer is a quarter of it, at
most 16,384 tokens. Without either, omp takes 128,000 and 16,384.

A provider `aigw` of your own in `models.yml` is left as it is, and the
extension adds no model; omp says so when a session starts. To set a model
by hand, for example with `reasoning: true` for omp's thinking levels, name
your provider otherwise:

```yaml
providers:
  gateway:
    baseUrl: https://<the gateway>/v1
    api: openai-completions
    apiKey: "!aigw-token"          # the full path if it is not on omp's PATH
    models:
      - id: GLM5.3                 # the name the gateway routes on
        reasoning: true            # offers omp's thinking levels
        contextWindow: 200000
        maxTokens: 16384
  gateway-anthropic:               # the gateway's Anthropic path, if wanted
    baseUrl: https://<the gateway>/anthropic
    api: anthropic-messages
    apiKey: "!aigw-token"
    models:
      - id: GLM5.3
        reasoning: true
```

`omp` runs `aigw-token` once per session and keeps the token, and runs it
again after a 401, so a change of budget, quota or models reaches you in
your next session (in Claude Code, within minutes).

The extension also shows the budgets in the status area, and `/aigw-usage`
lists them all. With `aigw-token` signed in there is nothing to set.
Without it, set `AIGW_HUB_URL` and `AIGW_API_KEY`. `AIGW_WARN_AT` changes
the warning percent.

**Thinking.** With `reasoning: true`, omp's thinking level goes to the model
as `reasoning_effort` on `/v1` (`--thinking off` sends `minimal`), and as
Anthropic's `thinking` on the Anthropic path. The gateway passes both on to
the model server unchanged; on the Anthropic path it does not turn
`thinking` into `reasoning_effort`, so whether the model thinks is up to the
model server (vLLM accepts both). The model's `reasoning_content` comes back
as thinking on both paths, and reasoning tokens are charged as output.

## Tested

- Claude Code: the scripts, against a stand-in for the hub: the status
  line, each warning once, a hub that is down, a wrong key, no address. The
  manifests pass `claude plugin validate`. Not yet run inside a Claude Code
  session against a real hub.
- Oh My Pi 18.11.1 against the sandbox hub and the lab gateway
  (2026-10-16): models from `models.yml` with `!aigw-token`, a real model
  answered and a spent budget refused (429, with omp's retry time); the
  extension inside `omp`'s terminal UI showed the budget across models in
  the status area, the spent warning, and `/aigw-usage`, signed in through
  `aigw-token` with nothing set. Thinking with a stand-in model server that
  answers as vLLM's reasoning parser does: `reasoning_effort` and
  `thinking` reached it, and its reasoning came back as omp's thoughts on
  both paths.

- aigw-token: against a stand-in for the hub: sign-in, a new token, the
  token kept, a hub that fails, a revoked key, no hub.
- install.sh: against a stand-in for the hub and for `claude` and `omp`:
  everything installed and the settings merged with the hub's models and
  length, a model the person may call kept, the wait while the gateway
  refuses the token, a second run that keeps the sign-in, takes an address
  ending in `/anthropic` and updates the marketplace, a hub without
  `/my/models`, no `node`, a wrong key. On 2026-10-18 against a local
  0.33.0 hub with Claude Code 2.1.292 in an empty config directory: the
  plugin installed and enabled from the hub's archive, `aigw-token` signed
  in and printing a token. On 2026-10-21 against a local 0.34.0 hub and a
  stand-in gateway, piped from the hub into an empty home: Claude Code
  2.1.292 asked the gateway's `/anthropic/v1/messages` for the hub's model
  with the token and printed the answer; omp 18.11.2 listed the hub's
  models under `aigw` and `omp -p --model aigw/big` streamed from `/v1`
  with the token.
- The Oh My Pi extension's models: `node --test clients/omp/`, against a
  stand-in hub (registered, the kept list with the hub down, a person's own
  provider `aigw`), and its token command on Windows.
- Updates (2026-10-25): the `sh` and PowerShell `aigw-token` against a
  stand-in hub that names a version: the installer again for another
  version and for a week-old setup, not again for a try that did not
  finish, not for a machine set up by hand, nor with `AIGW_AUTO_UPDATE=0`;
  the `sh` one prints its token while the installer is still running. Both
  installers in update mode (no key asked, the gateway given by hand kept,
  no gateway check), and the whole round with the real files: a hub
  upgraded to other files, then a fix the same day, each taken through
  `aigw-token`. Not run on Windows: there `Start-Process -WindowStyle
  Hidden` is meant to keep the update off Claude Code's handles.
- install.ps1 and aigw-token.ps1 (2026-10-25): under PowerShell 7.6.9 on
  Linux (`install-ps1.test.mjs`, `aigw-token-ps1.test.mjs`, which need
  `pwsh`), against the same stand-ins as `install.sh`, including the line
  `/my-usage` shows run through `iex` from the hub, and the `sh` and
  PowerShell `aigw-token` taking each other's sign-in. Not run on Windows
  or under Windows PowerShell 5.1. install.sh in Git Bash and in WSL: with a
  stand-in `uname` and stand-in Windows programs under a stand-in `/mnt/c`.

```sh
node --test clients/claude-code/scripts/ clients/aigw-token/ clients/install/ clients/omp/
```
