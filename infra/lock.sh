# shellcheck shell=bash
# Sourced by scripts that change the VM or the bundle. Held until the script exits, so a second run
# stops here rather than destroying what the first is building.
exec 9>"$DATA_DIR/.lock"
if ! flock -n 9; then
  echo "another bundle or VM command is running, try again when it finishes" >&2
  exit 1
fi
