# WinRM Executor for Windows VMs (password rotation)
# - Connects via WinRM (NTLM) and updates local user password
# - Outputs JSON to stdout including newPasswordB64 (base64-encoded)

FROM python:3.12-slim

RUN pip install --no-cache-dir pywinrm requests-ntlm

RUN useradd -u 1001 -g 0 -M -d /tmp -s /usr/sbin/nologin executor

WORKDIR /tmp
COPY executor.py /executor
RUN chmod +x /executor

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    HOME=/tmp

USER 1001

ENTRYPOINT ["/executor"]
