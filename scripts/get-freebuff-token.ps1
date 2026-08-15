# get-freebuff-token.ps1 - install the FreeBuff CLI, log in, extract the auth token
#
# Usage:
#   .\get-freebuff-token.ps1                # install + login (if needed) + write token into ../.env (or repo .env)
#   .\get-freebuff-token.ps1 -ToClipboard   # copy the token to the clipboard instead
#   .\get-freebuff-token.ps1 -Print         # print the RAW token (be careful - it is a credential)
#   .\get-freebuff-token.ps1 -Force         # force re-login even if a token already exists
#   .\get-freebuff-token.ps1 -EnvFile D:\path\.env   # explicit target .env
#
# Token sources (documented by the community proxies):
#   - Web:        https://freebuff.llm.pm  (login -> token shown on the page; no install)
#   - CLI:        this script - installs `freebuff`, runs the interactive login,
#                 then reads the authToken from the credentials file the CLI writes:
#                     %USERPROFILE%\.config\manicode\credentials.json
#                     ~/.config/manicode/credentials.json          (WSL/bash)
#
# The token is a 36-char UUID (or user_... form) and is used WITHOUT any "Bearer "
# prefix - the proxy adds it upstream itself.
param(
  [switch]$Print,
  [switch]$ToClipboard,
  [switch]$Force,
  [switch]$Logout,
  [string]$EnvFile = ""
)

$ErrorActionPreference = "Stop"

# --- 0. warning -------------------------------------------------------------
Write-Host ""
Write-Host "WARNING: using your FreeBuff token through this proxy conflicts with FreeBuff/Codebuff" -ForegroundColor Red
Write-Host "terms of service. Accounts get suspended or banned (403 account_banned, dashboard shows" -ForegroundColor Red
Write-Host "'suspended'). Bans are per account, usually permanent, and there is no self-service" -ForegroundColor Red
Write-Host "unban. Use ONE account, keep usage modest, do not run the proxy 24/7, and expect the" -ForegroundColor Red
Write-Host "account to be banned eventually. You accept this risk by continuing." -ForegroundColor Red
Write-Host ""

function Find-CredentialsFile {
  $candidates = @(
    (Join-Path $env:USERPROFILE ".config\manicode\credentials.json"),
    (Join-Path $env:USERPROFILE ".config\codebuff\credentials.json"),
    (Join-Path $HOME ".config/manicode\credentials.json")
  )
  foreach ($p in $candidates) { if (Test-Path $p) { return $p } }
  return $null
}

function Get-AuthToken([string]$path) {
  try {
    $data = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
  } catch {
    return $null
  }
  $acct = $null
  if ($null -ne $data.default) { $acct = $data.default }
  if ($null -eq $acct -and $null -ne $data) {
    $acct = $data.PSObject.Properties | ForEach-Object { $_.Value } |
      Where-Object { $_ -and $_.authToken } | Select-Object -First 1
  }
  if ($acct -and $acct.authToken) { return [string]$acct.authToken }
  return $null
}

function Get-AccountEmail([string]$path) {
  try {
    $data = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
    $acct = $null
    if ($null -ne $data.default) { $acct = $data.default }
    if ($null -eq $acct -and $null -ne $data) {
      $acct = $data.PSObject.Properties | ForEach-Object { $_.Value } |
        Where-Object { $_ -and $_.email } | Select-Object -First 1
    }
    if ($acct -and $acct.email) { return [string]$acct.email }
  } catch {}
  return $null
}

# --- 1. handle logout if requested ----------------------------------------
if ($Logout) {
  $creds = Find-CredentialsFile
  if ($creds -and (Test-Path $creds)) {
    Remove-Item -LiteralPath $creds -Force -ErrorAction SilentlyContinue
    Write-Host "Cleared existing login credentials ($creds)." -ForegroundColor Yellow
  } else {
    Write-Host "No existing credentials found to clear." -ForegroundColor Yellow
  }
  Write-Host "Ready to log in with a new FreeBuff account." -ForegroundColor Cyan
}

