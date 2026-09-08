# codeument shell hook for bash. Load with: eval "$(codeument hook bash)"
# Nothing here calls out to the network; it records the command you just ran
# into the local codeument journal via `codeument ingest`.
if [ -n "$BASH_VERSION" ] && [ -z "$__cdm_loaded" ]; then
__cdm_loaded=1
__cdm_bin='[[.Binary]]'
__cdm_notify='[[.NotifyFile]]'
__cdm_seen_dir='[[.SeenDir]]'
if [ -z "$CODEUMENT_SESSION" ]; then
  export CODEUMENT_SESSION="${HOSTNAME:-host}-$$-${EPOCHSECONDS:-$(date +%s)}-$RANDOM"
fi
__cdm_seen="$__cdm_seen_dir/$CODEUMENT_SESSION"
__cdm_at_prompt=1
__cdm_start=
__cdm_last_hist=

__cdm_now() {
  if [ -n "$EPOCHREALTIME" ]; then printf '%s' "$EPOCHREALTIME"; else date +%s; fi
}

__cdm_preexec() {
  [ -n "$COMP_LINE" ] && return
  [ -n "$__cdm_at_prompt" ] || return
  case "$BASH_COMMAND" in __cdm_precmd*) return ;; esac
  __cdm_at_prompt=
  __cdm_start=$(__cdm_now)
}

__cdm_precmd() {
  local __cdm_ec=$?
  if [ -z "$CODEUMENT_OFF" ] && [ -n "$__cdm_start" ]; then
    local __cdm_end __cdm_line __cdm_num __cdm_cmd
    __cdm_end=$(__cdm_now)
    __cdm_line=$(HISTTIMEFORMAT= builtin history 1)
    __cdm_num=${__cdm_line%%[^ 0-9]*}
    __cdm_num=${__cdm_num// /}
    __cdm_cmd=${__cdm_line#*[0-9] }
    __cdm_cmd=${__cdm_cmd# }
    if [ -n "$__cdm_cmd" ] && [ "$__cdm_num" != "$__cdm_last_hist" ]; then
      __cdm_last_hist=$__cdm_num
      printf 'cdm1\0%s\0%s\0%s\0%s\0%s\0bash\0%s\0%s\0' "$__cdm_cmd" "$__cdm_ec" "$__cdm_start" "$__cdm_end" "$PWD" "$CODEUMENT_SESSION" "$$" \
        | "$__cdm_bin" ingest >/dev/null 2>&1 &
      disown $! 2>/dev/null
    fi
  fi
  __cdm_start=
  __cdm_at_prompt=1
  if [ -s "$__cdm_notify" ] && [ ! "$__cdm_seen" -nt "$__cdm_notify" ]; then
    cat "$__cdm_notify"
    { [ -d "$__cdm_seen_dir" ] || mkdir -p "$__cdm_seen_dir"; } 2>/dev/null
    : > "$__cdm_seen" 2>/dev/null
  fi
  return $__cdm_ec
}

__cdm_exit() {
  [ -z "$CODEUMENT_OFF" ] && printf 'cdm1\0\0000\0%s\0%s\0%s\0bash\0%s\0%s\0' "$(__cdm_now)" "$(__cdm_now)" "$PWD" "$CODEUMENT_SESSION" "$$" | "$__cdm_bin" ingest --session-end >/dev/null 2>&1
}

set -o history 2>/dev/null
trap '__cdm_preexec' DEBUG
case "$PROMPT_COMMAND" in
  *__cdm_precmd*) ;;
  *) PROMPT_COMMAND="__cdm_precmd${PROMPT_COMMAND:+;$PROMPT_COMMAND}" ;;
esac
trap '__cdm_exit' EXIT
fi
