#!/usr/bin/env bash
set -euo pipefail

# Inputs
: "${TARGET_IP:?missing TARGET_IP}"
: "${TARGET_USER:?missing TARGET_USER}"
: "${TARGET_PORT:=22}"
: "${ROTATION_KIND:=ssh-key}"
: "${ROTATION_SOURCE:=generate}"
: "${MODE_REPLACE:=true}"
: "${CONNECT_TIMEOUT_SECONDS:=30}"
: "${EXEC_TIMEOUT_SECONDS:=60}"
: "${BOOTSTRAP_USERNAME_FILE:=}"
: "${BOOTSTRAP_PASSWORD_FILE:=}"
: "${BOOTSTRAP_PRIVATEKEY_FILE:=}"
: "${WORK_DIR:=/work}"
: "${PASSWORD_LENGTH:=}"
: "${PASSWORD_POLICY_LENGTH:=}"
: "${PASSWORD_POLICY_MIN_LENGTH:=}"
: "${PASSWORD_POLICY_MAX_LENGTH:=}"
: "${PASSWORD_POLICY_MIN_UPPER:=}"
: "${PASSWORD_POLICY_MIN_LOWER:=}"
: "${PASSWORD_POLICY_MIN_DIGITS:=}"
: "${PASSWORD_POLICY_MIN_SPECIAL:=}"

mkdir -p "$WORK_DIR" || true
export HOME="$WORK_DIR"

read_opt_file() {
  local f="$1"
  if [[ -n "$f" && -r "$f" ]]; then
    cat "$f"
  fi
}

read_opt_int_env() {
  local name="$1"
  local v="${!name:-}"
  if [[ -z "$v" ]]; then
    echo ""
    return 0
  fi
  if [[ ! "$v" =~ ^[0-9]+$ ]]; then
    echo "{\"status\":\"error\",\"message\":\"invalid ${name}\"}"
    exit 1
  fi
  echo "$v"
}

rand_u32() {
  od -An -N4 -tu4 < /dev/urandom | tr -d ' '
}

rand_range() {
  local min="$1"
  local max="$2"
  local span=$((max - min + 1))
  if (( span <= 0 )); then
    echo "{\"status\":\"error\",\"message\":\"invalid password length range\"}"
    exit 1
  fi
  local r
  r=$(rand_u32)
  echo $((min + (r % span)))
}

