# Optional convenience image — the primary artifact is the static binary.
# goreleaser injects the prebuilt binary; this stays FROM scratch: no shell,
# no package manager, nothing to CVE-scan but the binary itself.
FROM scratch
COPY pikopod /pikopod
USER 65532:65532
ENTRYPOINT ["/pikopod"]
