# Sets up a Windows machine for the AI gateway, from the hub alone, as
# install.sh does on macOS, Linux and WSL: aigw-token, signed in with the
# person's personal key; Claude Code, with the gateway's address, the models
# to ask for and the aigw-usage plugin; and Oh My Pi, with the extension that
# adds the person's models and shows the budget. The tenant's page
# (/my-usage) shows the line to run in PowerShell:
#
#   iex "& { $(irm HUB/clients/install.ps1) } HUB"
#
# HUB is the hub's address. The hub gives the gateway's address and the
# models; an address given after HUB is used instead of the hub's. The
# personal key is asked for, or taken from AIGW_PERSONAL_KEY; it is never put
# on a command line. Claude Code and Oh My Pi are each set up when they are
# installed, and skipped with what to do otherwise. Last, it checks that the
# gateway takes the token.
#
# Claude Code on Windows runs its apiKeyHelper with cmd.exe and Oh My Pi runs
# commands with its own shell, so neither can run the sh aigw-token: this
# puts aigw-token.ps1 in %USERPROFILE%\.local\share\aigw (off the PATH, where
# PowerShell would find it before aigw-token.cmd and refuse it under the
# default execution policy), aigw-token.cmd in %USERPROFILE%\.local\bin for
# people, and gives Claude Code and omp "powershell.exe -File" for the .ps1.
#
# It keeps the version of the client files it installed, and aigw-token.ps1
# runs it again by itself, hidden, with AIGW_UPDATE=1, when the hub serves
# others or a week later: then it asks for nothing, keeps a gateway address
# given before, and skips the gateway's check.
#
# Windows PowerShell 5.1 and PowerShell 7 both run this file: keep it ASCII
# (5.1 reads a file without a byte order mark in the system's code page). It
# runs inside the person's PowerShell through iex, so it never calls exit,
# which would close their window.
param([string]$Hub, [string]$Gateway)

