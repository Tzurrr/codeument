# codeument shell hook for fish. Load with: codeument hook fish | source
# Nothing here calls out to the network; it records the command you just ran
# into the local codeument journal via `codeument ingest`.
if not set -q __cdm_loaded
set -g __cdm_loaded 1
set -g __cdm_bin '[[.Binary]]'
set -g __cdm_notify '[[.NotifyFile]]'
set -g __cdm_seen_dir '[[.SeenDir]]'
if not set -q CODEUMENT_SESSION
  set -gx CODEUMENT_SESSION (hostname)"-$fish_pid-"(date +%s)"-"(random)
end
set -g __cdm_seen "$__cdm_seen_dir/$CODEUMENT_SESSION"
set -g __cdm_start ''

function __cdm_now
  date +%s.%N 2>/dev/null; or date +%s
end

function __cdm_preexec --on-event fish_preexec
  set -g __cdm_start (__cdm_now)
end

function __cdm_postexec --on-event fish_postexec
  set -l ec $status
  if not set -q CODEUMENT_OFF; and test -n "$__cdm_start"
    printf 'cdm1\0%s\0%s\0%s\0%s\0%s\0fish\0%s\0%s\0' "$argv[1]" "$ec" "$__cdm_start" (__cdm_now) "$PWD" "$CODEUMENT_SESSION" "$fish_pid" \
      | "$__cdm_bin" ingest >/dev/null 2>&1 &
    disown 2>/dev/null
  end
  set -g __cdm_start ''
end

function __cdm_prompt --on-event fish_prompt
  if test -s "$__cdm_notify"; and not test "$__cdm_seen" -nt "$__cdm_notify"
    cat "$__cdm_notify"
    mkdir -p "$__cdm_seen_dir" 2>/dev/null
    printf '' > "$__cdm_seen" 2>/dev/null
  end
end

function __cdm_exit --on-event fish_exit
  if not set -q CODEUMENT_OFF
    printf 'cdm1\0\0000\0%s\0%s\0%s\0fish\0%s\0%s\0' (__cdm_now) (__cdm_now) "$PWD" "$CODEUMENT_SESSION" "$fish_pid" | "$__cdm_bin" ingest --session-end >/dev/null 2>&1
  end
end
end
