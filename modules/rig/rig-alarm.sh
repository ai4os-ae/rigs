# shellcheck shell=bash
# rig-alarm — beep while a laptop rig is unplugged or has its lid shut.
#
# Both states are something a person did to the machine while standing at it,
# and both are invisible from everywhere else: a rig on battery looks perfectly
# healthy over SSH right up until it doesn't, and a closed lid takes away the
# kiosk screen that says not to touch it. The answer to both is the same — make
# a noise next to the person who is still in the room.
#
# The loop polls. There is no single event source that covers mains and lid,
# and a poll that misses a transition catches it a few seconds later, which for
# an alarm that repeats for minutes is not a difference anybody can hear.
#
# The beeping stops on its own after RIG_ALARM_TIMEOUT, because an alarm that
# never gives up stops being a signal: the room learns to ignore it, and the
# rig is left making a noise at nobody all weekend. Five minutes is long enough
# to fetch whoever unplugged it. The rig keeps watching either way — a change
# in what is wrong, or the fault clearing and coming back, starts a fresh five
# minutes.
#
# RIG_ALARM_DRY_RUN=1 prints what the current state would do and exits, which
# is the way to check the sensors on a rig without waiting for a beep.
set -euo pipefail

SYSFS="${RIG_ALARM_SYSFS:-/sys/class/power_supply}"
# Path of the tone to play. Defaulted by the Nix wrapper to the WAV built
# alongside it; overridable so a different sound can be tried in place.
TONE="${RIG_ALARM_TONE:?}"
# Seconds between beeps while alarming, and between checks while quiet.
INTERVAL="${RIG_ALARM_INTERVAL:-5}"
POLL="${RIG_ALARM_POLL:-5}"
# How long one episode of beeping lasts before it falls silent. 0 never stops.
TIMEOUT="${RIG_ALARM_TIMEOUT:-300}"
VOLUME="${RIG_ALARM_VOLUME:-85}"
# Tried in order, all of them, because which one carries the built-in speaker
# depends on the codec: on Intel HDA it is usually Master plus Speaker, and on
# machines that expose neither it is PCM.
CONTROLS="${RIG_ALARM_CONTROLS:-Master Speaker PCM Headphone}"
DRY_RUN="${RIG_ALARM_DRY_RUN:-}"

on_battery() {
  local supply kind online status mains=0

  for supply in "$SYSFS"/*; do
    kind="$(cat "$supply/type" 2>/dev/null || true)"
    # Only Mains counts as being plugged in. USB power supplies are not a
    # second opinion on the same question: a USB-C dock or a charging phone
    # shows up as type USB with online=1 on a laptop that is, at that moment,
    # discharging.
    [ "$kind" = "Mains" ] || continue
    online="$(cat "$supply/online" 2>/dev/null || true)"
    case "$online" in
      1) return 1 ;;
      0) mains=1 ;;
    esac
  done

  if [ "$mains" -eq 1 ]; then
    return 0
  fi

  # No mains supply to ask — some laptops expose only the battery. Discharging
  # is then the same fact arrived at from the other side.
  for supply in "$SYSFS"/BAT*; do
    status="$(cat "$supply/status" 2>/dev/null || true)"
    if [ "$status" = "Discharging" ]; then
      return 0
    fi
  done

  return 1
}

lid_closed() {
  # logind rather than /proc/acpi/button/lid/*/state: logind takes the lid from
  # whichever input device reports SW_LID, which is the only source on the
  # laptops that expose no ACPI lid button at all. The property prints as
  # "b true" / "b false"; anything else (no logind, denied, no lid) is not a
  # closed lid and must not sound the alarm.
  local state
  state="$(busctl --no-pager get-property \
    org.freedesktop.login1 /org/freedesktop/login1 \
    org.freedesktop.login1.Manager LidClosed 2>/dev/null || true)"
  [ "$state" = "b true" ]
}

beep() {
  local ctl controls
  read -ra controls <<< "$CONTROLS"

  # Nothing else on a rig owns the mixer, so the alarm simply takes it: a rig
  # left muted is a rig whose alarm nobody hears, and there is no session here
  # whose volume setting we would be trampling on. Best-effort per control —
  # a card that has no Speaker is not a reason to skip the beep.
  for ctl in "${controls[@]}"; do
    amixer -q -M sset "$ctl" "$VOLUME%" unmute 2>/dev/null || true
  done

  # Never let a busy or missing sound card kill a service whose whole job is to
  # still be running when the thing it watches for finally happens.
  aplay -q "$TONE" 2>/dev/null || true
}

main() {
  # `since` is when the current episode started, on bash's own clock; `spent`
  # marks that this episode has already been reported as timed out, so the
  # journal gets one line about it rather than one every POLL seconds.
  local why prev="" since=0 spent=0

  while true; do
    why=""
    if on_battery; then
      why="unplugged"
    fi
    if lid_closed; then
      why="${why:+$why and }lid closed"
    fi

    if [ -n "$DRY_RUN" ]; then
      printf '%s\n' "${why:-ok}"
      exit 0
    fi

    if [ -n "$why" ]; then
      # Only on the transition: this loop runs until somebody fixes the thing
      # it is complaining about, and a line every five seconds until then would
      # bury whatever else the journal has to say.
      #
      # A different reason is a different episode — closing the lid on a rig
      # that has already beeped its five minutes for being unplugged is new
      # news, and gets its own five minutes.
      if [ "$why" != "$prev" ]; then
        echo "rig-alarm: $why"
        prev="$why"
        since=$SECONDS
        spent=0
      fi

      if [ "$TIMEOUT" -eq 0 ] || [ $((SECONDS - since)) -lt "$TIMEOUT" ]; then
        beep
        sleep "$INTERVAL"
      else
        if [ "$spent" -eq 0 ]; then
          echo "rig-alarm: still $why after ${TIMEOUT}s; going quiet"
          spent=1
        fi
        sleep "$POLL"
      fi
    else
      if [ -n "$prev" ]; then
        echo "rig-alarm: back on mains with the lid open"
      fi
      prev=""
      spent=0
      sleep "$POLL"
    fi
  done
}

main "$@"
