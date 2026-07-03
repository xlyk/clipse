-- arrange-windows.applescript — position Linear (left half) and iTerm2 (right
-- half) of the main display for the smoke-test demo recording.
--
-- Requires Accessibility permission for the process that runs osascript (it
-- moves the Linear window via System Events). It ONLY positions windows — the
-- human puts Linear into list-view-grouped-by-status on the CLI team board
-- beforehand; this script must never try to drive Linear's UI.

tell application "Finder" to set screenBounds to bounds of window of desktop
set dw to item 3 of screenBounds
set dh to item 4 of screenBounds
set half to (dw / 2) as integer
set topY to 25 -- clear the menu bar

tell application "Linear" to activate
delay 0.6
tell application "System Events" to tell process "Linear"
    set position of front window to {0, topY}
    set size of front window to {half, (dh - topY)}
end tell

tell application "iTerm2"
    activate
    set bounds of front window to {half, topY, dw, dh}
end tell
