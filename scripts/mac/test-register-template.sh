#!/usr/bin/env bash
# Exercise register-template's GARM CLI invocation without a live controller.
# The renderer is stubbed only at the Go command boundary so the real template
# path check and real render-template.sh/registration scripts still execute.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
mkdir -p "$workdir/bin"

cat > "$workdir/bin/go" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [ "$#" -ne 3 ] || [ "$1" != run ]; then
  echo "unexpected go invocation: $*" >&2
  exit 1
fi
printf '#!/bin/bash\nexit 0\n' > "$3/jit.sh"
printf '#!/bin/bash\nexit 0\n' > "$3/nojit.sh"
STUB

cat > "$workdir/bin/garm-cli" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" > "$GARM_CLI_LOG"
STUB
chmod +x "$workdir/bin/go" "$workdir/bin/garm-cli"

GARM_CLI_LOG="$workdir/args" PATH="$workdir/bin:$PATH" \
  "$repo_root/scripts/mac/register-template.sh" fixture-template gitea

printf '%s\n' \
  template create \
  --name fixture-template \
  --path "$repo_root/internal/macprovider/macos-runner.sh" \
  --forge-type gitea \
  --os-type linux > "$workdir/expected"

diff -u "$workdir/expected" "$workdir/args"
echo "register-template self-test: passed"
