#!/usr/bin/env bash
# Install / start continuous synthetic-org traffic.
#
# Copies the plist from the repo into ~/Library/LaunchAgents/ and
# registers it with macOS launchd. After this completes, the
# synthetic-org runs automatically every hour in dry-run mode.
#
# Idempotent: safe to re-run after editing the plist in the repo.
# The unload step silently no-ops if nothing was previously loaded.
#
# Run:
#   bash synthetic-org/scripts/install_continuous_traffic.sh

set -uo pipefail

PLIST_SRC="/Users/robertcanario/mesedi/synthetic-org/scripts/ai.mesedi.synthetic-org.plist"
PLIST_DST="$HOME/Library/LaunchAgents/ai.mesedi.synthetic-org.plist"
LABEL="ai.mesedi.synthetic-org"

if [[ ! -f "$PLIST_SRC" ]]; then
  echo "ERROR: plist not found at $PLIST_SRC"
  echo "       Is the Mesedi repo at the expected location?"
  exit 1
fi

echo "── Installing continuous synthetic-org traffic ──"
echo ""
echo "  Source plist: $PLIST_SRC"
echo "  Destination:  $PLIST_DST"
echo ""

echo "  [1/3] Copying plist to LaunchAgents..."
cp "$PLIST_SRC" "$PLIST_DST"
echo "         ok"

echo "  [2/3] Unloading any previous registration (idempotent)..."
launchctl unload "$PLIST_DST" 2>/dev/null
echo "         ok"

echo "  [3/3] Loading new registration..."
if launchctl load "$PLIST_DST"; then
  echo "         ok"
else
  echo "         FAILED. Check the plist for syntax errors:"
  echo "           plutil -lint $PLIST_DST"
  exit 1
fi

echo ""
echo "── Status ──"
if launchctl list | grep -q "$LABEL"; then
  echo "  ✓ $LABEL is registered with launchd"
  launchctl list | grep "$LABEL" | sed 's/^/    /'
else
  echo "  ✗ $LABEL was NOT found in launchctl list — install may have failed silently"
  exit 1
fi

echo ""
echo "── What happens now ──"
echo "  - First batch fires 3600 seconds (1 hour) from now."
echo "  - To trigger an immediate batch for testing:"
echo "      launchctl start $LABEL"
echo "  - To watch the next batch live:"
echo "      tail -f /Users/robertcanario/mesedi/synthetic-org/continuous_traffic.log"
echo "  - To stop the scheduled job:"
echo "      bash synthetic-org/scripts/stop_continuous_traffic.sh"
echo ""
