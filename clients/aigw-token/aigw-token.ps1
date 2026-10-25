# aigw-token for Windows: the sh aigw-token in PowerShell. Claude Code on
# Windows runs its apiKeyHelper with cmd.exe and Oh My Pi runs commands with
# its own shell, so neither can run a sh script; the hub's install.ps1 gives
# both "powershell.exe -File" for this file instead.
#
# It prints a token for the AI gateway: it trades the user's personal key at
# the hub for a token that carries the tenant's budget and models as they are
# now, and gets a new one every AIGW_TOKEN_RENEW_SECONDS (600). If the hub
# cannot be reached it prints the token it has while that is valid (7 days by
# default), so a hub that is down stops nobody.
#
#   aigw-token login HUB_URL [PERSONAL_KEY]   keep the hub's address and the key
#                                            (asked for when not given)
#   aigw-token                               print a token
#   aigw-token logout                        forget them
#
# It keeps the same files as the sh one, in %USERPROFILE%\.config\aigw, so
# the Claude Code plugin and the Oh My Pi extension find the key there. As
# the sh one does, it runs the hub's installer again, hidden, when the hub
# serves other client files or a week after the last setup (Follow-Hub).
#
# Windows PowerShell 5.1 and PowerShell 7 both run this file: keep it ASCII,
# as 5.1 reads a file without a byte order mark in the system's code page.
param(
	[Parameter(Position = 0)][string]$Action = "",
	[Parameter(Position = 1)][string]$HubUrl = "",
	[Parameter(Position = 2)][string]$PersonalKey = ""
)
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
# Windows PowerShell 5.1 on an older .NET offers TLS 1.0 only unless told.
try { [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072 } catch {}

$base = $env:XDG_CONFIG_HOME
if (-not $base) { $base = Join-Path $HOME ".config" }
$dir = $env:AIGW_CONFIG_DIR
if (-not $dir) { $dir = Join-Path $base "aigw" }
$renew = 600
if ($env:AIGW_TOKEN_RENEW_SECONDS) { $renew = [int64]$env:AIGW_TOKEN_RENEW_SECONDS }

function Fail([string]$Message) {
	[Console]::Error.WriteLine("aigw-token: $Message")
	exit 1
}

function Read-Kept([string]$Name) {
	$path = Join-Path $dir $Name
	if (Test-Path -LiteralPath $path) { return ([IO.File]::ReadAllText($path)).Trim() }
	return ""
}

# A file is written whole and then renamed, as the sh one does, so a reader
# never sees half of it. WriteAllText writes UTF-8 without a byte order mark.
function Save([string]$Name, [string]$Text) {
	$path = Join-Path $dir $Name
	[IO.File]::WriteAllText("$path.tmp", $Text)
	Move-Item -Force -LiteralPath "$path.tmp" -Destination $path
}

function Forget([string]$Name) {
	Remove-Item -Force -ErrorAction SilentlyContinue -LiteralPath (Join-Path $dir $Name)
}

# The installer keeps "<version> <when>" in clients. When the hub serves
# other client files, or AIGW_UPDATE_DAYS (7) after the last setup, it runs
# again from the hub in the background, hidden, with this sign-in, writing
# to update.log. A try that has not finished a setup (update-tried later
# than the setup) is not made again for a day; after a setup, the next
# version is taken at once. AIGW_AUTO_UPDATE=0 turns it off. Nothing here
# may stop the token from being printed.
function Follow-Hub([string]$Want, [string]$Hub, [int64]$Now) {
	if (-not $Want -or $env:AIGW_AUTO_UPDATE -eq "0") { return }
	if (-not (Test-Path -LiteralPath (Join-Path $dir "clients"))) { return }
	try {
		$installed = (Read-Kept "clients") -split " "
		$at = [int64]0
		if ($installed.Count -ge 2) { $null = [int64]::TryParse($installed[1], [ref]$at) }
		$tried = [int64]0
		$null = [int64]::TryParse((Read-Kept "update-tried"), [ref]$tried)
		$days = [int64]7
		if ($env:AIGW_UPDATE_DAYS) { $null = [int64]::TryParse($env:AIGW_UPDATE_DAYS, [ref]$days) }
		if ($installed[0] -eq $Want -and ($Now - $at) -lt ($days * 86400)) { return }
		if ($tried -gt $at -and ($Now - $tried) -lt 86400) { return }
		Save "update-tried" "$Now`n"

		$quote = { param([string]$Text) "'" + ($Text -replace "'", "''") + "'" }
		$script = @(
			'$env:AIGW_UPDATE = "1"'
			('$log = New-Object IO.StreamWriter(' + (& $quote (Join-Path $dir "update.log")) + ')')
			'$log.AutoFlush = $true'
			'[Console]::SetOut($log)'
			'[Console]::SetError($log)'
			('try { & ([scriptblock]::Create((Invoke-RestMethod -UseBasicParsing -TimeoutSec 60 -Uri ' + (& $quote "$Hub/clients/install.ps1") + '))) ' + (& $quote $Hub) + ' }')
			'catch { [Console]::Error.WriteLine($_.Exception.Message) } finally { $log.Close() }'
		) -join "`n"
		$encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($script))
		$self = [Diagnostics.Process]::GetCurrentProcess().MainModule.FileName
		$arguments = "-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand $encoded"
		# Through the shell and hidden, so it holds none of the caller's
		# handles: Claude Code waits for this helper's output to end, and gets
		# its token now. PowerShell 7 off Windows has no window to hide.
		if ($PSVersionTable.PSVersion.Major -lt 6 -or $IsWindows) {
			Start-Process -FilePath $self -ArgumentList $arguments -WindowStyle Hidden
		} else {
			Start-Process -FilePath $self -ArgumentList $arguments
		}
	} catch {}
}

