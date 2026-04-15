# SSH Executor for Linux VMs (key rotation)
# - Replaces authorized_keys with a new or provided public key
# - Supports auth via private key or password (sshpass)
# - Ed25519 default key generation when source==generate

FROM alpine:3.19

RUN apk add --no-cache \
    openssh-client \
    sshpass \
    coreutils \
    bash \
    jq

WORKDIR /
COPY executor.sh /executor
RUN chmod +x /executor

ENTRYPOINT ["/executor"]
