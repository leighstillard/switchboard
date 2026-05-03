# Jcode Protocol Version

This document tracks the jcode Unix socket protocol version that Switchboard targets.

## Current Target

- **Protocol Version:** TBD (pending jcode `serve` stabilisation)
- **Wire Format:** Newline-delimited JSON over Unix domain socket
- **Framing:** Each message is a JSON object terminated by `\n`

## Compatibility Notes

Switchboard implements its own protocol types in `internal/jcodeproto/` rather than
importing from jcode directly. This allows Switchboard to target a specific protocol
version and handle version negotiation independently.

## Version History

| Date | Version | Notes |
|------|---------|-------|
| TBD  | 0.1     | Initial protocol definition |
