package session

import "context"

// fitWindowScript waits for Chromium's main window to fill the display and, if it has not done
// so after about three seconds, resizes it. Chromium maps its first window at a default size and
// maximizes it a moment later, so a client that measured the page right after create could
// otherwise see a window that does not fill the screen. Only real browser windows (larger than
// 300x200) are touched.
const fitWindowScript = `
export DISPLAY=:99.0
eval "$(xdotool getdisplaygeometry | awk '{print "SW="$1" SH="$2}')"
[ -n "$SW" ] || exit 0
i=0
while [ $i -lt 40 ]; do
  all=1; any=0
  for id in $(xdotool search --onlyvisible --class chromium 2>/dev/null); do
    eval "$(xdotool getwindowgeometry --shell $id 2>/dev/null)"
    [ "${WIDTH:-0}" -gt 300 ] && [ "${HEIGHT:-0}" -gt 200 ] || continue
    any=1
    if [ "$WIDTH" != "$SW" ] || [ "$HEIGHT" != "$SH" ]; then
      all=0
      [ $i -ge 30 ] && xdotool windowmove $id 0 0 windowsize $id $SW $SH 2>/dev/null
    fi
  done
  [ $any = 1 ] && [ $all = 1 ] && exit 0
  sleep 0.1; i=$((i+1))
done
exit 0
`

// fitWindow is best effort: a failure here never fails a create.
func (m *Manager) fitWindow(ctx context.Context, cid string) {
	_, _, _ = m.run(ctx, cid, "neko", "sh", "-c", fitWindowScript)
}