pick_char() {
  local set="$1"
  local n=${#set}
  if (( n <= 0 )); then
    echo "{\"status\":\"error\",\"message\":\"empty charset\"}"
    exit 1
  fi
  local r
  r=$(rand_u32)
  local idx=$((r % n))
  printf '%s' "${set:$idx:1}"
}

shuffle_chars() {
  local -n arr_ref=$1
  local i j tmp r
  for ((i=${#arr_ref[@]}-1; i>0; i--)); do
    r=$(rand_u32)
    j=$((r % (i+1)))
    tmp="${arr_ref[$i]}"
    arr_ref[$i]="${arr_ref[$j]}"
    arr_ref[$j]="$tmp"
  done
}

generate_password() {
  local length="$1"
  local min_upper="$2"
  local min_lower="$3"
  local min_digits="$4"
  local min_special="$5"

  if (( length < 8 )); then
    echo "{\"status\":\"error\",\"message\":\"password length must be >= 8\"}"
    exit 1
  fi
  if (( min_upper < 0 || min_lower < 0 || min_digits < 0 || min_special < 0 )); then
    echo "{\"status\":\"error\",\"message\":\"password policy minimum counts must be >= 0\"}"
    exit 1
  fi
  local req=$((min_upper + min_lower + min_digits + min_special))
  if (( req > length )); then
    echo "{\"status\":\"error\",\"message\":\"password policy minimum counts exceed password length\"}"
    exit 1
  fi

  local upper="ABCDEFGHIJKLMNOPQRSTUVWXYZ"
  local lower="abcdefghijklmnopqrstuvwxyz"
  local digits="0123456789"
  local special="@#%+=_-"
  local all="${upper}${lower}${digits}${special}"

  local -a chars=()
  local k
  for ((k=0; k<min_upper; k++)); do chars+=("$(pick_char "$upper")"); done
  for ((k=0; k<min_lower; k++)); do chars+=("$(pick_char "$lower")"); done
  for ((k=0; k<min_digits; k++)); do chars+=("$(pick_char "$digits")"); done
  for ((k=0; k<min_special; k++)); do chars+=("$(pick_char "$special")"); done
  for ((k=${#chars[@]}; k<length; k++)); do chars+=("$(pick_char "$all")"); done

  shuffle_chars chars
  printf '%s' "${chars[*]}" | tr -d ' '
}

BOOT_USER_CONTENT="$(read_opt_file "$BOOTSTRAP_USERNAME_FILE")"
BOOT_PASS_CONTENT="$(read_opt_file "$BOOTSTRAP_PASSWORD_FILE")"
BOOT_KEY_FILE="$BOOTSTRAP_PRIVATEKEY_FILE"

SSH_BASE=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o GlobalKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout="${CONNECT_TIMEOUT_SECONDS}" -p "${TARGET_PORT}")

SSH_CMD=(ssh "${SSH_BASE[@]}" "${TARGET_USER}@${TARGET_IP}")
SSH_CMD_TTY=(ssh -tt "${SSH_BASE[@]}" "${TARGET_USER}@${TARGET_IP}")
SCP_CMD=(scp -q "${SSH_BASE[@]}")

if [[ "$ROTATION_KIND" == "linux-password" && -n "$BOOT_PASS_CONTENT" ]]; then
  SSH_CMD=(sshpass -p "$BOOT_PASS_CONTENT" ssh "${SSH_BASE[@]}" "${TARGET_USER}@${TARGET_IP}")
  SSH_CMD_TTY=(sshpass -p "$BOOT_PASS_CONTENT" ssh -tt "${SSH_BASE[@]}" "${TARGET_USER}@${TARGET_IP}")
  SCP_CMD=(sshpass -p "$BOOT_PASS_CONTENT" scp -q "${SSH_BASE[@]}")
elif [[ -n "$BOOT_KEY_FILE" && -r "$BOOT_KEY_FILE" ]]; then
  SSH_CMD=(ssh -i "$BOOT_KEY_FILE" "${SSH_BASE[@]}" "${TARGET_USER}@${TARGET_IP}")
  SSH_CMD_TTY=(ssh -tt -i "$BOOT_KEY_FILE" "${SSH_BASE[@]}" "${TARGET_USER}@${TARGET_IP}")
  SCP_CMD=(scp -i "$BOOT_KEY_FILE" -q "${SSH_BASE[@]}")
elif [[ -n "$BOOT_PASS_CONTENT" ]]; then
  SSH_CMD=(sshpass -p "$BOOT_PASS_CONTENT" ssh "${SSH_BASE[@]}" "${TARGET_USER}@${TARGET_IP}")
  SSH_CMD_TTY=(sshpass -p "$BOOT_PASS_CONTENT" ssh -tt "${SSH_BASE[@]}" "${TARGET_USER}@${TARGET_IP}")
  SCP_CMD=(sshpass -p "$BOOT_PASS_CONTENT" scp -q "${SSH_BASE[@]}")
fi

# Determine public/private key to install
NEW_PRIV=""
NEW_PUB=""
NEW_PASS=""
if [[ "$ROTATION_KIND" == "ssh-key" ]]; then
  if [[ "$ROTATION_SOURCE" == "generate" ]]; then
    # Generate ed25519 locally (requires writable WORK_DIR)
    KEY_PATH="$WORK_DIR/newkey"
    ssh-keygen -q -t ed25519 -N "" -f "$KEY_PATH" >/dev/null
    NEW_PRIV="$(cat "$KEY_PATH")"
    NEW_PUB="$(cat "$KEY_PATH.pub")"
  else
    echo '{"status":"error","message":"unsupported ROTATION_SOURCE"}'
    exit 1
  fi
elif [[ "$ROTATION_KIND" == "linux-password" ]]; then
  if [[ "$ROTATION_SOURCE" != "generate" ]]; then
    echo '{"status":"error","message":"unsupported ROTATION_SOURCE"}'
    exit 1
  fi

  PP_LEN="$(read_opt_int_env PASSWORD_POLICY_LENGTH)"
  PP_MIN_LEN="$(read_opt_int_env PASSWORD_POLICY_MIN_LENGTH)"
  PP_MAX_LEN="$(read_opt_int_env PASSWORD_POLICY_MAX_LENGTH)"
  PP_MIN_UPPER="$(read_opt_int_env PASSWORD_POLICY_MIN_UPPER)"
  PP_MIN_LOWER="$(read_opt_int_env PASSWORD_POLICY_MIN_LOWER)"
  PP_MIN_DIGITS="$(read_opt_int_env PASSWORD_POLICY_MIN_DIGITS)"
  PP_MIN_SPECIAL="$(read_opt_int_env PASSWORD_POLICY_MIN_SPECIAL)"

  PW_LEN=24
  if [[ -n "$PP_LEN" ]]; then
    PW_LEN="$PP_LEN"
  else
    if [[ -n "$PP_MIN_LEN" || -n "$PP_MAX_LEN" ]]; then
      if [[ -z "$PP_MIN_LEN" ]]; then PP_MIN_LEN="$PP_MAX_LEN"; fi
      if [[ -z "$PP_MAX_LEN" ]]; then PP_MAX_LEN="$PP_MIN_LEN"; fi
      if (( PP_MIN_LEN > PP_MAX_LEN )); then
        echo '{"status":"error","message":"password policy minLength cannot be greater than maxLength"}'
        exit 1
      fi
      PW_LEN="$(rand_range "$PP_MIN_LEN" "$PP_MAX_LEN")"
    elif [[ -n "$PASSWORD_LENGTH" ]]; then
      LEGACY_LEN="$(read_opt_int_env PASSWORD_LENGTH)"
      if [[ -n "$LEGACY_LEN" ]]; then
        PW_LEN="$LEGACY_LEN"
      fi
    fi
  fi

  NEW_PASS="$(generate_password "$PW_LEN" "${PP_MIN_UPPER:-0}" "${PP_MIN_LOWER:-0}" "${PP_MIN_DIGITS:-0}" "${PP_MIN_SPECIAL:-0}")"
else
  echo '{"status":"error","message":"unsupported ROTATION_KIND"}'
  exit 1
fi

if [[ "$ROTATION_KIND" == "ssh-key" ]]; then
  # Prepare remote command: replace or append authorized_keys
  # Escape single quotes in NEW_PUB for safe embedding inside single-quoted remote script
  PUB_ESC=${NEW_PUB//\'/\'"\'"\'}
  REMOTE_CMD='mkdir -p ~/.ssh >/dev/null 2>&1 || true; chmod 700 ~/.ssh 2>/dev/null || true; '
  if [[ "$MODE_REPLACE" == "true" ]]; then
    REMOTE_CMD+="printf '%s\\n' '$PUB_ESC' > ~/.ssh/authorized_keys; chmod 600 ~/.ssh/authorized_keys;"
  else
    REMOTE_CMD+="touch ~/.ssh/authorized_keys; grep -qxF '$PUB_ESC' ~/.ssh/authorized_keys || printf '%s\\n' '$PUB_ESC' >> ~/.ssh/authorized_keys; chmod 600 ~/.ssh/authorized_keys;"
  fi

  REMOTE_CMD_ESC=${REMOTE_CMD//\'/\'"\'"\'}
  REMOTE_FULL_CMD="env -u BASH_ENV -u ENV /bin/bash --noprofile --norc -c '$REMOTE_CMD_ESC'"
  if ! timeout "${EXEC_TIMEOUT_SECONDS}" "${SSH_CMD[@]}" "$REMOTE_FULL_CMD"; then
    echo '{"status":"error","message":"failed to update authorized_keys on target"}'
    exit 1
  fi

  # Output JSON result
  PUB_JSON=$(printf '%s' "$NEW_PUB" | jq -Rs .)
  if [[ -n "$NEW_PRIV" ]]; then
    PRIV_B64_JSON=$(printf '%s\n' "$NEW_PRIV" | base64 | tr -d '\n' | jq -Rs .)
    printf '{"status":"success","message":"authorized_keys updated","newPublicKey":%s,"newPrivateKeyB64":%s}\n' "$PUB_JSON" "$PRIV_B64_JSON"
  else
    printf '{"status":"success","message":"authorized_keys updated","newPublicKey":%s}\n' "$PUB_JSON"
  fi
elif [[ "$ROTATION_KIND" == "linux-password" ]]; then
  REMOTE_CMD='set -euo pipefail; OLD_PASS=""; NEW_PASS=""; '
  REMOTE_CMD+='IFS= read -r OLD_PASS || true; IFS= read -r NEW_PASS || true; '
  REMOTE_CMD+='if [[ -z "$NEW_PASS" ]]; then echo "missing new password" 1>&2; exit 1; fi; '
  REMOTE_CMD+='USERNAME="'"$TARGET_USER"'"; '
  REMOTE_CMD+='IS_ROOT="false"; if [[ "$(id -u)" == "0" ]]; then IS_ROOT="true"; fi; '
  REMOTE_CMD+='if command -v passwd >/dev/null 2>&1; then '
  REMOTE_CMD+='  if [[ "$IS_ROOT" == "true" ]]; then '
  REMOTE_CMD+='    if command -v script >/dev/null 2>&1; then '
  REMOTE_CMD+='      if printf "%s\n%s\n" "$NEW_PASS" "$NEW_PASS" | script -q -e -c "passwd $USERNAME" /dev/null >/dev/null 2>&1; then exit 0; fi; '
  REMOTE_CMD+='    fi; '
  REMOTE_CMD+='    if printf "%s\n%s\n" "$NEW_PASS" "$NEW_PASS" | passwd "$USERNAME" >/dev/null 2>&1; then exit 0; fi; '
  REMOTE_CMD+='  elif [[ -n "$OLD_PASS" ]]; then '
  REMOTE_CMD+='    if command -v script >/dev/null 2>&1; then '
  REMOTE_CMD+='      if printf "%s\n%s\n%s\n" "$OLD_PASS" "$NEW_PASS" "$NEW_PASS" | script -q -e -c "passwd" /dev/null >/dev/null 2>&1; then exit 0; fi; '
  REMOTE_CMD+='    fi; '
  REMOTE_CMD+='    if printf "%s\n%s\n%s\n" "$OLD_PASS" "$NEW_PASS" "$NEW_PASS" | passwd >/dev/null 2>&1; then exit 0; fi; '
  REMOTE_CMD+='  fi; '
  REMOTE_CMD+='fi; '
  REMOTE_CMD+='if command -v sudo >/dev/null 2>&1; then '
  REMOTE_CMD+='  if printf "%s:%s\n" "$USERNAME" "$NEW_PASS" | sudo -n chpasswd >/dev/null 2>&1; then exit 0; fi; '
  REMOTE_CMD+='fi; '
  REMOTE_CMD+='if command -v chpasswd >/dev/null 2>&1; then '
  REMOTE_CMD+='  if printf "%s:%s\n" "$USERNAME" "$NEW_PASS" | chpasswd >/dev/null 2>&1; then exit 0; fi; '
  REMOTE_CMD+='fi; '
  REMOTE_CMD+='if command -v sudo >/dev/null 2>&1 && command -v passwd >/dev/null 2>&1; then '
  REMOTE_CMD+='  if printf "%s\n" "$NEW_PASS" | sudo -n passwd --stdin "$USERNAME" >/dev/null 2>&1; then exit 0; fi; '
  REMOTE_CMD+='fi; '
  REMOTE_CMD+='if command -v passwd >/dev/null 2>&1; then '
  REMOTE_CMD+='  if printf "%s\n" "$NEW_PASS" | passwd --stdin "$USERNAME" >/dev/null 2>&1; then exit 0; fi; '
  REMOTE_CMD+='fi; '
  REMOTE_CMD+='echo "linux password update failed (self-change requires current password; otherwise requires root/sudo for chpasswd)" 1>&2; exit 1;'

  if [[ -z "$BOOT_PASS_CONTENT" ]]; then
    echo '{"status":"error","message":"bootstrap password is required for linux-password rotation"}'
    exit 1
  fi

  REMOTE_CMD_ESC=${REMOTE_CMD//\'/\'"\'"\'}
  REMOTE_FULL_CMD="env -u BASH_ENV -u ENV /bin/bash --noprofile --norc -c '$REMOTE_CMD_ESC'"

  if ! printf '%s\n%s\n' "$BOOT_PASS_CONTENT" "$NEW_PASS" | timeout "${EXEC_TIMEOUT_SECONDS}" "${SSH_CMD[@]}" "$REMOTE_FULL_CMD"; then
    echo '{"status":"error","message":"failed to update password on target"}'
    exit 1
  fi

  VERIFY_SSH=(sshpass -p "$NEW_PASS" ssh "${SSH_BASE[@]}" "${TARGET_USER}@${TARGET_IP}")
  if ! timeout 20 "${VERIFY_SSH[@]}" "true" >/dev/null 2>&1; then
    echo '{"status":"error","message":"password update verification failed (new password does not work for SSH login)"}'
    exit 1
  fi

  PASS_B64_JSON=$(printf '%s' "$NEW_PASS" | base64 | tr -d '\n' | jq -Rs .)
  printf '{"status":"success","message":"password updated","newPasswordB64":%s}\n' "$PASS_B64_JSON"
fi
