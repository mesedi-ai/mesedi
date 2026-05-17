#!/usr/bin/env bash
# Stop continuous synthetic-org traffic.
#
# Unregisters the launchd job so the synthetic-org stops running on
# the hourly schedule. The plist file in ~/Library/LaunchAgents/ is
# left in place by default — to re-enable, run install_continuous_traffic.sh
# again.
#
# If you want a FULL uninstall (remove the plist file too, not just
# pause it), run with --remove:
#   bash synthetic-org/scripts/stop_continuous_traffic.sh --remove
#
# Default (pause without removing the plist file):
#   bash synthetic-org/scripts/stop_continuous_traffic.sh

set -uo pipefail

PLIST_DST="$HOME/Library/LaunchAgents/ai.mesedi.synthetic-org.plist"
LABEL="ai.mesedi.synthetic-org"
REMOVE_FLAG="${1:-}"

echo "── Stopping continuous synthetic-org traffic ──"
echo ""

if [[ ! -f "$PLIST_DST" ]]; then
  echo "  Plist not present at $PLIST_DST"
  echo "  Nothing to stop — continuous traffic isn't installed."
  exit 0
fi

if launchctl list | grep -q "$LABEL"; then
  echo "  [1/2] Unloading registration..."
  launchctl unload "$PLIST_DST"
  echo "         ok"
else
  echo "  [1/2] Job not currently loaded — skipping unload."
fi

if [[ "$REMOVE_FLAG" == "--remove" ]]; then
  echo "  [2/2] Removing plist file (--remove flag set)..."
  rm "$PLIST_DST"
  echo "         removed $PLIST_DST"
  echo ""
  echo "  ✓ Continuous traffic fully uninstalled."
  echo "    To re-enable: bash synthetic-org/scripts/install_continuous_traffic.sh"
else
  echo "  [2/2] Plist file kept at $PLIST_DST (paused, not removed)."
  echo ""
  echo "  ✓ Continuous traffic stopped (paused)."
  echo "    To resume: bash synthetic-org/scripts/install_continuous_traffic.sh"
  echo "    To fully uninstall: bash synthetic-org/scripts/stop_continuous_traffic.sh --remove"
fi

echo ""
echo "── Status ──"
if launchctl list | grep -q "$LABEL"; then
  echo "  ✗ $LABEL is still in launchctl list — stop may have failed"
  exit 1
else
  echo "  ✓ $LABEL is no longer registered with launchd"
fi
echo ""
