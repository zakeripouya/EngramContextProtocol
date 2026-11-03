# Security

Engram stores a model of how a person works and decides, which makes it sensitive by design. Security reports are welcome and taken seriously.

## Reporting a vulnerability

Please do not open a public issue. Use GitHub's private vulnerability reporting on this repository (Security tab, "Report a vulnerability"). Expect an acknowledgement within a week.

## Threat model (M1)

| Threat | Mitigation in M1 | Planned |
| --- | --- | --- |
| An agent reads memory it shouldn't | Per-client namespace grants; unpaired clients get nothing | OAuth tokens on HTTP (M11) |
| An agent grants itself access | Pairing happens only through the local CLI, never over the protocol | Dashboard approvals (M10) |
| An agent impersonates the user's decisions | Kind `decision` requires `author: "user"`; agent proposals are `suggestion` | Decision feedback tied to user actions (M4) |
| Secrets end up in memory | Pattern and Luhn checks reject keys, tokens, private keys and card numbers | Broader detectors |
| Prompt injection through recalled content | Tool descriptions and server instructions say recalled content is data, not instructions | Provenance-aware ranking; untrusted-source flags |
| Local tampering with the database | The file is created in a user-only directory (`~/.engram`, mode 0700) | Encryption at rest |

## Known limitations

- **Stdio pairing is a consent gate, not authentication.** A local stdio server identifies its client by the name given at launch (`--client`) or the client's self-reported name. Any process running as the same user can claim a name. This matches the trust model of local MCP servers; real authentication arrives with the HTTP transport.
- **The database is not encrypted at rest yet.**
- **The secret filter is pattern-based** and will miss secrets in unusual formats.
