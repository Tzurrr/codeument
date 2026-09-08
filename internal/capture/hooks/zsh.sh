# codeument shell hook for zsh. Load with: eval "$(codeument hook zsh)"
# Nothing here calls out to the network; it records the command you just ran
# into the local codeument journal via `codeument ingest`.
if [ -n "$ZSH_VERSION" ] && [ -z "$__cdm_loaded" ]; then
__cdm_loaded=1
__cdm_bin='[[.Binary]]'
__cdm_notify='[[.NotifyFile]]'
__cdm_seen_dir='[[.SeenDir]]'
zmodload zsh/datetime 2>/dev/null
autoload -Uz add-zsh-hook
if [ -z "$CODEUMENT_SESSION" ]; then
  export CODEUMENT_SESSION="${HOST:-host}-$$-${EPOCHSECONDS:-$(date +%s)}-$RANDOM"
fi
__cdm_seen="$__cdm_seen_dir/$CODEUMENT_SESSION"
__cdm_start=
__cdm_cmd=

__cdm_now() {
  if [ -n "$EPOCHREALTIME" ]; then printf '%s' "$EPOCHREALTIME"; else date +%s; fi
}

__cdm_preexec() {
  __cdm_cmd=$1
  __cdm_start=$(__cdm_now)
}

__cdm_precmd() {
  local __cdm_ec=$?
  if [ -z "$CODEUMENT_OFF" ] && [ -n "$__cdm_start" ] && [ -n "$__cdm_cmd" ]; then
    printf 'cdm1\0%s\0%s\0%s\0%s\0%s\0zsh\0%s\0%s\0' "$__cdm_cmd" "$__cdm_ec" "$__cdm_start" "$(__cdm_now)" "$PWD" "$CODEUMENT_SESSION" "$$" \
      | "$__cdm_bin" ingest >/dev/null 2>&1 &!
  fi
  __cdm_start=
  __cdm_cmd=
  if [ -s "$__cdm_notify" ] && [ ! "$__cdm_seen" -nt "$__cdm_notify" ]; then
    cat "$__cdm_notify"
    { [ -d "$__cdm_seen_dir" ] || mkdir -p "$__cdm_seen_dir"; } 2>/dev/null
    : > "$__cdm_seen" 2>/dev/null
  fi
  return $__cdm_ec
}

__cdm_exit() {
  [ -z "$CODEUMENT_OFF" ] && printf 'cdm1\0\0000\0%s\0%s\0%s\0zsh\0%s\0%s\0' "$(__cdm_now)" "$(__cdm_now)" "$PWD" "$CODEUMENT_SESSION" "$$" | "$__cdm_bin" ingest --session-end >/dev/null 2>&1
}

add-zsh-hook preexec __cdm_preexec
add-zsh-hook precmd __cdm_precmd
add-zsh-hook zshexit __cdm_exit
fi
