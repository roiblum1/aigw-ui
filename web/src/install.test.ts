// Run with "npm test".
import assert from "node:assert/strict";
import { test } from "node:test";
import { unixInstallLine, windowsInstallLine } from "./install.ts";

// The PowerShell line is the one install.ps1's tests run through pwsh; the
// hub's address goes in twice, the second time as the script's argument.
test("the install lines for a Unix shell and for PowerShell", () => {
  assert.equal(unixInstallLine("https://aigw.example.com"), "curl -fsSL https://aigw.example.com/clients/install.sh | sh -s -- https://aigw.example.com");
  assert.equal(windowsInstallLine("https://aigw.example.com"), 'iex "& { $(irm https://aigw.example.com/clients/install.ps1) } https://aigw.example.com"');
});
