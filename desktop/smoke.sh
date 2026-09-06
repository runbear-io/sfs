#!/bin/sh
# Smoke-check a RUNNING BearDrive.app: the sidecar answers in desktop mode,
# the macOS menu bar exists with every shortcut registered, and the tray is
# up. Works over SSH and even at the lock screen (accessibility reaches the
# session's menus); what it cannot check — actual key presses, downloads,
# external-link opens, window-state restore — is the manual ship checklist
# in .claude/skills/mac-app. Exit 0 = all assertions hold.
set -e

fail() { echo "FAIL: $1" >&2; exit 1; }

curl -sf http://127.0.0.1:8990/api/config | grep -q '"desktop":true' \
  || fail "sidecar not answering in desktop mode on :8990"
echo "ok sidecar desktop mode"

MENUS=$(osascript -e 'tell application "System Events" to tell process "BearDrive" to get name of menu bar items of menu bar 1')
case "$MENUS" in
  *BearDrive*Edit*View*History*Window*) echo "ok menu bar: $MENUS" ;;
  *) fail "menu bar structure: $MENUS" ;;
esac

# Accelerators, straight from the AX tree — this is the same registration
# macOS uses to dispatch the key equivalents.
ACCELS=$(osascript -e 'tell application "System Events" to tell process "BearDrive"
  set out to {}
  repeat with pair in {{"View", "Reload"}, {"View", "Actual Size"}, {"View", "Zoom In"}, {"View", "Zoom Out"}, {"History", "Back"}, {"History", "Forward"}, {"Edit", "Copy"}, {"Edit", "Paste"}, {"Window", "Close Window"}, {"Window", "Minimize"}}
  set mi to menu item (item 2 of pair) of menu 1 of menu bar item (item 1 of pair) of menu bar 1
    set end of out to (item 2 of pair as string) & "=" & (value of attribute "AXMenuItemCmdChar" of mi)
  end repeat
  return out
end tell')
echo "accelerators: $ACCELS"
for want in "Reload=R" "Actual Size=0" "Zoom In==" "Zoom Out=-" "Back=[" "Forward=]" "Copy=C" "Paste=V" "Close Window=W" "Minimize=M"; do
  case "$ACCELS" in
    *"$want"*) ;;
    *) fail "accelerator missing: $want (got: $ACCELS)" ;;
  esac
done
echo "ok all shortcut accelerators registered"

osascript -e 'tell application "System Events" to tell process "BearDrive" to get menu bar item 1 of menu bar 2' >/dev/null \
  || fail "tray icon missing"
echo "ok tray present"

# The onboarding entry point for later mounts (storyboard frame 10). Only
# shown when signed in — a signed-out app has nothing to connect a folder to.
TRAY=$(osascript -e 'tell application "System Events" to tell process "BearDrive" to get name of menu items of menu 1 of menu bar item 1 of menu bar 2')
case "$TRAY" in
  *"Connect a folder"*) echo "ok tray: Connect a folder…" ;;
  *"Sign in"*) echo "ok tray: signed out (Connect a folder… appears once signed in)" ;;
  *) fail "tray menu has neither Connect a folder… nor Sign in…: $TRAY" ;;
esac

echo "smoke: all checks passed"
