#!/usr/bin/env bash
# get-freebuff-token.sh — install the FreeBuff CLI, log in, extract the auth token
#
# Usage:
#   ./get-freebuff-token.sh                # install + login (if needed) + write token into repo .env
#   ./get-freebuff-token.sh --clipboard    # copy the token to the clipboard (xclip/xsel/pbcopy)
#   ./get-freebuff-token.sh --print        # print the RAW token (be careful — it is a credential)
#   ./get-freebuff-token.sh --force        # force re-login even if a token already exists
#   ./get-freebuff-token.sh --env-file /path/.env
#
# Token sources (documented by the community proxies):
#   - Web:        https://freebuff.llm.pm  (login → token shown on the page; no install)
#   - CLI:        this script — installs `freebuff`, runs the interactive login,
#                 then reads the authToken from the credentials file the CLI writes:
#                     ~/.config/manicode/credentials.json
#                     ~/.config/codebuff/credentials.json
#
# The token is a 36-char UUID (or user_... form) and is used WITHOUT any "Bearer "
# prefix — the proxy adds it upstream itself.
set -euo pipefail

# --- 0. warning -------------------------------------------------------------
echo ""
echo "WARNING: using your FreeBuff token through this proxy conflicts with FreeBuff/Codebuff" >&2
echo "terms of service. Accounts get suspended or banned (403 account_banned, dashboard shows" >&2
echo "'suspended'). Bans are per account, usually permanent, and there is no self-service" >&2
echo "unban. Use ONE account, keep usage modest, do not run the proxy 24/7, and expect the" >&2
echo "account to be banned eventually. You accept this risk by continuing." >&2
echo "" >&2

PRINT=0
CLIPBOARD=0
FORCE=0
LOGOUT=0
ENV_FILE=""

while [ $# -gt 0 ]; do
  case "$1" in
    --print) PRINT=1; shift ;;
    --clipboard) CLIPBOARD=1; shift ;;
    --force) FORCE=1; shift ;;
    --logout) LOGOUT=1; shift ;;
    --env-file) ENV_FILE="${2:-}"; shift 2 ;;
    --env-file=*) ENV_FILE="${1#*=}"; shift ;;
    --help) grep '^#' "$0" | head -30; exit 0 ;;
    *) echo "unknown arg: $1 (see header)" >&2; exit 1 ;;
  esac
done

if [ "$LOGOUT" = "1" ]; then
  for p in "$HOME/.config/manicode/credentials.json" "$HOME/.config/codebuff/credentials.json"; do
    [ -f "$p" ] && rm -f "$p" && echo "Cleared existing login credentials ($p)."
  done
  echo "Ready to log in with a new FreeBuff account."
fi

# --- 2. install the FreeBuff CLI -------------------------------------------
if ! command -v freebuff >/dev/null 2>&1; then
  if ! command -v node >/dev/null 2>&1; then
    echo "ERROR: node/npm not found and the freebuff CLI is not installed either." >&2
    echo "Install Node.js >= 22 (https://nodejs.org) and re-run." >&2
    exit 1
  fi
  echo "Installing the FreeBuff CLI (npm i -g freebuff)..."
  npm install -g freebuff
fi

CREDS=""
for p in "$HOME/.config/manicode/credentials.json" "$HOME/.config/codebuff/credentials.json"; do
  [ -f "$p" ] && CREDS="$p" && break
done

extract_token() {
  if command -v node >/dev/null 2>&1; then
    node -e '
      const fs = require("fs");
      const data = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
      const acct = data.default || Object.values(data)[0];
      if (acct && acct.authToken) process.stdout.write(acct.authToken);
    ' "$CREDS"
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c '
      import json, sys
      d = json.load(open(sys.argv[1], encoding="utf-8"))
      acct = d.get("default") or next(iter(d.values()), {})
      t = acct.get("authToken") if isinstance(acct, dict) else None
      if t: sys.stdout.write(t)
    ' "$CREDS"
  else
    # Crude fallback: single-line "authToken": "..." in the JSON.
    sed -n 's/.*"authToken"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CREDS" | head -1
  fi
}

