#!/usr/bin/env bash
set -euo pipefail
umask 077

# Docker bind mounts keep host ownership. UID 10001 needs to read the secret.
# A root-owned 0700 parent directory keeps other host users from reaching it.
mode="docker"
case "${1:-}" in
  "") ;;
  --local) mode="local" ;;
  -h|--help)
    printf 'Usage: sudo scripts/init.sh | scripts/init.sh --local\n'
    exit 0 ;;
  *) printf 'Unknown option. Use --local for a host binary.\n' >&2; exit 2 ;;
esac
if [[ "$mode" == "docker" && "$(id -u)" != "0" ]]; then
  printf 'Docker mode needs root to set secret ownership to UID 10001. Run sudo scripts/init.sh.\n' >&2
  exit 1
fi
command -v openssl >/dev/null || { printf 'openssl is required.\n' >&2; exit 1; }
project_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd -- "$project_dir"
[[ ! -L secrets ]] || { printf 'Refusing a symlink at secrets/.\n' >&2; exit 1; }
mkdir -p secrets
chmod 0700 secrets
if [[ "$mode" == "docker" ]]; then chown 0:0 secrets; fi
password_file="secrets/admin_password.txt"
[[ ! -L "$password_file" ]] || { printf 'Refusing a symlink at the password path.\n' >&2; exit 1; }
if [[ ! -e "$password_file" ]]; then
  # Write and set permissions before making the completed password visible.
  temporary_file="$(mktemp secrets/.password.XXXXXX)"
  trap 'rm -f -- "$temporary_file"' EXIT
  openssl rand -base64 36 > "$temporary_file"
  chmod 0600 "$temporary_file"
  if [[ "$mode" == "docker" ]]; then chown 10001:10001 "$temporary_file"; fi
  mv -- "$temporary_file" "$password_file"
  trap - EXIT
  printf 'Created secrets/admin_password.txt. The password is not printed.\n'
else
  [[ -f "$password_file" && -s "$password_file" ]] || { printf 'Existing password must be a nonempty regular file.\n' >&2; exit 1; }
  chmod 0600 "$password_file"
  if [[ "$mode" == "docker" ]]; then chown 10001:10001 "$password_file"; fi
  printf 'Kept the existing admin password.\n'
fi
if [[ ! -e .env ]]; then
  cp deploy/env.example .env
  chmod 0600 .env
  printf 'Created .env; set SOLODRIVE_PUBLIC_URL before starting.\n'
fi
if [[ "$mode" == "local" ]]; then
  mkdir -p data
  chmod 0700 data
fi
printf 'Read the password locally when needed: sudo cat secrets/admin_password.txt\n'
