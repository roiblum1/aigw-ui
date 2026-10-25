// The lines a person runs to set up their tools from this hub: one for a
// Unix shell (macOS, Linux, WSL) and one for PowerShell on Windows, where
// Claude Code and Oh My Pi cannot run the sh aigw-token.

/** The line for macOS, Linux and WSL. */
export function unixInstallLine(hub: string): string {
  return `curl -fsSL ${hub}/clients/install.sh | sh -s -- ${hub}`;
}

/**
 * The line for PowerShell, Microsoft's own pattern for a script from the web
 * that takes arguments: the script is fetched, put in a script block and run
 * with the hub's address. iex runs it inside the person's session, so the
 * execution policy does not stop it.
 */
export function windowsInstallLine(hub: string): string {
  return `iex "& { $(irm ${hub}/clients/install.ps1) } ${hub}"`;
}