# --- 2. install the FreeBuff CLI -------------------------------------------
if (-not (Get-Command freebuff -ErrorAction SilentlyContinue)) {
  if (-not (Get-Command node -ErrorAction SilentlyContinue)) {
    Write-Host "ERROR: node/npm not found and the freebuff CLI is not installed either." -ForegroundColor Red
    Write-Host "Install Node.js >= 22 (https://nodejs.org) and re-run." -ForegroundColor Red
    exit 1
  }
  Write-Host "Installing the FreeBuff CLI (npm i -g freebuff)..." -ForegroundColor Cyan
  npm install -g freebuff
  if ($LASTEXITCODE -ne 0) { Write-Host "npm install failed." -ForegroundColor Red; exit 1 }
}

$existing = Find-CredentialsFile
if (-not $Force -and -not $Logout -and $existing -and (Get-AuthToken $existing)) {
  $email = Get-AccountEmail $existing
  if ($email) {
    Write-Host "Already logged in as $email - reusing existing token ($existing). Use -Logout to log in with a new account." -ForegroundColor Green
  } else {
    Write-Host "Already logged in - reusing existing token ($existing). Use -Logout to log in with a new account." -ForegroundColor Green
  }
} else {
  Write-Host "Starting the FreeBuff login - complete it in the browser/terminal that opens, then come back." -ForegroundColor Yellow
  & freebuff
  if ($LASTEXITCODE -ne 0) { Write-Host "freebuff exited with code $LASTEXITCODE - login may have failed." -ForegroundColor Red; exit 1 }
}
# --- 3. extract the token ---------------------------------------------------
$creds = Find-CredentialsFile
if (-not $creds) {
  Write-Host "ERROR: credentials file not found after login (~\.config\manicode\credentials.json)." -ForegroundColor Red
  Write-Host "Make sure you completed the 'freebuff' CLI login in your browser." -ForegroundColor Red
  exit 1
}
$token = Get-AuthToken $creds
if (-not $token) {
  Write-Host "ERROR: no authToken field in $creds" -ForegroundColor Red
  exit 1
}

if ($token.Length -gt 12) {
  $masked = $token.Substring(0, 8) + "..." + $token.Substring($token.Length - 4)
} else {
  $masked = "***"
}
Write-Host "Token found ($($token.Length) chars): $masked" -ForegroundColor Green

# --- 4. deliver --------------------------------------------------------------
if ($Print) {
  Write-Host $token
  exit 0
}
if ($ToClipboard) {
  Set-Clipboard -Value $token
  Write-Host "Token copied to clipboard." -ForegroundColor Green
  exit 0
}
# Default: write AUTH_TOKENS into a freebuff-proxy .env
if (-not $EnvFile) {
  $repoEnv = Join-Path (Split-Path $PSScriptRoot -Parent) ".env"   # scripts/.. -> repo root
  if (Test-Path (Join-Path (Split-Path $PSScriptRoot -Parent) ".env.example")) { $EnvFile = $repoEnv }
}
if ($EnvFile) {
  if (Test-Path $EnvFile) {
    $content = Get-Content $EnvFile -Raw
    if ($content -match '(?m)^AUTH_TOKENS=.*$') {
      $content = $content -replace '(?m)^AUTH_TOKENS=.*$', "AUTH_TOKENS=$token"
    } else {
      $content = $content.TrimEnd() + "`nAUTH_TOKENS=$token`n"
    }
    Set-Content -Path $EnvFile -Value $content -NoNewline -Encoding utf8
  } else {
    Set-Content -Path $EnvFile -Value "AUTH_TOKENS=$token`n" -Encoding utf8
  }
  Write-Host "AUTH_TOKENS written to $EnvFile (gitignored)." -ForegroundColor Green
} else {
  Write-Host "Token: $token" -ForegroundColor Yellow
  Write-Host "Run with -ToClipboard or -Print to see it again; or pass -EnvFile with a path." -ForegroundColor Yellow
}