extract_email() {
  if command -v node >/dev/null 2>&1; then
    node -e '
      const fs = require("fs");
      const data = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
      const acct = data.default || Object.values(data)[0];
      if (acct && acct.email) process.stdout.write(acct.email);
    ' "$CREDS" 2>/dev/null || true
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c '
      import json, sys
      d = json.load(open(sys.argv[1], encoding="utf-8"))
      acct = d.get("default") or next(iter(d.values()), {})
      e = acct.get("email") if isinstance(acct, dict) else None
      if e: sys.stdout.write(e)
    ' "$CREDS" 2>/dev/null || true
  fi
}

EMAIL="$(extract_email)"
if [ "$FORCE" = "0" ] && [ "$LOGOUT" = "0" ] && [ -n "$CREDS" ] && [ -n "$(extract_token)" ]; then
  if [ -n "$EMAIL" ]; then
    echo "Already logged in as $EMAIL — reusing the existing token ($CREDS). Use --logout to log in with a new account."
  else
    echo "Already logged in — reusing the existing token ($CREDS). Use --logout to log in with a new account."
  fi
else
  echo "Starting the FreeBuff login — complete it in the browser/terminal that opens, then come back."
  freebuff
fi
# --- 3. extract the token ---------------------------------------------------
CREDS=""
for p in "$HOME/.config/manicode/credentials.json" "$HOME/.config/codebuff/credentials.json"; do
  [ -f "$p" ] && CREDS="$p" && break
done
if [ -z "$CREDS" ]; then
  echo "ERROR: credentials file not found after login (~/.config/manicode/credentials.json)." >&2
  echo "Make sure you completed the 'freebuff' CLI login in your browser." >&2
  exit 1
fi
TOKEN="$(extract_token)"
if [ -z "$TOKEN" ]; then
  echo "ERROR: no authToken field in $CREDS" >&2
  exit 1
fi
MASKED="${TOKEN:0:8}...${TOKEN: -4}"
echo "Token found (${#TOKEN} chars): $MASKED"

# --- 4. deliver ------------------------------------------------------------
if [ "$PRINT" = "1" ]; then printf '%s\n' "$TOKEN"; exit 0; fi
if [ "$CLIPBOARD" = "1" ]; then
  if command -v pbcopy >/dev/null 2>&1; then printf '%s' "$TOKEN" | pbcopy
  elif command -v xclip >/dev/null 2>&1; then printf '%s' "$TOKEN" | xclip -selection clipboard
  elif command -v xsel >/dev/null 2>&1; then printf '%s' "$TOKEN" | xsel --clipboard --input
  else echo "No clipboard tool (pbcopy/xclip/xsel); token printed below:"; printf '%s\n' "$TOKEN"; exit 0
  fi
  echo "Token copied to clipboard."
  exit 0
fi

# Default: write AUTH_TOKENS into a freebuff-proxy .env (repo root next to scripts/)
if [ -z "$ENV_FILE" ] && [ -f "$(dirname "$0")/../.env.example" ]; then
  ENV_FILE="$(dirname "$0")/../.env"
fi
if [ -n "$ENV_FILE" ]; then
  if [ -f "$ENV_FILE" ]; then
    if grep -q '^AUTH_TOKENS=' "$ENV_FILE"; then
      sed -i "s|^AUTH_TOKENS=.*|AUTH_TOKENS=$TOKEN|" "$ENV_FILE"
    else
      printf 'AUTH_TOKENS=%s\n' "$TOKEN" >> "$ENV_FILE"
    fi
  else
    printf 'AUTH_TOKENS=%s\n' "$TOKEN" > "$ENV_FILE"
  fi
  echo "AUTH_TOKENS written to $ENV_FILE (gitignored)."
else
  echo "Token: $TOKEN"
  echo "Run with --clipboard or --print to see it again; or pass --env-file <path>."
fi
