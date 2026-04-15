#!/usr/bin/env python3

import base64
import json
import os
import secrets
import string
import sys
import time

import winrm


def _die(message: str, code: int = 1):
    sys.stdout.write(json.dumps({"status": "error", "message": message}) + "\n")
    sys.exit(code)


def _read_opt_file(path: str) -> str:
    if not path:
        return ""
    try:
        with open(path, "r", encoding="utf-8") as f:
            return f.read().strip("\n")
    except FileNotFoundError:
        return ""


def _read_opt_int_env(name: str):
    v = os.environ.get(name, "")
    if not v:
        return None
    try:
        return int(v)
    except ValueError:
        _die(f"invalid {name}")


def _generate_password(
    length: int,
    min_upper: int,
    min_lower: int,
    min_digits: int,
    min_special: int,
) -> str:
    upper = string.ascii_uppercase
    lower = string.ascii_lowercase
    digits = string.digits
    special = "@#%+=_-"

    if length < 8:
        _die("password length must be >= 8")
    if min_upper < 0 or min_lower < 0 or min_digits < 0 or min_special < 0:
        _die("password policy minimum counts must be >= 0")

    req = min_upper + min_lower + min_digits + min_special
    if req > length:
        _die("password policy minimum counts exceed password length")

    chars = []
    for _ in range(min_upper):
        chars.append(secrets.choice(upper))
    for _ in range(min_lower):
        chars.append(secrets.choice(lower))
    for _ in range(min_digits):
        chars.append(secrets.choice(digits))
    for _ in range(min_special):
        chars.append(secrets.choice(special))

    alphabet = upper + lower + digits + special
    for _ in range(length - len(chars)):
        chars.append(secrets.choice(alphabet))

    secrets.SystemRandom().shuffle(chars)
    return "".join(chars)


def _ps_single_quote(s: str) -> str:
    # PowerShell single-quoted string escaping: '' represents a single '
    return s.replace("'", "''")


def main() -> None:
    target_ip = os.environ.get("TARGET_IP", "")
    target_user = os.environ.get("TARGET_USER", "")
    target_port = int(os.environ.get("TARGET_PORT", "5985"))

    rotation_kind = os.environ.get("ROTATION_KIND", "windows-password")
    rotation_source = os.environ.get("ROTATION_SOURCE", "generate")

    connect_timeout = int(os.environ.get("CONNECT_TIMEOUT_SECONDS", "30"))
    exec_timeout = int(os.environ.get("EXEC_TIMEOUT_SECONDS", "60"))

    tls = os.environ.get("WINRM_TLS", "false").lower() == "true"
    # For MVP over HTTP, skip TLS validation is irrelevant. For HTTPS later, we can expose WINRM_SERVER_CERT_VALIDATION.

    bootstrap_username_file = os.environ.get("BOOTSTRAP_USERNAME_FILE", "")
    bootstrap_password_file = os.environ.get("BOOTSTRAP_PASSWORD_FILE", "")

    if not target_ip:
        _die("missing TARGET_IP")
    if not target_user:
        _die("missing TARGET_USER")

    if rotation_kind != "windows-password":
        _die("unsupported ROTATION_KIND")
    if rotation_source not in ("generate",):
        _die("unsupported ROTATION_SOURCE")

    boot_user = _read_opt_file(bootstrap_username_file) or target_user
    boot_pass = _read_opt_file(bootstrap_password_file)

    if not boot_pass:
        _die("bootstrap password file is missing or unreadable")

    length_env = os.environ.get("PASSWORD_LENGTH", "")

    pp_len = _read_opt_int_env("PASSWORD_POLICY_LENGTH")
    pp_min_len = _read_opt_int_env("PASSWORD_POLICY_MIN_LENGTH")
    pp_max_len = _read_opt_int_env("PASSWORD_POLICY_MAX_LENGTH")
    pp_min_upper = _read_opt_int_env("PASSWORD_POLICY_MIN_UPPER")
    pp_min_lower = _read_opt_int_env("PASSWORD_POLICY_MIN_LOWER")
    pp_min_digits = _read_opt_int_env("PASSWORD_POLICY_MIN_DIGITS")
    pp_min_special = _read_opt_int_env("PASSWORD_POLICY_MIN_SPECIAL")

    policy_present = any(
        x is not None
        for x in (
            pp_len,
            pp_min_len,
            pp_max_len,
            pp_min_upper,
            pp_min_lower,
            pp_min_digits,
            pp_min_special,
        )
    )

    pw_len = 24
    if policy_present:
        if pp_len is not None:
            pw_len = pp_len
        else:
            if pp_min_len is None and pp_max_len is None:
                pw_len = 24
            else:
                if pp_min_len is None:
                    pp_min_len = pp_max_len
                if pp_max_len is None:
                    pp_max_len = pp_min_len
                if pp_min_len is None or pp_max_len is None:
                    _die("invalid password policy length range")
                if pp_min_len > pp_max_len:
                    _die("password policy minLength cannot be greater than maxLength")
                pw_len = pp_min_len + secrets.randbelow(pp_max_len - pp_min_len + 1)
    elif length_env:
        try:
            pw_len = int(length_env)
        except ValueError:
            _die("invalid PASSWORD_LENGTH")

    new_password = _generate_password(
        pw_len,
        pp_min_upper or 0,
        pp_min_lower or 0,
        pp_min_digits or 0,
        pp_min_special or 0,
    )

    # WinRM endpoint
    scheme = "https" if tls else "http"
    endpoint = f"{scheme}://{target_ip}:{target_port}/wsman"

    # pywinrm timeouts are in seconds; operation_timeout_sec affects remote command exec timeout
    # read_timeout_sec should be >= operation_timeout_sec
    operation_timeout = max(10, exec_timeout)
    read_timeout = operation_timeout + 10

    try:
        sess = winrm.Session(
            endpoint,
            auth=(boot_user, boot_pass),
            transport="ntlm",
            server_cert_validation="ignore" if tls else "ignore",
            operation_timeout_sec=operation_timeout,
            read_timeout_sec=read_timeout,
        )
    except Exception as e:
        _die(f"failed to create winrm session: {e}")

    # Change password of TARGET_USER (MVP)
    # Use PowerShell to set password via secure string
    u = _ps_single_quote(target_user)
    p = _ps_single_quote(new_password)
    ps = (
        f"$u='{u}'; $p=ConvertTo-SecureString '{p}' -AsPlainText -Force; "
        f"try {{ Set-LocalUser -Name $u -Password $p; Write-Output 'OK' }} "
        f"catch {{ net user $u '{p}'; if ($LASTEXITCODE -ne 0) {{ throw }}; Write-Output 'OK' }}"
    )

    start = time.time()
    try:
        r = sess.run_ps(ps)
    except Exception as e:
        _die(f"winrm execution error: {e}")
    dur = time.time() - start

    if r.status_code != 0:
        stderr = (r.std_err or b"").decode("utf-8", errors="replace").strip()
        stdout = (r.std_out or b"").decode("utf-8", errors="replace").strip()
        msg = stderr or stdout or "password update failed"
        _die(f"password update failed: {msg}")

    new_b64 = base64.b64encode(new_password.encode("utf-8")).decode("ascii").strip()

    out = {
        "status": "success",
        "message": f"password updated for user {target_user} via winrm in {dur:.2f}s",
        "newPasswordB64": new_b64,
    }
    sys.stdout.write(json.dumps(out) + "\n")


if __name__ == "__main__":
    main()