function Get-Token {
	$hub = Read-Kept "hub"
	$key = Read-Kept "personal-key"
	if (-not $hub -or -not $key) { Fail "not signed in: run aigw-token login HUB_URL" }
	$now = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()

	# The token kept, as "<fetched> <expires> <token>": printed while it is
	# young enough, so the hub is asked once per AIGW_TOKEN_RENEW_SECONDS.
	$fetched = [int64]0
	$expires = [int64]0
	$token = ""
	$kept = (Read-Kept "token") -split " "
	if ($kept.Count -ge 3) {
		$fetched = [int64]$kept[0]
		$expires = [int64]$kept[1]
		$token = $kept[2]
	}
	if ($token -and ($now - $fetched) -lt $renew -and $expires -gt ($now + 60)) { return $token }

	# The key goes in a header of this process, never on a command line.
	$code = 0
	$body = ""
	try {
		$r = Invoke-WebRequest -UseBasicParsing -Method Post -TimeoutSec 10 -Uri "$hub/api/v1/token" -Headers @{ Authorization = "Bearer $key" }
		$code = [int]$r.StatusCode
		$body = [string]$r.Content
	} catch {
		# PowerShell 7 and 5.1 both put the answer of an HTTP error here;
		# a hub that cannot be reached has none.
		$response = $_.Exception.Response
		if ($response) { $code = [int]$response.StatusCode }
	}
	if ($code -eq 200) {
		$answer = $null
		try { $answer = $body | ConvertFrom-Json } catch {}
		if (-not $answer -or -not $answer.token -or -not $answer.expires_in) { Fail "the hub's answer has no token" }
		Save "token" ("{0} {1} {2}`n" -f $now, ($now + [int64]$answer.expires_in), $answer.token)
		Follow-Hub ([string]$answer.clients_version) $hub $now
		return [string]$answer.token
	}
	if ($code -eq 401) {
		# The key is revoked or wrong: the token kept would be refused too.
		Forget "token"
		Fail "the hub does not take this personal key; ask for a new one"
	}
	# The hub cannot be reached or failed: the token kept, while it is valid.
	if ($token -and $expires -gt ($now + 60)) { return $token }
	Fail ("the hub gave no token (HTTP {0:D3}) and the one kept has expired" -f $code)
}

switch ($Action) {
	"login" {
		if (-not $HubUrl) { Fail "usage: aigw-token login HUB_URL [PERSONAL_KEY]" }
		$hub = $HubUrl -replace "/$", ""
		$key = $PersonalKey
		if (-not $key) {
			if ([Console]::IsInputRedirected) {
				# A key piped in, as the installer does with AIGW_PERSONAL_KEY.
				$key = [Console]::In.ReadLine()
			} else {
				$secure = Read-Host -Prompt "Personal key" -AsSecureString
				$key = (New-Object Net.NetworkCredential("", $secure)).Password
			}
		}
		if ($null -eq $key) { $key = "" }
		$key = $key.Trim()
		if (-not $key.StartsWith("aigw-pk-")) { Fail "a personal key starts with aigw-pk-" }
		New-Item -ItemType Directory -Force -Path $dir | Out-Null
		Save "hub" "$hub`n"
		Save "personal-key" "$key`n"
		Forget "token"
		$null = Get-Token
		[Console]::Error.WriteLine("aigw-token: signed in to $hub")
		exit 0
	}
	"logout" {
		Forget "hub"
		Forget "personal-key"
		Forget "token"
		exit 0
	}
	"" {}
	default { Fail "usage: aigw-token [login HUB_URL [PERSONAL_KEY] | logout]" }
}

[Console]::Out.WriteLine((Get-Token))
exit 0