# The person's session may have strict mode on; this script reads optional
# fields of JSON as $null.
Set-StrictMode -Off
$ErrorActionPreference = "Continue"
$ProgressPreference = "SilentlyContinue"
try { [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072 } catch {}

function Say([string]$Message) { [Console]::Error.WriteLine("aigw install: $Message") }
function Fail([string]$Message) { throw "aigw install: $Message" }

# A program on the PATH, never a .ps1: npm puts claude.ps1 and omp.ps1 next
# to claude.cmd and omp.cmd, and PowerShell refuses a .ps1 under the default
# execution policy.
function Find-Program([string]$Name) {
	$found = Get-Command $Name -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
	if ($found) { return $found.Source }
	return $null
}

# Windows PowerShell 5.1, and 7 before 7.3, pass a program the double quotes
# of an argument unescaped, so omp would get JSON without them; so do 7.3 and
# later for a .cmd, such as the one npm makes for omp.
function Native-Argument([string]$Text, [string]$Program) {
	$legacy = $PSVersionTable.PSVersion -lt [version]"7.3"
	if (-not $legacy) {
		$mode = Get-Variable -Name PSNativeCommandArgumentPassing -ValueOnly -ErrorAction SilentlyContinue
		$legacy = ($mode -eq "Legacy") -or ($mode -eq "Windows" -and $Program -match "\.(cmd|bat)$")
	}
	if ($legacy) { return $Text -replace '"', '\"' }
	return $Text
}

function Set-Property($Object, [string]$Name, $Value) {
	$Object | Add-Member -NotePropertyName $Name -NotePropertyValue $Value -Force
}

function Get-HubFile([string]$Name, [string]$Destination) {
	try {
		Invoke-WebRequest -UseBasicParsing -TimeoutSec 60 -Uri "$Hub/clients/$Name" -OutFile "$Destination.tmp" -ErrorAction Stop
	} catch {
		Fail "could not download $Name from ${Hub}: $($_.Exception.Message)"
	}
	Move-Item -Force -LiteralPath "$Destination.tmp" -Destination $Destination
}

function Install-Aigw {
	if (-not $Hub) { Fail 'usage: iex "& { $(irm HUB/clients/install.ps1) } HUB [GATEWAY]"' }
	if ($PSVersionTable.PSVersion -lt [version]"5.1") { Fail "PowerShell 5.1 or later is needed" }
	$Hub = $Hub -replace "/$", ""
	$update = $env:AIGW_UPDATE -eq "1"
	$home_ = $HOME
	$bin = $env:AIGW_BIN_DIR
	if (-not $bin) { $bin = [IO.Path]::Combine($home_, ".local", "bin") }
	$share = [IO.Path]::Combine($home_, ".local", "share", "aigw")
	$claudeDir = $env:CLAUDE_CONFIG_DIR
	if (-not $claudeDir) { $claudeDir = Join-Path $home_ ".claude" }
	$ompDir = [IO.Path]::Combine($home_, ".omp", "agent")
	$base = $env:XDG_CONFIG_HOME
	if (-not $base) { $base = Join-Path $home_ ".config" }
	$config = $env:AIGW_CONFIG_DIR
	if (-not $config) { $config = Join-Path $base "aigw" }
	$tmp = Join-Path ([IO.Path]::GetTempPath()) ("aigw-" + [Guid]::NewGuid().ToString("N"))
	New-Item -ItemType Directory -Force -Path $bin, $share, $tmp | Out-Null
	# aigw-token, run below, must not start an update of its own meanwhile.
	# This runs in the person's session: their variable is put back after.
	$autoUpdate = $env:AIGW_AUTO_UPDATE
	$env:AIGW_AUTO_UPDATE = "0"

	try {
		# 1. aigw-token, from the hub. The PowerShell this runs in runs it
		# here; Claude Code and omp get Windows PowerShell, which every
		# Windows has.
		$ps1 = Join-Path $share "aigw-token.ps1"
		Get-HubFile "aigw-token.ps1" $ps1
		$cmd = Join-Path $bin "aigw-token.cmd"
		[IO.File]::WriteAllText($cmd, "@echo off`r`npowershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File `"$ps1`" %*`r`n")
		$helper = "powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File `"$($ps1 -replace '\\', '/')`""
		$self = [Diagnostics.Process]::GetCurrentProcess().MainModule.FileName
		$run = @("-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $ps1)
		Say "aigw-token is in $bin (aigw-token.cmd)"
		$pathDirs = ($env:PATH -split [IO.Path]::PathSeparator) | ForEach-Object { $_.TrimEnd("\", "/") }
		if ($pathDirs -notcontains $bin.TrimEnd("\", "/")) { Say "add $bin to your PATH to run aigw-token yourself" }

		# 2. Signed in. Already signed in to this hub with a key it takes: kept.
		$keptHub = ""
		if (Test-Path -LiteralPath (Join-Path $config "hub")) { $keptHub = ([IO.File]::ReadAllText((Join-Path $config "hub"))).Trim() }
		$signedIn = $false
		if ($keptHub -eq $Hub) {
			$env:AIGW_TOKEN_RENEW_SECONDS = "0"
			& $self @run 2>$null | Out-Null
			$signedIn = $LASTEXITCODE -eq 0
			Remove-Item Env:\AIGW_TOKEN_RENEW_SECONDS
		}
		if ($signedIn) {
			Say "already signed in to $Hub (aigw-token logout, then run this again, to change the key)"
		} elseif ($update) {
			Fail "not signed in to $Hub with a key it takes: the update stops; run the installer by hand"
		} elseif ($env:AIGW_PERSONAL_KEY) {
			# Piped, so the key is on no command line.
			$env:AIGW_PERSONAL_KEY | & $self @run login $Hub
			if ($LASTEXITCODE -ne 0) { Fail "not signed in" }
		} else {
			& $self @run login $Hub
			if ($LASTEXITCODE -ne 0) { Fail "not signed in" }
		}

		# 3. What the hub sets clients up with: the gateway's address and the
		# person's models (GET /api/v1/my/models, with the personal key). A
		# hub before 0.34.0 has none of it.
		$key = ([IO.File]::ReadAllText((Join-Path $config "personal-key"))).Trim()
		$setup = $null
		try { $setup = Invoke-RestMethod -UseBasicParsing -TimeoutSec 30 -Uri "$Hub/api/v1/my/models" -Headers @{ Authorization = "Bearer $key" } -ErrorAction Stop } catch {}
		# A gateway address given by hand is kept for the updates, which give none.
		$saved = Join-Path $config "gateway"
		if ($update) {
			$Gateway = ""
			if (Test-Path -LiteralPath $saved) { $Gateway = ([IO.File]::ReadAllText($saved)).Trim() }
		} elseif ($Gateway) {
			[IO.File]::WriteAllText($saved, "$Gateway`n")
		} else {
			Remove-Item -Force -ErrorAction SilentlyContinue -LiteralPath $saved
		}
		if (-not $Gateway -and $setup) { $Gateway = [string]$setup.gateway_url }
		# An address copied from a client's settings ends with its API's path.
		$Gateway = $Gateway -replace "/$", "" -replace "/anthropic$", "" -replace "/v1$", ""
		if (-not $Gateway) { Say "the hub is not told the gateway's address (config.gatewayURL): ask its operator, or give it after the hub's" }
		$models = @()
		if ($setup -and $setup.models) { $models = @($setup.models) }
		$cc = $null
		if ($setup) { $cc = $setup.claude_code }
		$node = Find-Program "node"

		# 4. Claude Code: aigw-token as the API key helper, the gateway's
		# Anthropic API as the address, the hub's models where the person has
		# none of their own, and the plugin's status line when there is none.
		# Everything else in the settings is kept, and the old file beside them.
		$claude = Find-Program "claude"
		if ($claude -or (Test-Path -LiteralPath $claudeDir)) {
			New-Item -ItemType Directory -Force -Path $claudeDir | Out-Null
			$file = Join-Path $claudeDir "settings.json"
			$s = [PSCustomObject]@{}
			$readable = $true
			if (Test-Path -LiteralPath $file) {
				$text = [IO.File]::ReadAllText($file)
				try {
					# PowerShell 7 reads a date-like string as a date unless told.
					$convert = @{ InputObject = $text; ErrorAction = "Stop" }
					if ((Get-Command ConvertFrom-Json).Parameters.ContainsKey("DateKind")) { $convert.DateKind = "String" }
					if ($text.Trim()) { $s = ConvertFrom-Json @convert }
					if ($s -isnot [Management.Automation.PSCustomObject]) { throw "not an object" }
				} catch {
					$readable = $false
					Say "$file is not JSON; left as it is"
				}
				if ($readable) { Copy-Item -Force -LiteralPath $file -Destination "$file.aigw-backup" }
			}
			if ($readable) {
				Set-Property $s "apiKeyHelper" $helper
				$env_ = [ordered]@{}
				if ($s.env) { foreach ($p in $s.env.PSObject.Properties) { $env_[$p.Name] = $p.Value } }
				if ($Gateway) { $env_["ANTHROPIC_BASE_URL"] = "$Gateway/anthropic" }
				# Claude Code asks for Claude models unless told otherwise. A
				# model the person set that they may call is kept; any other is
				# replaced.
				$mine = @($models | ForEach-Object { [string]$_.name })
				if ($cc) {
					$want = [ordered]@{
						ANTHROPIC_MODEL                = $cc.model
						ANTHROPIC_DEFAULT_OPUS_MODEL   = $cc.model
						ANTHROPIC_DEFAULT_SONNET_MODEL = $cc.model
						ANTHROPIC_DEFAULT_FABLE_MODEL  = $cc.model
						ANTHROPIC_DEFAULT_HAIKU_MODEL  = $cc.small_model
						ANTHROPIC_SMALL_FAST_MODEL     = $cc.small_model
					}
					foreach ($k in $want.Keys) {
						if ($mine -notcontains [string]$env_[$k]) { $env_[$k] = $want[$k] }
					}
				}
				# Claude Code takes a model it does not know for 200,000 tokens
				# long and asks for answers of up to 32,000, which a shorter model
				# refuses: the length of the model it works with, and a quarter of
				# it for the answer.
				$used = $models | Where-Object { [string]$_.name -eq [string]$env_["ANTHROPIC_MODEL"] } | Select-Object -First 1
				if ($used -and [int64]$used.context_window -gt 0) {
					$length = [int64]$used.context_window
					$env_["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] = [string]$length
					$env_["CLAUDE_CODE_MAX_OUTPUT_TOKENS"] = [string][Math]::Min(32000, [Math]::Floor($length / 4))
				}
				if ($env_.Count -gt 0) { Set-Property $s "env" ([PSCustomObject]$env_) }
				# The status line runs through Git Bash, or PowerShell without
				# it: a path with forward slashes works in both. The plugin puts
				# the script there at the start of a session.
				if (-not $s.statusLine -and $node) {
					$line = ($home_ -replace "\\", "/") + "/.claude/aigw-usage/statusline.mjs"
					Set-Property $s "statusLine" ([PSCustomObject][ordered]@{ type = "command"; command = "node `"$line`""; refreshInterval = 30 })
				}
				# Windows PowerShell 5.1's type data for arrays makes ConvertTo-Json
				# write some lists as {"value": [...], "Count": n}.
				if ($PSVersionTable.PSVersion.Major -lt 6) { Remove-TypeData System.Array -ErrorAction SilentlyContinue }
				[IO.File]::WriteAllText($file, (ConvertTo-Json -InputObject $s -Depth 64) + "`n")
				$model = [string]$env_["ANTHROPIC_MODEL"]
				if ($model) { Say "Claude Code settings written to $file, model $model" } else {
					Say "Claude Code settings written to $file"
					Say "the hub gave no model for Claude Code: set env.ANTHROPIC_MODEL in $file to one of your models"
				}
			}

			# The aigw-usage plugin, from a copy of its marketplace the hub
			# serves. Its hooks and status line run node.
			if (-not $claude) {
				Say "claude is not on the PATH: the plugin is skipped; run this again once it is"
			} elseif (-not $node) {
				Say "node is not installed: the aigw-usage plugin, which shows your budget, needs it; install Node.js and run this again"
			} else {
				$market = Join-Path $claudeDir "aigw-marketplace"
				$archive = Join-Path $tmp "claude-code.tar.gz"
				Get-HubFile "claude-code.tar.gz" $archive
				Remove-Item -Recurse -Force -ErrorAction SilentlyContinue -LiteralPath "$market.tmp"
				New-Item -ItemType Directory -Force -Path "$market.tmp" | Out-Null
				# Windows' own tar (bsdtar): Git's GNU tar reads C:\... as a host.
				$tar = "tar"
				$system = [Environment]::SystemDirectory
				if ($system -and (Test-Path -LiteralPath (Join-Path $system "tar.exe"))) { $tar = Join-Path $system "tar.exe" }
				& $tar -xzf $archive -C "$market.tmp"
				if ($LASTEXITCODE -ne 0) { Fail "could not unpack the plugin from $Hub" }
				# Claude Code loads the plugin from this folder at each session's
				# start: swapped in two renames, so a session that starts
				# meanwhile misses it for as short a time as can be.
				Remove-Item -Recurse -Force -ErrorAction SilentlyContinue -LiteralPath "$market.old"
				if (Test-Path -LiteralPath $market) { Move-Item -LiteralPath $market -Destination "$market.old" }
				Move-Item -LiteralPath "$market.tmp" -Destination $market
				Remove-Item -Recurse -Force -ErrorAction SilentlyContinue -LiteralPath "$market.old"
				$list = & $claude plugin marketplace list 2>$null
				if ($list | Select-String -Pattern "\baigw\b" -Quiet) {
					& $claude plugin marketplace update aigw | Out-Null
				} else {
					& $claude plugin marketplace add $market | Out-Null
				}
				if ($LASTEXITCODE -ne 0) { Fail "claude could not add the plugin's marketplace" }
				# The plugin finds the key aigw-token keeps; the hub is given so
				# that Claude Code does not ask for it.
				& $claude plugin install aigw-usage@aigw --config "hub_url=$Hub" --config warn_at=90 | Out-Null
				if ($LASTEXITCODE -ne 0) { Fail "claude could not install the aigw-usage plugin" }
				Say "the aigw-usage plugin is installed: restart Claude Code to see your budget in the status line"
			}
		} else {
			Say "Claude Code is not installed: skipped; run this again once it is"
		}

		# 5. Oh My Pi: the extension adds the person's models from the hub as
		# the provider aigw, with aigw-token for tokens, and shows the budget.
		$omp = Find-Program "omp"
		if ($omp -or (Test-Path -LiteralPath $ompDir)) {
			$extensions = Join-Path $ompDir "extensions"
			New-Item -ItemType Directory -Force -Path $extensions | Out-Null
			Get-HubFile "aigw-usage.ts" (Join-Path $extensions "aigw-usage.ts")
			Say "the Oh My Pi extension is in ${extensions}: omp --model aigw/<model> (omp models aigw lists them)"
			# Its roles: the hub's model as the default, and its small model as
			# smol, for omp's quick tasks. A role the person set to another
			# provider is theirs; one on a gateway model they may no longer
			# call is replaced.
			if ($omp -and $cc) {
				$roles = [ordered]@{}
				try {
					$got = (& $omp config get modelRoles --json 2>$null | Out-String) | ConvertFrom-Json -ErrorAction Stop
					if ($got.value) { foreach ($p in $got.value.PSObject.Properties) { $roles[$p.Name] = $p.Value } }
				} catch {}
				$gatewayModels = @($models | ForEach-Object { "aigw/" + $_.name })
				$changed = $false
				foreach ($role in @(@("default", $cc.model), @("smol", $cc.small_model))) {
					# A role may carry a thinking level after a colon.
					$now = ([string]$roles[$role[0]]) -replace ":[^/]*$", ""
					if (-not $now -or ($now.StartsWith("aigw/") -and $gatewayModels -notcontains $now)) {
						$roles[$role[0]] = "aigw/" + $role[1]
						$changed = $true
					}
				}
				if ($changed) {
					$json = ConvertTo-Json -InputObject $roles -Compress
					& $omp config set modelRoles (Native-Argument $json $omp) | Out-Null
					if ($LASTEXITCODE -eq 0) { Say "Oh My Pi's model roles: $json" }
				}
			}
			$models_yml = Join-Path $ompDir "models.yml"
			if ((Test-Path -LiteralPath $models_yml) -and (Select-String -LiteralPath $models_yml -Pattern "^\s+aigw:\s*(#.*)?$" -Quiet)) {
				Say "$models_yml defines the provider aigw itself, so the hub's models are not added: remove it there to get them"
			}
		} else {
			Say "Oh My Pi is not installed: skipped; run this again once it is"
		}

		# 6. The gateway takes the token. While it takes API keys too, it takes
		# a personal key's token once a sync has listed it, a few seconds after
		# the first sign-in.
		if ($Gateway -and -not $update) {
			$wait = 60
			if ($env:AIGW_CHECK_SECONDS) { $wait = [int]$env:AIGW_CHECK_SECONDS }
			$start = Get-Date
			$waited = $false
			while ($true) {
				$code = 0
				$bearer = (& $self @run 2>$null | Select-Object -Last 1)
				try {
					$r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 10 -Uri "$Gateway/v1/models" -Headers @{ Authorization = "Bearer $bearer" } -ErrorAction Stop
					$code = [int]$r.StatusCode
				} catch {
					$response = $_.Exception.Response
					if ($response) { $code = [int]$response.StatusCode }
				}
				if ($code -ne 401 -or ((Get-Date) - $start).TotalSeconds -ge $wait) { break }
				if (-not $waited) { Say "waiting for the gateway to take your token" }
				$waited = $true
				Start-Sleep -Seconds 2
			}
			switch ($code) {
				200 { Say "the gateway at $Gateway takes your token" }
				401 { Say "the gateway at $Gateway still refuses your token (401): tell the hub's operator" }
				0 { Say "the gateway at $Gateway cannot be reached from here" }
				default { Say "the gateway at $Gateway answered HTTP $code" }
			}
		}
		# What was installed, for aigw-token to tell when to run this again.
		if ($setup -and $setup.clients_version) {
			$now = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
			[IO.File]::WriteAllText((Join-Path $config "clients"), "$($setup.clients_version) $now`n")
		}
		Say "done"
	} finally {
		Remove-Item -Recurse -Force -ErrorAction SilentlyContinue -LiteralPath $tmp
		$env:AIGW_AUTO_UPDATE = $autoUpdate
	}
}

# Run as a file (powershell -File install.ps1 HUB) it can say it failed;
# run through iex, exit would close the person's window.
$asFile = [bool]$MyInvocation.MyCommand.Path
try {
	Install-Aigw
} catch {
	[Console]::Error.WriteLine($_.Exception.Message)
	if ($asFile) { exit 1 }
}
