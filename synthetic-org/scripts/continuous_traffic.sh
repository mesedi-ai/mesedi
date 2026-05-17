#!/usr/bin/env bash
# Continuous synthetic-org traffic — wrapper script invoked by launchd.
#
# Runs one batch of synthetic-org agents in dry-run mode (no API spend),
# appends stdout/stderr to a rotating log file. Designed to be called
# on a schedule (hourly via launchd) so the Mesedi dashboard stays
# continuously populated with fresh-shape failure-group data.
#
# Environment guarantees:
#   - cd to the synthetic-org directory (otherwise relative imports in
#     runner.py won't find synthetic_inputs)
#   - PATH includes /usr/local/bin and /opt/homebrew/bin so a launchd-
#     spawned subprocess can find python3 the same way an interactive
#     shell does (launchd's default PATH is minimal)
#   - MESEDI_SYNTHETIC_ORG_DRY_RUN=1 forces LLM calls to mock responses
#     so this script never charges the Anthropic account
#
# Failure-mode: if the backend isn't running, runner.py prints
# connection-refused errors and continues. The script does NOT retry
# or restart — by design, a regression that crashes the backend
# should be visible (gap in dashboard data) rather than papered over.

set -uo pipefail

# Resolve absolute path to the synthetic-org dir regardless of where
# this script was invoked from. Follows symlinks so launchd's
# arbitrary working directory doesn't matter.
SCRIPT_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"
SYNTH_DIR="$( cd -- "$SCRIPT_DIR/.." &> /dev/null && pwd )"
LOG_FILE="$SYNTH_DIR/continuous_traffic.log"

# Rotate log when it crosses 5 MB. Keeps history bounded without
# requiring logrotate or any system-level config.
if [[ -f "$LOG_FILE" ]] && [[ $(stat -f%z "$LOG_FILE" 2>/dev/null || stat -c%s "$LOG_FILE") -gt 5242880 ]]; then
  mv "$LOG_FILE" "$LOG_FILE.1"
fi

# Inherit a workable PATH. launchd's default PATH is
# /usr/bin:/bin:/usr/sbin:/sbin — too narrow to find python3 on most
# Mac setups. Append the common Homebrew + system Python locations.
export PATH="/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"
export MESEDI_SYNTHETIC_ORG_DRY_RUN=1

cd "$SYNTH_DIR" || { echo "[$(date)] FATAL: cd $SYNTH_DIR failed" >> "$LOG_FILE"; exit 1; }

{
  echo ""
  echo "════════════════════════════════════════════════════════════════"
  echo "  $(date '+%Y-%m-%d %H:%M:%S')  — continuous_traffic batch start"
  echo "════════════════════════════════════════════════════════════════"
  python3 runner.py --agent all --iterations 10 --pace-seconds 0.5 2>&1
  echo "  exit_code=$?"
} >> "$LOG_FILE" 2>&1
